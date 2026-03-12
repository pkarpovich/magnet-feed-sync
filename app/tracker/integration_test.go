package tracker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"magnet-feed-sync/app/tracker/providers"
)

type recordingProvider struct {
	canHandleURLs []string
	parseCalls    []string
	result        *providers.Result
	err           error
}

func (p *recordingProvider) CanHandle(url string) bool {
	for _, u := range p.canHandleURLs {
		if u == url {
			return true
		}
	}
	return false
}

func (p *recordingProvider) Parse(ctx context.Context, url string) (*providers.Result, error) {
	p.parseCalls = append(p.parseCalls, url)
	return p.result, p.err
}

func TestIntegration_JackettURLParsedThenUpdateUsesHTMLProvider(t *testing.T) {
	jackettURL := "http://jackett:9117/api/v2.0/indexers/rutracker/results/torznab?apikey=KEY&t=details&id=6810475"
	trackerURL := "https://rutracker.org/forum/viewtopic.php?t=6810475"

	jackettProvider := &recordingProvider{
		canHandleURLs: []string{jackettURL},
		result: &providers.Result{
			ID:         "6810475",
			Title:      "Severance S02 2160p",
			Magnet:     "magnet:?xt=urn:btih:abc123",
			UpdatedAt:  time.Date(2026, 3, 10, 14, 0, 0, 0, time.UTC),
			TrackerURL: trackerURL,
		},
	}

	htmlProvider := &recordingProvider{
		canHandleURLs: []string{trackerURL},
		result: &providers.Result{
			ID:        "6810475",
			Title:     "Severance S02 2160p (updated)",
			Magnet:    "magnet:?xt=urn:btih:abc123updated",
			UpdatedAt: time.Date(2026, 3, 11, 10, 0, 0, 0, time.UTC),
		},
	}

	parser := NewParser(
		&mockDownloadClient{defaultLocation: "/downloads/default"},
		jackettProvider,
		htmlProvider,
	)

	metadata, err := parser.Parse(jackettURL, "/downloads/tv")
	require.NoError(t, err)

	assert.Equal(t, "6810475", metadata.ID)
	assert.Equal(t, "Severance S02 2160p", metadata.Name)
	assert.Equal(t, "magnet:?xt=urn:btih:abc123", metadata.Magnet)
	assert.Equal(t, "/downloads/tv", metadata.Location)
	assert.Equal(t, trackerURL, metadata.OriginalUrl)
	assert.Len(t, jackettProvider.parseCalls, 1)

	updateMetadata, err := parser.Parse(metadata.OriginalUrl, "")
	require.NoError(t, err)

	assert.Equal(t, "6810475", updateMetadata.ID)
	assert.Equal(t, "Severance S02 2160p (updated)", updateMetadata.Name)
	assert.Equal(t, "magnet:?xt=urn:btih:abc123updated", updateMetadata.Magnet)
	assert.Equal(t, trackerURL, updateMetadata.OriginalUrl)
	assert.Len(t, htmlProvider.parseCalls, 1)
	assert.Equal(t, trackerURL, htmlProvider.parseCalls[0])
}

func TestIntegration_JackettWithoutTrackerURLFallsBackToJackettURL(t *testing.T) {
	jackettURL := "http://jackett:9117/api/v2.0/indexers/unknown/results/torznab?apikey=KEY&t=details&id=999"

	jackettProvider := &recordingProvider{
		canHandleURLs: []string{jackettURL},
		result: &providers.Result{
			ID:         "999",
			Title:      "Unknown Tracker Item",
			Magnet:     "magnet:?xt=urn:btih:unknown",
			TrackerURL: "",
		},
	}

	parser := NewParser(
		&mockDownloadClient{defaultLocation: "/downloads/default"},
		jackettProvider,
	)

	metadata, err := parser.Parse(jackettURL, "")
	require.NoError(t, err)

	assert.Equal(t, jackettURL, metadata.OriginalUrl)
}

func TestIntegration_RuTrackerURLStillWorksDirectly(t *testing.T) {
	rutrackerURL := "https://rutracker.org/forum/viewtopic.php?t=123456"

	htmlProvider := &recordingProvider{
		canHandleURLs: []string{rutrackerURL},
		result: &providers.Result{
			ID:        "123456",
			Title:     "Direct RuTracker Item",
			Magnet:    "magnet:?xt=urn:btih:direct",
			UpdatedAt: time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC),
		},
	}

	parser := NewParser(
		&mockDownloadClient{defaultLocation: "/downloads/default"},
		htmlProvider,
	)

	metadata, err := parser.Parse(rutrackerURL, "/downloads/movies")
	require.NoError(t, err)

	assert.Equal(t, "123456", metadata.ID)
	assert.Equal(t, rutrackerURL, metadata.OriginalUrl)
	assert.Equal(t, "/downloads/movies", metadata.Location)
	assert.Len(t, htmlProvider.parseCalls, 1)
}
