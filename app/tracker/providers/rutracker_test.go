package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRutrackerProvider_CanHandle(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{
			name: "valid rutracker url",
			url:  "https://rutracker.org/forum/viewtopic.php?t=6810475",
			want: true,
		},
		{
			name: "non-rutracker url",
			url:  "https://nnmclub.to/forum/viewtopic.php?t=123",
			want: false,
		},
		{
			name: "empty url",
			url:  "",
			want: false,
		},
	}

	provider := &RutrackerProvider{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, provider.CanHandle(tt.url))
		})
	}
}

func TestRutrackerProvider_Parse(t *testing.T) {
	fixtureData, err := os.ReadFile("testdata/rutracker_6810475.html")
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=windows-1251")
		_, _ = w.Write(fixtureData)
	}))
	defer server.Close()

	provider := &RutrackerProvider{}

	result, err := provider.Parse(context.Background(), server.URL+"/forum/viewtopic.php?t=6810475")
	require.NoError(t, err)

	assert.Equal(t, "6810475", result.ID)
	assert.NotEmpty(t, result.Title)
	assert.NotEmpty(t, result.Magnet)
	assert.False(t, result.UpdatedAt.IsZero())
	assert.True(t, result.UpdatedAt.Before(time.Now().Add(-time.Minute)))
	assert.Empty(t, result.TrackerURL)
}

func TestRutrackerProvider_Parse_StableDate(t *testing.T) {
	fixtureData, err := os.ReadFile("testdata/rutracker_6810475.html")
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=windows-1251")
		_, _ = w.Write(fixtureData)
	}))
	defer server.Close()

	provider := &RutrackerProvider{}
	url := server.URL + "/forum/viewtopic.php?t=6810475"

	result1, err := provider.Parse(context.Background(), url)
	require.NoError(t, err)

	time.Sleep(10 * time.Millisecond)

	result2, err := provider.Parse(context.Background(), url)
	require.NoError(t, err)

	assert.Equal(t, result1.UpdatedAt, result2.UpdatedAt)
}

func TestRutrackerProvider_Parse_3304959(t *testing.T) {
	fixtureData, err := os.ReadFile("testdata/rutracker_3304959.html")
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=windows-1251")
		_, _ = w.Write(fixtureData)
	}))
	defer server.Close()

	provider := &RutrackerProvider{}

	result, err := provider.Parse(context.Background(), server.URL+"/forum/viewtopic.php?t=3304959")
	require.NoError(t, err)

	assert.Equal(t, "3304959", result.ID)
	assert.NotEmpty(t, result.Title)
	assert.Contains(t, result.Magnet, "urn:btih:170C2D1D9496B69215647B52506959D7C7647B25")
	assert.Equal(t, time.Date(2026, 3, 22, 12, 59, 0, 0, time.UTC), result.UpdatedAt)
	assert.Empty(t, result.TrackerURL)
}

func TestRutrackerProvider_Parse_3304959_StableDate(t *testing.T) {
	fixtureData, err := os.ReadFile("testdata/rutracker_3304959.html")
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=windows-1251")
		_, _ = w.Write(fixtureData)
	}))
	defer server.Close()

	provider := &RutrackerProvider{}
	url := server.URL + "/forum/viewtopic.php?t=3304959"

	result1, err := provider.Parse(context.Background(), url)
	require.NoError(t, err)

	time.Sleep(10 * time.Millisecond)

	result2, err := provider.Parse(context.Background(), url)
	require.NoError(t, err)

	assert.Equal(t, result1.UpdatedAt, result2.UpdatedAt)
	assert.False(t, result1.UpdatedAt.IsZero())
}

func TestRutrackerProvider_GetID(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "valid_url",
			url:  "https://rutracker.org/forum/viewtopic.php?t=6810475",
			want: "6810475",
		},
		{
			name: "url_with_extra_params",
			url:  "https://rutracker.org/forum/viewtopic.php?t=123456&start=0",
			want: "123456",
		},
	}

	provider := &RutrackerProvider{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := provider.getID(tt.url)
			assert.Equal(t, tt.want, got)
		})
	}
}
