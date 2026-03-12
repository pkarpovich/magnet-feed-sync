package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"magnet-feed-sync/app/config"
	"magnet-feed-sync/app/tracker"
	"magnet-feed-sync/app/types"
)

type mockTaskCreator struct {
	lastURL      string
	lastLocation string
	returnMeta   *tracker.FileMetadata
	returnErr    error
}

func (m *mockTaskCreator) CreateFromURL(url, location string) (*tracker.FileMetadata, error) {
	m.lastURL = url
	m.lastLocation = location
	return m.returnMeta, m.returnErr
}

func (m *mockTaskCreator) CheckFileForUpdates(fileId string) {}
func (m *mockTaskCreator) CheckForUpdates()                  {}

type mockFileStore struct {
	lastMetadata *tracker.FileMetadata
	createErr    error
	removedID    string
}

func (m *mockFileStore) GetAll() ([]*tracker.FileMetadata, error) { return nil, nil }
func (m *mockFileStore) GetById(id string) (*tracker.FileMetadata, error) {
	return nil, nil
}
func (m *mockFileStore) CreateOrReplace(metadata *tracker.FileMetadata) error {
	m.lastMetadata = metadata
	return m.createErr
}
func (m *mockFileStore) Remove(id string) error {
	m.removedID = id
	return nil
}

type mockDownloadClient struct {
	lastMagnet      string
	lastDestination string
	defaultLocation string
	downloadErr     error
}

func (m *mockDownloadClient) CreateDownloadTask(url, destination string) error {
	m.lastMagnet = url
	m.lastDestination = destination
	return m.downloadErr
}

func (m *mockDownloadClient) SetLocation(taskID, location string) error { return nil }
func (m *mockDownloadClient) GetLocations() []types.Location            { return nil }
func (m *mockDownloadClient) GetHashByMagnet(magnet string) (string, error) {
	return "", nil
}
func (m *mockDownloadClient) GetDefaultLocation() string {
	return m.defaultLocation
}

func TestHandleCreateFile_WithURL(t *testing.T) {
	now := time.Now()
	creator := &mockTaskCreator{
		returnMeta: &tracker.FileMetadata{
			ID:               "6810475",
			OriginalUrl:      "https://rutracker.org/forum/viewtopic.php?t=6810475",
			Name:             "Severance S02 2160p",
			Magnet:           "magnet:?xt=urn:btih:abc123",
			Location:         "/downloads/tv shows",
			LastSyncAt:       now,
			TorrentUpdatedAt: now,
		},
	}
	store := &mockFileStore{}
	dlClient := &mockDownloadClient{defaultLocation: "/downloads/tv shows"}

	c := NewClient(config.HttpConfig{}, store, creator, dlClient, false)

	body := `{"url":"https://rutracker.org/forum/viewtopic.php?t=6810475","location":"/downloads/tv shows"}`
	req := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateFile(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "https://rutracker.org/forum/viewtopic.php?t=6810475", creator.lastURL)
	assert.Equal(t, "/downloads/tv shows", creator.lastLocation)

	var resp FileMetadataResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, "6810475", resp.ID)
	assert.Equal(t, "Severance S02 2160p", resp.Name)
	assert.Equal(t, "magnet:?xt=urn:btih:abc123", resp.Magnet)
	assert.Equal(t, "/downloads/tv shows", resp.Location)
}

func TestHandleCreateFile_WithMagnet(t *testing.T) {
	store := &mockFileStore{}
	dlClient := &mockDownloadClient{defaultLocation: "/downloads/tv shows"}
	creator := &mockTaskCreator{}

	c := NewClient(config.HttpConfig{}, store, creator, dlClient, false)

	body := `{"magnet":"magnet:?xt=urn:btih:abc123def456&dn=test","name":"Test Torrent","location":"/downloads/movies"}`
	req := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateFile(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)

	require.NotNil(t, store.lastMetadata)
	assert.Equal(t, "abc123def456", store.lastMetadata.ID)
	assert.Equal(t, "Test Torrent", store.lastMetadata.Name)
	assert.Equal(t, "magnet:?xt=urn:btih:abc123def456&dn=test", store.lastMetadata.Magnet)
	assert.Equal(t, "/downloads/movies", store.lastMetadata.Location)
	assert.Empty(t, store.lastMetadata.OriginalUrl)

	assert.Equal(t, "magnet:?xt=urn:btih:abc123def456&dn=test", dlClient.lastMagnet)
	assert.Equal(t, "/downloads/movies", dlClient.lastDestination)

	var resp FileMetadataResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, "abc123def456", resp.ID)
	assert.Equal(t, "Test Torrent", resp.Name)
}

