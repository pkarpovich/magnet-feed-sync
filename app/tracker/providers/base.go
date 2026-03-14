package providers

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"golang.org/x/net/html/charset"
)

type Provider interface {
	Parse(ctx context.Context, url string) (*Result, error)
	CanHandle(url string) bool
}

type Result struct {
	ID         string
	Title      string
	Magnet     string
	UpdatedAt  time.Time
	Comment    string
	TrackerURL string
}

func fetchPage(ctx context.Context, pageURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("[ERROR] error closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status: %s", resp.Status)
	}

	utf8Reader, err := charset.NewReader(resp.Body, resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, err
	}

	const maxResponseSize = 10 * 1024 * 1024
	body, err := io.ReadAll(io.LimitReader(utf8Reader, maxResponseSize))
	if err != nil {
		return nil, err
	}

	return body, nil
}
