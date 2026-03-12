package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJackettProvider_CanHandle(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		url     string
		want    bool
	}{
		{
			name:    "matching jackett url",
			baseURL: "http://nas:9117",
			url:     "http://nas:9117/api/v2.0/indexers/rutracker/results/torznab?apikey=KEY&t=details&id=6810475",
			want:    true,
		},
		{
			name:    "matching jackett url with trailing slash in base",
			baseURL: "http://nas:9117/",
			url:     "http://nas:9117/api/v2.0/indexers/rutracker/results/torznab?apikey=KEY",
			want:    true,
		},
		{
			name:    "matching with full api path in base url",
			baseURL: "http://nas:9117/api/v2.0/indexers/all/results/torznab?apikey=KEY",
			url:     "http://nas:9117/api/v2.0/indexers/rutracker/results/torznab?apikey=KEY&t=details&id=6810475",
			want:    true,
		},
		{
			name:    "non-jackett url",
			baseURL: "http://nas:9117",
			url:     "https://rutracker.org/forum/viewtopic.php?t=6810475",
			want:    false,
		},
		{
			name:    "empty url",
			baseURL: "http://nas:9117",
			url:     "",
			want:    false,
		},
		{
			name:    "empty base url",
			baseURL: "",
			url:     "http://nas:9117/api/v2.0/indexers/rutracker/results/torznab",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := NewJackettProvider(tt.baseURL)
			assert.Equal(t, tt.want, provider.CanHandle(tt.url))
		})
	}
}

func TestJackettProvider_Parse_ValidResponse(t *testing.T) {
	fixtureData, err := os.ReadFile("testdata/jackett_rutracker.xml")
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Write(fixtureData)
	}))
	defer server.Close()

	provider := NewJackettProvider(server.URL)

	result, err := provider.Parse(context.Background(), server.URL+"/api/v2.0/indexers/rutracker/results/torznab?apikey=KEY&t=details&id=6810475")
	require.NoError(t, err)

	assert.Equal(t, "6810475", result.ID)
	assert.Equal(t, "Severance S02 2160p WEB-DL DDP5.1 HDR DoVi Hybrid HEVC-FLUX", result.Title)
	assert.Contains(t, result.Magnet, "magnet:?xt=urn:btih:abc123def456")
	assert.Equal(t, "https://rutracker.org/forum/viewtopic.php?t=6810475", result.TrackerURL)
	assert.False(t, result.UpdatedAt.IsZero())
}

func TestJackettProvider_Parse_EmptyResponse(t *testing.T) {
	fixtureData, err := os.ReadFile("testdata/jackett_empty.xml")
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Write(fixtureData)
	}))
	defer server.Close()

	provider := NewJackettProvider(server.URL)

	_, err = provider.Parse(context.Background(), server.URL+"/api/v2.0/indexers/rutracker/results/torznab")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no items found")
}

func TestJackettProvider_Parse_NoTrackerURL(t *testing.T) {
	fixtureData, err := os.ReadFile("testdata/jackett_no_tracker_url.xml")
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Write(fixtureData)
	}))
	defer server.Close()

	provider := NewJackettProvider(server.URL)

	result, err := provider.Parse(context.Background(), server.URL+"/api/v2.0/indexers/test/results/torznab?id=12345")
	require.NoError(t, err)

	assert.Equal(t, "Some Movie 2024 1080p BluRay", result.Title)
	assert.Contains(t, result.Magnet, "magnet:?xt=urn:btih:xyz789")
	assert.Empty(t, result.TrackerURL)
	assert.Equal(t, "12345", result.ID)
}

func TestJackettProvider_Parse_EnclosureMagnet(t *testing.T) {
	fixtureData, err := os.ReadFile("testdata/jackett_enclosure_magnet.xml")
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Write(fixtureData)
	}))
	defer server.Close()

	provider := NewJackettProvider(server.URL)

	result, err := provider.Parse(context.Background(), server.URL+"/api/v2.0/indexers/nnm/results/torznab")
	require.NoError(t, err)

	assert.Equal(t, "Documentary 2025 4K UHD", result.Title)
	assert.Contains(t, result.Magnet, "magnet:?xt=urn:btih:enc456")
	assert.Equal(t, "https://nnmclub.to/forum/viewtopic.php?t=999888", result.TrackerURL)
	assert.Equal(t, "999888", result.ID)
}

func TestJackettProvider_TrackerURL_Extraction(t *testing.T) {
	tests := []struct {
		name       string
		comments   string
		guid       string
		wantURL    string
	}{
		{
			name:     "tracker url from comments",
			comments: "https://rutracker.org/forum/viewtopic.php?t=123",
			guid:     "https://rutracker.org/forum/viewtopic.php?t=123",
			wantURL:  "https://rutracker.org/forum/viewtopic.php?t=123",
		},
		{
			name:     "tracker url from guid when no comments",
			comments: "",
			guid:     "https://nnmclub.to/forum/viewtopic.php?t=456",
			wantURL:  "https://nnmclub.to/forum/viewtopic.php?t=456",
		},
		{
			name:     "no tracker url when non-http values",
			comments: "",
			guid:     "internal-id-789",
			wantURL:  "",
		},
		{
			name:     "comments preferred over guid",
			comments: "https://rutracker.org/forum/viewtopic.php?t=111",
			guid:     "https://nnmclub.to/forum/viewtopic.php?t=222",
			wantURL:  "https://rutracker.org/forum/viewtopic.php?t=111",
		},
	}

	provider := NewJackettProvider("http://localhost:9117")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := torznabItem{
				Comments: tt.comments,
				GUID:     tt.guid,
			}
			got := provider.extractTrackerURL(item)
			assert.Equal(t, tt.wantURL, got)
		})
	}
}

func TestJackettProvider_ExtractID(t *testing.T) {
	tests := []struct {
		name        string
		trackerURL  string
		originalURL string
		wantID      string
	}{
		{
			name:        "id from tracker url t param",
			trackerURL:  "https://rutracker.org/forum/viewtopic.php?t=6810475",
			originalURL: "http://nas:9117/api/v2.0/indexers/rutracker/results/torznab?id=999",
			wantID:      "6810475",
		},
		{
			name:        "id from original url when tracker has no t param",
			trackerURL:  "https://example.com/topic/123",
			originalURL: "http://nas:9117/api/v2.0/indexers/test/results/torznab?id=555",
			wantID:      "555",
		},
		{
			name:        "no id when neither has params",
			trackerURL:  "",
			originalURL: "http://nas:9117/api/v2.0/indexers/test/results/torznab",
			wantID:      "",
		},
	}

	provider := NewJackettProvider("http://nas:9117")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := provider.extractID(tt.trackerURL, tt.originalURL)
			assert.Equal(t, tt.wantID, got)
		})
	}
}

func TestJackettProvider_Parse_InvalidXML(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Write([]byte("not xml at all"))
	}))
	defer server.Close()

	provider := NewJackettProvider(server.URL)

	_, err := provider.Parse(context.Background(), server.URL+"/api/v2.0/indexers/test/results/torznab")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse jackett XML")
}
