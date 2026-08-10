package providers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/html/charset"
)

// Fetcher retrieves the raw bytes of a tracker page.
type Fetcher interface {
	Fetch(ctx context.Context, url string) ([]byte, error)
}

const (
	directUserAgent   = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"
	challengeMarker   = "Just a moment..."
	maxResponseSize   = 10 * 1024 * 1024
	directHTTPTimeout = 30 * time.Second
)

type directFetcher struct {
	client *http.Client
}

// NewDirectFetcher returns a Fetcher issuing plain HTTP requests to the tracker.
func NewDirectFetcher() Fetcher {
	// the cron sweep passes a context without deadline, so the bound has to live on the client
	return &directFetcher{client: &http.Client{Timeout: directHTTPTimeout}}
}

func (f *directFetcher) Fetch(ctx context.Context, pageURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, &ProviderError{Kind: KindPermanent, Err: fmt.Errorf("build request: %w", err)}
	}
	req.Header.Set("User-Agent", directUserAgent)

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, &ProviderError{Kind: KindTransient, Err: fmt.Errorf("do request: %w", withoutURL(err))}
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Error("error closing response body", "error", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		kind := classifyStatus(resp.StatusCode)
		return nil, &ProviderError{Kind: kind, Err: fmt.Errorf("bad status: %s", resp.Status)}
	}

	body, err := f.readBody(resp)
	if err != nil {
		return nil, &ProviderError{Kind: KindTransient, Err: err}
	}

	if resp.Header.Get("cf-mitigated") != "" || bytes.Contains(body, []byte(challengeMarker)) {
		return nil, &ProviderError{Kind: KindBlocked, Err: errors.New("cloudflare challenge")}
	}

	return body, nil
}

// withoutURL drops the *url.Error wrapper, whose message embeds the whole request url —
// for jackett that carries the api key, and this error reaches the logs and loki.
func withoutURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}

	return err
}

// classifyStatus maps a tracker HTTP status onto an ErrorKind; shared by the direct
// fetcher and the solver, which sees the same statuses through solution.status.
func classifyStatus(code int) ErrorKind {
	switch code {
	case http.StatusForbidden, http.StatusTooManyRequests:
		return KindBlocked
	case http.StatusNotFound:
		return KindPermanent
	default:
		return KindTransient
	}
}

func (f *directFetcher) readBody(resp *http.Response) ([]byte, error) {
	utf8Reader, err := charset.NewReader(resp.Body, resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, fmt.Errorf("decode charset: %w", err)
	}

	body, err := io.ReadAll(io.LimitReader(utf8Reader, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	return body, nil
}
