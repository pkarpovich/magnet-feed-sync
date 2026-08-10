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
	"sync"
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
	baseURL   string
	client    *http.Client
	mu        sync.Mutex
	sessionID string
}

// NewSolverFetcher returns a Fetcher routing requests through the FlareSolverr
// command endpoint at baseURL (the full url including the /v1 path).
func NewSolverFetcher(baseURL string) *solverFetcher {
	return &solverFetcher{
		baseURL: baseURL,
		client:  &http.Client{Timeout: solverHTTPTimeout},
	}
}

func (f *solverFetcher) Fetch(ctx context.Context, pageURL string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

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
		return nil, err
	}

	if resp.Solution == nil {
		return nil, &ProviderError{Kind: KindBlocked, Err: errors.New("flaresolverr returned no solution")}
	}

	return []byte(resp.Solution.Response), nil
}

// Close destroys the FlareSolverr session so the remote browser is released.
func (f *solverFetcher) Close(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

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

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, &ProviderError{Kind: KindBlocked, Err: fmt.Errorf("call solver: %w", err)}
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Error("error closing solver response body", "error", err)
		}
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, &ProviderError{Kind: KindBlocked, Err: fmt.Errorf("read solver response: %w", err)}
	}

	var decoded solverResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, &ProviderError{Kind: KindBlocked, Err: fmt.Errorf("decode solver response: %w", err)}
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
