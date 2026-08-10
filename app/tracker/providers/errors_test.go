package providers

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestErrorKindString(t *testing.T) {
	tests := []struct {
		name string
		kind ErrorKind
		want string
	}{
		{name: "transient", kind: KindTransient, want: "Transient"},
		{name: "blocked", kind: KindBlocked, want: "Blocked"},
		{name: "permanent", kind: KindPermanent, want: "Permanent"},
		{name: "unknown_falls_back_to_transient", kind: ErrorKind(42), want: "Transient"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.kind.String())
		})
	}
}

func TestProviderErrorTextAndUnwrap(t *testing.T) {
	inner := errors.New("flaresolverr not configured")
	err := &ProviderError{Kind: KindBlocked, Err: inner}

	assert.Equal(t, "Blocked: flaresolverr not configured", err.Error())
	assert.ErrorIs(t, err, inner)

	wrapped := fmt.Errorf("failed to fetch rutracker page: %w", err)
	var pe *ProviderError
	require.True(t, errors.As(wrapped, &pe))
	assert.Equal(t, KindBlocked, pe.Kind)
}

func TestProviderExtractionFailureIsPermanent(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		provider func(f Fetcher) Provider
	}{
		{
			name: "rutracker_without_magnet",
			body: `<html><body><a id="topic-title">Some Title</a></body></html>`,
			provider: func(f Fetcher) Provider {
				return NewRutrackerProvider(f)
			},
		},
		{
			name: "nnm_without_magnet",
			body: `<html><body><a class="maintitle">Some Title</a></body></html>`,
			provider: func(f Fetcher) Provider {
				return NewNnmProvider(f)
			},
		},
		{
			name: "jackett_without_items",
			body: `<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel></channel></rss>`,
			provider: func(f Fetcher) Provider {
				return NewJackettProvider("http://jackett:9117", f)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := tt.provider(&stubFetcher{body: []byte(tt.body)})

			result, err := provider.Parse(context.Background(), "http://tracker.local/page?t=1")
			require.Error(t, err)
			assert.Nil(t, result)

			var pe *ProviderError
			require.True(t, errors.As(err, &pe))
			assert.Equal(t, KindPermanent, pe.Kind)
		})
	}
}

func TestProviderNames(t *testing.T) {
	fetcher := &stubFetcher{}

	assert.Equal(t, "rutracker", NewRutrackerProvider(fetcher).Name())
	assert.Equal(t, "nnm", NewNnmProvider(fetcher).Name())
	assert.Equal(t, "jackett", NewJackettProvider("http://jackett:9117", fetcher).Name())
}
