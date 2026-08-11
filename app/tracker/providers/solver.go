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
	"strings"
	"time"
)

const (
	solverHTTPTimeout = 180 * time.Second
	solverMaxTimeout  = 120000
	cmdRequestGet     = "request.get"
)

type solverRequest struct {
	Cmd        string `json:"cmd"`
	URL        string `json:"url,omitempty"`
	Session    string `json:"session,omitempty"`
	MaxTimeout int    `json:"maxTimeout,omitempty"`
}

type solverSolution struct {
	Status    int            `json:"status"`
	Response  string         `json:"response"`
	Cookies   []solverCookie `json:"cookies"`
	UserAgent string         `json:"userAgent"`
}

// solverCookie mirrors the cookie shape flaresolverr emits. net/http.Cookie cannot be
// unmarshalled directly: its Expires is a time.Time and the solver sends a unix number.
type solverCookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
	Path   string `json:"path"`
}

// SolvedPage is a page fetched through FlareSolverr together with the browser state that
// produced it. The cookie and the User-Agent belong together — presenting one without the
// other re-triggers the challenge immediately.
type SolvedPage struct {
	Body      []byte
	Cookies   []*http.Cookie
	UserAgent string
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

	page, err := f.request(ctx, pageURL)
	if err != nil {
		return nil, err
	}

	if bytes.Contains(page.Body, []byte(challengeMarker)) {
		return nil, &ProviderError{Kind: KindBlocked, Err: errors.New("cloudflare challenge")}
	}

	return page.Body, nil
}

// Solve fetches pageURL and returns the cookies and User-Agent alongside the body, for a
// caller that continues the session with its own http client. Unlike Fetch it does not
// judge the body: such a caller owns its own challenge detection.
func (f *solverFetcher) Solve(ctx context.Context, pageURL string) (*SolvedPage, error) {
	if err := f.acquire(ctx); err != nil {
		return nil, &ProviderError{Kind: KindTransient, Err: fmt.Errorf("wait for solver: %w", err)}
	}
	defer f.release()

	return f.request(ctx, pageURL)
}

func (f *solverFetcher) request(ctx context.Context, pageURL string) (*SolvedPage, error) {
	if err := f.ensureSession(ctx); err != nil {
		return nil, err
	}

	resp, err := f.command(ctx, solverRequest{
		Cmd:        cmdRequestGet,
		URL:        pageURL,
		Session:    f.sessionID,
		MaxTimeout: solverMaxTimeout,
	})
	if err != nil {
		// flaresolverr forgets every session when it restarts and then rejects this id
		// forever; drop it so the next fetch creates a fresh one instead of failing for good.
		// Every other failure leaves the session alive on the solver, so the id is kept:
		// clearing it would orphan a browser there and cost a ~74s cold solve on the next fetch
		if errors.Is(err, errSessionGone) {
			f.sessionID = ""
		}
		return nil, err
	}

	// a response shaped unlike what this code expects is a solver-version problem, not a
	// refusal by the tracker, so it must not trip the breaker
	if resp.Solution == nil {
		return nil, &ProviderError{Kind: KindTransient, Err: errors.New("flaresolverr returned no solution")}
	}

	// flaresolverr reports the command as ok even when the tracker refused it, so the
	// tracker's own status has to be classified here or a 403 reaches the parser as html
	if status := resp.Solution.Status; status != 0 && status != http.StatusOK {
		return nil, &ProviderError{
			Kind: classifyStatus(status),
			Err:  fmt.Errorf("bad status: %d %s", status, http.StatusText(status)),
		}
	}

	return &SolvedPage{
		Body:      []byte(resp.Solution.Response),
		Cookies:   solvedCookies(resp.Solution.Cookies),
		UserAgent: resp.Solution.UserAgent,
	}, nil
}

func solvedCookies(cookies []solverCookie) []*http.Cookie {
	out := make([]*http.Cookie, 0, len(cookies))
	for _, c := range cookies {
		out = append(out, &http.Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path})
	}

	return out
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
		failure := fmt.Errorf("solver %s failed: %s", cmd.Cmd, decoded.Message)

		// a lost session and a refused session command are both solver-side infrastructure and
		// say nothing about the tracker; only a refused page fetch may trip the breaker
		if sessionGone(decoded.Message) {
			return nil, &ProviderError{Kind: KindTransient, Err: fmt.Errorf("%w: %w", errSessionGone, failure)}
		}
		if cmd.Cmd != cmdRequestGet {
			return nil, &ProviderError{Kind: KindTransient, Err: failure}
		}

		// even on request.get most failures are the solver's own (browser start-up, dns inside
		// its container, internal timeouts) and would trip the breaker for 24h over a container
		// hiccup; only a challenge it could not get past says the tracker refused us
		if !challengeFailure(decoded.Message) {
			return nil, &ProviderError{Kind: KindTransient, Err: failure}
		}

		return nil, &ProviderError{Kind: KindBlocked, Err: failure}
	}

	return &decoded, nil
}

// errSessionGone marks the one failure that invalidates the held session id: flaresolverr
// answering a command with a session it no longer knows, which happens after it restarts.
var errSessionGone = errors.New("flaresolverr session is gone")

func sessionGone(message string) bool {
	return strings.Contains(strings.ToLower(message), "session does not exist")
}

// challengeMarkers are what flaresolverr reports when the tracker's protection is what
// stopped it, as opposed to its own browser or network failing.
var challengeMarkers = []string{"challenge", "cloudflare", "captcha"}

func challengeFailure(message string) bool {
	lower := strings.ToLower(message)
	for _, marker := range challengeMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}

	return false
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
