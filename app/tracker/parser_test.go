package tracker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"magnet-feed-sync/app/tracker/providers"
	"magnet-feed-sync/app/types"
)

type mockDownloadClient struct {
	defaultLocation string
}

func (m *mockDownloadClient) CreateDownloadTask(url, destination string) error {
	return nil
}

func (m *mockDownloadClient) GetHashByMagnet(magnet string) (string, error) {
	return "", nil
}

func (m *mockDownloadClient) SetLocation(taskID, location string) error {
	return nil
}

func (m *mockDownloadClient) GetLocations() []types.Location {
	return nil
}

func (m *mockDownloadClient) GetDefaultLocation() string {
	return m.defaultLocation
}

type mockProvider struct {
	canHandleResult bool
	result          *providers.Result
	err             error
}

func (m *mockProvider) CanHandle(url string) bool {
	return m.canHandleResult
}

func (m *mockProvider) Parse(ctx context.Context, url string) (*providers.Result, error) {
	return m.result, m.err
}

func TestParser_Parse(t *testing.T) {
	mockResult := &providers.Result{
		ID:        "123",
		Title:     "Test Title",
		Magnet:    "magnet:?xt=urn:btih:abc",
		UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Comment:   "test comment",
	}

	t.Run("successful parse with explicit location", func(t *testing.T) {
		p := NewParser(
			&mockDownloadClient{defaultLocation: "/default"},
			&mockProvider{canHandleResult: true, result: mockResult},
		)

		metadata, err := p.Parse("https://example.com/test", "/custom")
		require.NoError(t, err)
		assert.Equal(t, "123", metadata.ID)
		assert.Equal(t, "Test Title", metadata.Name)
		assert.Equal(t, "magnet:?xt=urn:btih:abc", metadata.Magnet)
		assert.Equal(t, "/custom", metadata.Location)
		assert.Equal(t, "https://example.com/test", metadata.OriginalUrl)
		assert.Equal(t, "test comment", metadata.LastComment)
		assert.Equal(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), metadata.TorrentUpdatedAt)
	})

	t.Run("empty location falls back to default", func(t *testing.T) {
		p := NewParser(
			&mockDownloadClient{defaultLocation: "/default"},
			&mockProvider{canHandleResult: true, result: mockResult},
		)

		metadata, err := p.Parse("https://example.com/test", "")
		require.NoError(t, err)
		assert.Equal(t, "/default", metadata.Location)
	})

	t.Run("no matching provider returns error", func(t *testing.T) {
		p := NewParser(
			&mockDownloadClient{defaultLocation: "/default"},
			&mockProvider{canHandleResult: false},
		)

		_, err := p.Parse("https://unknown.com/test", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "provider not found")
	})

	t.Run("provider error is propagated", func(t *testing.T) {
		p := NewParser(
			&mockDownloadClient{defaultLocation: "/default"},
			&mockProvider{canHandleResult: true, err: fmt.Errorf("parse failed")},
		)

		_, err := p.Parse("https://example.com/test", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse failed")
	})
}

func TestParser_LocationParameter(t *testing.T) {
	tests := []struct {
		name             string
		inputLocation    string
		defaultLocation  string
		expectedLocation string
	}{
		{
			name:             "explicit location is used",
			inputLocation:    "/downloads/movies",
			defaultLocation:  "/downloads/tv shows",
			expectedLocation: "/downloads/movies",
		},
		{
			name:             "empty location falls back to default",
			inputLocation:    "",
			defaultLocation:  "/downloads/tv shows",
			expectedLocation: "/downloads/tv shows",
		},
		{
			name:             "custom location overrides default",
			inputLocation:    "/downloads/anime",
			defaultLocation:  "/downloads/other",
			expectedLocation: "/downloads/anime",
		},
	}

	mockResult := &providers.Result{
		ID:     "1",
		Title:  "Test",
		Magnet: "magnet:?xt=urn:btih:abc",
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewParser(
				&mockDownloadClient{defaultLocation: tt.defaultLocation},
				&mockProvider{canHandleResult: true, result: mockResult},
			)

			metadata, err := p.Parse("https://example.com/test", tt.inputLocation)
			require.NoError(t, err)
			assert.Equal(t, tt.expectedLocation, metadata.Location)
		})
	}
}

