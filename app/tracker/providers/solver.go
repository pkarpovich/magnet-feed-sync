package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

const (
	solverHTTPTimeout = 180 * time.Second
	solverMaxTimeout  = 120000
)

type solverRequest struct {
	Cmd        string `json:"cmd"`
	URL        string `json:"url,omitempty"`
	Session    string `json:"session,omitempty"`
	MaxTimeout int    `json:"maxTimeout,omitempty"`
}

type solverSolution struct {
	Status   int    `json:"status"`
	Response string `json:"response"`
}

type solverResponse struct {
	Status   string          `json:"status"`
	Message  string          `json:"message"`
	Solution *solverSolution `json:"solution"`
}

type solverFetcher struct {
	baseURL string
	client  *http.Client
	// sem serialises solver calls and guards sessionID; a channel rather than a mutex so
	// a caller can give up when its context ends instead of waiting out a 180s solve
	sem       chan struct{}
	sessionID string
}

// NewSolverFetcher returns a Fetcher routing requests through the FlareSolverr
// command endpoint at baseURL (the full url including the /v1 path).
func NewSolverFetcher(baseURL string) *solverFetcher {
	return &solverFetcher{
		baseURL: baseURL,
		client:  &http.Client{Timeout: solverHTTPTimeout},
		sem:     make(chan struct{}, 1),
	}
}

func (f *solverFetcher) acquire(ctx context.Context) error {
	// checked up front so an already dead context never wins a race for a free slot
	if err := ctx.Err(); err != nil {
		return err
	}

	select {
	case f.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *solverFetcher) release() {
	<-f.sem
}

func (f *solverFetcher) Fetch(ctx context.Context, pageURL string) ([]byte, error) {
	if err := f.acquire(ctx); err != nil {
		return nil, &ProviderError{Kind: KindTransient, Err: fmt.Errorf("wait for solver: %w", err)}
	}
	defer f.release()

	if err := f.ensureSession(ctx); err != nil {
		return nil, err
	}

	resp, err := f.command(ctx, solverRequest{
		Cmd:        "request.get",
		URL:        pageURL,
		Session:    f.sessionID,
		MaxTimeout: solverMaxTimeout,
	})
	if err != nil {
		// flaresolverr forgets every session when it restarts and then rejects this id
		// forever; drop it so the next fetch creates a fresh one instead of failing for good
		if ctx.Err() == nil {
			f.sessionID = ""
		}
		return nil, err
	}

	if resp.Solution == nil {
		return nil, &ProviderError{Kind: KindBlocked, Err: errors.New("flaresolverr returned no solution")}
	}

	// flaresolverr reports the command as ok even when the tracker refused it, so the
	// tracker's own status has to be classified here or a 403 reaches the parser as html
	if status := resp.Solution.Status; status != 0 && status != http.StatusOK {
		return nil, &ProviderError{
			Kind: classifyStatus(status),
			Err:  fmt.Errorf("bad status: %d %s", status, http.StatusText(status)),
		}
	}

	body := []byte(resp.Solution.Response)
	if bytes.Contains(body, []byte(challengeMarker)) {
		return nil, &ProviderError{Kind: KindBlocked, Err: errors.New("cloudflare challenge")}
	}

	return body, nil
}

// Close destroys the FlareSolverr session so the remote browser is released. It gives
// up when ctx ends rather than waiting for an in-flight fetch to finish.
func (f *solverFetcher) Close(ctx context.Context) error {
	if err := f.acquire(ctx); err != nil {
		return fmt.Errorf("wait for solver: %w", err)
	}
	defer f.release()

	if f.sessionID == "" {
		return nil
	}

	if _, err := f.command(ctx, solverRequest{Cmd: "sessions.destroy", Session: f.sessionID}); err != nil {
		return err
	}
	f.sessionID = ""

	return nil
}

func (f *solverFetcher) ensureSession(ctx context.Context) error {
	if f.sessionID != "" {
		return nil
	}

	id := "magnet-feed-sync-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := f.command(ctx, solverRequest{Cmd: "sessions.create", Session: id}); err != nil {
		return err
	}
	f.sessionID = id
	slog.Info("flaresolverr session created", "session", id)

	return nil
}

func (f *solverFetcher) command(ctx context.Context, cmd solverRequest) (*solverResponse, error) {
	payload, err := json.Marshal(cmd)
	if err != nil {
		return nil, &ProviderError{Kind: KindPermanent, Err: fmt.Errorf("encode solver request: %w", err)}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.baseURL, bytes.NewReader(payload))
	if err != nil {
		return nil, &ProviderError{Kind: KindPermanent, Err: fmt.Errorf("build solver request: %w", err)}
	}
	req.Header.Set("Content-Type", "application/json")

	// reaching, reading and decoding the solver are local infrastructure concerns: they say
	// nothing about the tracker, so they must stay transient and leave the breaker closed
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, &ProviderError{Kind: KindTransient, Err: fmt.Errorf("call solver: %w", err)}
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Error("error closing solver response body", "error", err)
		}
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, &ProviderError{Kind: KindTransient, Err: fmt.Errorf("read solver response: %w", err)}
	}

	var decoded solverResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, &ProviderError{Kind: KindTransient, Err: fmt.Errorf("decode solver response: %w", err)}
	}

	if resp.StatusCode != http.StatusOK || decoded.Status != "ok" {
		return nil, &ProviderError{Kind: KindBlocked, Err: fmt.Errorf("solver %s failed: %s", cmd.Cmd, decoded.Message)}
	}

	return &decoded, nil
}

type blockedFetcher struct{}

// NewBlockedFetcher returns a Fetcher reporting every page as blocked, used when
// no solver is configured for a tracker that needs one.
func NewBlockedFetcher() Fetcher {
	return &blockedFetcher{}
}

func (f *blockedFetcher) Fetch(ctx context.Context, pageURL string) ([]byte, error) {
	return nil, &ProviderError{Kind: KindBlocked, Err: errors.New("flaresolverr not configured")}
}
