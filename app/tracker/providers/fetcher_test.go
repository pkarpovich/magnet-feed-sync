package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubFetcher struct {
	body []byte
	err  error
	urls []string
}

func (s *stubFetcher) Fetch(ctx context.Context, url string) ([]byte, error) {
	s.urls = append(s.urls, url)
	return s.body, s.err
}

func TestDirectFetcherClassification(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		timeout time.Duration
		want    ErrorKind
	}{
		{
			name: "blocked_403",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			},
			want: KindBlocked,
		},
		{
			name: "blocked_429",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
			},
			want: KindBlocked,
		},
		{
			name: "blocked_cf_body",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte("<html><title>Just a moment...</title></html>"))
			},
			want: KindBlocked,
		},
		{
			name: "blocked_cf_header",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("cf-mitigated", "challenge")
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte("<html><body>ok</body></html>"))
			},
			want: KindBlocked,
		},
		{
			name: "transient_500",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			want: KindTransient,
		},
		{
			name: "permanent_404",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
			want: KindPermanent,
		},
		{
			name: "transient_timeout",
			handler: func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(200 * time.Millisecond)
			},
			timeout: 20 * time.Millisecond,
			want:    KindTransient,
		},
		{
			name: "transient_other_status",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTeapot)
			},
			want: KindTransient,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()

			ctx := context.Background()
			if tt.timeout > 0 {
				timeoutCtx, cancel := context.WithTimeout(ctx, tt.timeout)
				defer cancel()
				ctx = timeoutCtx
			}

			body, err := NewDirectFetcher().Fetch(ctx, server.URL)
			require.Error(t, err)
			assert.Nil(t, body)

			var pe *ProviderError
			require.True(t, errors.As(err, &pe))
			assert.Equal(t, tt.want, pe.Kind)
		})
	}
}

func TestDirectFetcherSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body>hello</body></html>"))
	}))
	defer server.Close()

	body, err := NewDirectFetcher().Fetch(context.Background(), server.URL)
	require.NoError(t, err)
	assert.Contains(t, string(body), "hello")
}

func TestDirectFetcherSendsUserAgent(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html></html>"))
	}))
	defer server.Close()

	_, err := NewDirectFetcher().Fetch(context.Background(), server.URL)
	require.NoError(t, err)
	assert.Equal(t, directUserAgent, got)
}