func TestHandleCreateFile_MissingURLAndMagnet(t *testing.T) {
	c := NewClient(config.HttpConfig{}, &mockFileStore{}, &mockTaskCreator{}, &mockDownloadClient{}, false)

	body := `{"location":"/downloads/movies"}`
	req := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateFile(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandleCreateFile_InvalidBody(t *testing.T) {
	c := NewClient(config.HttpConfig{}, &mockFileStore{}, &mockTaskCreator{}, &mockDownloadClient{}, false)

	req := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateFile(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandleCreateFile_URLParseError(t *testing.T) {
	creator := &mockTaskCreator{
		returnErr: fmt.Errorf("provider not found for url: https://unknown.com"),
	}
	c := NewClient(config.HttpConfig{}, &mockFileStore{}, creator, &mockDownloadClient{}, false)

	body := `{"url":"https://unknown.com"}`
	req := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateFile(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestHandleCreateFile_MagnetLocationFallback(t *testing.T) {
	store := &mockFileStore{}
	dlClient := &mockDownloadClient{defaultLocation: "/downloads/default"}
	creator := &mockTaskCreator{}

	c := NewClient(config.HttpConfig{}, store, creator, dlClient, false)

	body := `{"magnet":"magnet:?xt=urn:btih:hash999&dn=test","name":"Fallback Test"}`
	req := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateFile(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
	require.NotNil(t, store.lastMetadata)
	assert.Equal(t, "/downloads/default", store.lastMetadata.Location)
	assert.Equal(t, "/downloads/default", dlClient.lastDestination)
}

func TestHandleCreateFile_MagnetStoreError(t *testing.T) {
	store := &mockFileStore{createErr: fmt.Errorf("db error")}
	dlClient := &mockDownloadClient{defaultLocation: "/downloads/default"}

	c := NewClient(config.HttpConfig{}, store, &mockTaskCreator{}, dlClient, false)

	body := `{"magnet":"magnet:?xt=urn:btih:abc123","name":"Test"}`
	req := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateFile(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestHandleCreateFile_MagnetDownloadError(t *testing.T) {
	store := &mockFileStore{}
	dlClient := &mockDownloadClient{
		defaultLocation: "/downloads/default",
		downloadErr:     fmt.Errorf("download failed"),
	}

	c := NewClient(config.HttpConfig{}, store, &mockTaskCreator{}, dlClient, false)

	body := `{"magnet":"magnet:?xt=urn:btih:abc123","name":"Test"}`
	req := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateFile(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, "abc123", store.removedID)
}

func TestHandleCreateFile_MagnetInvalidNoHash(t *testing.T) {
	c := NewClient(config.HttpConfig{}, &mockFileStore{}, &mockTaskCreator{}, &mockDownloadClient{}, false)

	body := `{"magnet":"magnet:?dn=test","name":"No Hash"}`
	req := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateFile(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestExtractHashFromMagnet(t *testing.T) {
	tests := []struct {
		name     string
		magnet   string
		expected string
	}{
		{
			name:     "standard magnet",
			magnet:   "magnet:?xt=urn:btih:abc123def456&dn=test",
			expected: "abc123def456",
		},
		{
			name:     "magnet without extra params",
			magnet:   "magnet:?xt=urn:btih:abc123def456",
			expected: "abc123def456",
		},
		{
			name:     "no btih",
			magnet:   "magnet:?dn=test",
			expected: "",
		},
		{
			name:     "empty string",
			magnet:   "",
			expected: "",
		},
		{
			name:     "uppercase URN",
			magnet:   "magnet:?xt=URN:BTIH:ABC123&dn=test",
			expected: "abc123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, extractHashFromMagnet(tt.magnet))
		})
	}
}