func TestParser_TrackerURLSwap(t *testing.T) {
	t.Run("TrackerURL replaces input URL as OriginalUrl", func(t *testing.T) {
		jackettURL := "http://jackett:9117/api/v2.0/indexers/rutracker/results/torznab?t=details&id=123"
		trackerURL := "https://rutracker.org/forum/viewtopic.php?t=123"

		p := NewParser(
			&mockDownloadClient{defaultLocation: "/default"},
			&mockProvider{
				canHandleResult: true,
				result: &providers.Result{
					ID:         "123",
					Title:      "Test Torrent",
					Magnet:     "magnet:?xt=urn:btih:abc",
					TrackerURL: trackerURL,
				},
			},
		)

		metadata, err := p.Parse(jackettURL, "/downloads")
		require.NoError(t, err)
		assert.Equal(t, trackerURL, metadata.OriginalUrl)
		assert.Equal(t, "123", metadata.ID)
		assert.Equal(t, "Test Torrent", metadata.Name)
	})

	t.Run("empty TrackerURL falls back to input URL", func(t *testing.T) {
		inputURL := "http://jackett:9117/api/v2.0/indexers/unknown/results/torznab?t=details&id=456"

		p := NewParser(
			&mockDownloadClient{defaultLocation: "/default"},
			&mockProvider{
				canHandleResult: true,
				result: &providers.Result{
					ID:         "456",
					Title:      "Unknown Tracker Torrent",
					Magnet:     "magnet:?xt=urn:btih:def",
					TrackerURL: "",
				},
			},
		)

		metadata, err := p.Parse(inputURL, "")
		require.NoError(t, err)
		assert.Equal(t, inputURL, metadata.OriginalUrl)
	})

	t.Run("non-Jackett provider without TrackerURL keeps original URL", func(t *testing.T) {
		rutrackerURL := "https://rutracker.org/forum/viewtopic.php?t=789"

		p := NewParser(
			&mockDownloadClient{defaultLocation: "/default"},
			&mockProvider{
				canHandleResult: true,
				result: &providers.Result{
					ID:     "789",
					Title:  "Direct Tracker Torrent",
					Magnet: "magnet:?xt=urn:btih:ghi",
				},
			},
		)

		metadata, err := p.Parse(rutrackerURL, "")
		require.NoError(t, err)
		assert.Equal(t, rutrackerURL, metadata.OriginalUrl)
	})
}

func TestParser_ProviderSelection(t *testing.T) {
	result1 := &providers.Result{ID: "from-provider-1", Title: "Provider 1", Magnet: "magnet:1"}
	result2 := &providers.Result{ID: "from-provider-2", Title: "Provider 2", Magnet: "magnet:2"}

	t.Run("first matching provider is used", func(t *testing.T) {
		p := NewParser(
			&mockDownloadClient{defaultLocation: "/default"},
			&mockProvider{canHandleResult: true, result: result1},
			&mockProvider{canHandleResult: true, result: result2},
		)

		metadata, err := p.Parse("https://example.com/test", "")
		require.NoError(t, err)
		assert.Equal(t, "from-provider-1", metadata.ID)
	})

	t.Run("second provider used when first cannot handle", func(t *testing.T) {
		p := NewParser(
			&mockDownloadClient{defaultLocation: "/default"},
			&mockProvider{canHandleResult: false, result: result1},
			&mockProvider{canHandleResult: true, result: result2},
		)

		metadata, err := p.Parse("https://example.com/test", "")
		require.NoError(t, err)
		assert.Equal(t, "from-provider-2", metadata.ID)
	})
}
