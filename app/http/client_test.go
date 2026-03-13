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
	lastURL           string
	lastLocation      string
	lastMagnetHash    string
	lastMagnetMagnet  string
	lastMagnetName    string
	lastMagnetLoc     string
	returnMeta        *tracker.FileMetadata
	returnErr         error
	magnetReturnMeta  *tracker.FileMetadata
	magnetReturnErr   error
}

func (m *mockTaskCreator) CreateFromURL(url, location string) (*tracker.FileMetadata, error) {
	m.lastURL = url
	m.lastLocation = location
	return m.returnMeta, m.returnErr
}

func (m *mockTaskCreator) CreateFromMagnet(hash, magnet, name, location string) (*tracker.FileMetadata, error) {
	m.lastMagnetHash = hash
	m.lastMagnetMagnet = magnet
	m.lastMagnetName = name
	m.lastMagnetLoc = location
	return m.magnetReturnMeta, m.magnetReturnErr
}

func (m *mockTaskCreator) RemoveTask(id string) error              { return nil }
func (m *mockTaskCreator) UpdateTaskLocation(id, location string) error { return nil }
func (m *mockTaskCreator) CheckFileForUpdates(fileId string)       {}
func (m *mockTaskCreator) CheckForUpdates()                        {}

type mockFileStore struct {
	existingFile *tracker.FileMetadata
	getByIdErr   error
}

func (m *mockFileStore) GetAll() ([]*tracker.FileMetadata, error) { return nil, nil }
func (m *mockFileStore) GetById(id string) (*tracker.FileMetadata, error) {
	return m.existingFile, m.getByIdErr
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

	c := NewClient(config.HttpConfig{}, store, creator, dlClient)

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
	now := time.Now()
	creator := &mockTaskCreator{
		magnetReturnMeta: &tracker.FileMetadata{
			ID:         "abc123def456",
			Name:       "Test Torrent",
			Magnet:     "magnet:?xt=urn:btih:abc123def456&dn=test",
			Location:   "/downloads/movies",
			LastSyncAt: now,
		},
	}
	dlClient := &mockDownloadClient{defaultLocation: "/downloads/tv shows"}

	c := NewClient(config.HttpConfig{}, &mockFileStore{}, creator, dlClient)

	body := `{"magnet":"magnet:?xt=urn:btih:abc123def456&dn=test","name":"Test Torrent","location":"/downloads/movies"}`
	req := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateFile(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "abc123def456", creator.lastMagnetHash)
	assert.Equal(t, "magnet:?xt=urn:btih:abc123def456&dn=test", creator.lastMagnetMagnet)
	assert.Equal(t, "Test Torrent", creator.lastMagnetName)
	assert.Equal(t, "/downloads/movies", creator.lastMagnetLoc)

	var resp FileMetadataResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, "abc123def456", resp.ID)
	assert.Equal(t, "Test Torrent", resp.Name)
}

func TestHandleCreateFile_MissingURLAndMagnet(t *testing.T) {
	c := NewClient(config.HttpConfig{}, &mockFileStore{}, &mockTaskCreator{}, &mockDownloadClient{})

	body := `{"location":"/downloads/movies"}`
	req := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateFile(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandleCreateFile_InvalidBody(t *testing.T) {
	c := NewClient(config.HttpConfig{}, &mockFileStore{}, &mockTaskCreator{}, &mockDownloadClient{})

	req := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateFile(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandleCreateFile_URLProviderNotFound(t *testing.T) {
	creator := &mockTaskCreator{
		returnErr: fmt.Errorf("%w for url: https://unknown.com", tracker.ErrProviderNotFound),
	}
	c := NewClient(config.HttpConfig{}, &mockFileStore{}, creator, &mockDownloadClient{})

	body := `{"url":"https://unknown.com"}`
	req := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateFile(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandleCreateFile_URLServerError(t *testing.T) {
	creator := &mockTaskCreator{
		returnErr: fmt.Errorf("network timeout"),
	}
	c := NewClient(config.HttpConfig{}, &mockFileStore{}, creator, &mockDownloadClient{})

	body := `{"url":"https://rutracker.org/forum/viewtopic.php?t=123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateFile(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestHandleCreateFile_MagnetLocationFallback(t *testing.T) {
	now := time.Now()
	creator := &mockTaskCreator{
		magnetReturnMeta: &tracker.FileMetadata{
			ID:         "hash999",
			Name:       "Fallback Test",
			Magnet:     "magnet:?xt=urn:btih:hash999&dn=test",
			Location:   "/downloads/default",
			LastSyncAt: now,
		},
	}
	dlClient := &mockDownloadClient{defaultLocation: "/downloads/default"}

	c := NewClient(config.HttpConfig{}, &mockFileStore{}, creator, dlClient)

	body := `{"magnet":"magnet:?xt=urn:btih:hash999&dn=test","name":"Fallback Test"}`
	req := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateFile(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "/downloads/default", creator.lastMagnetLoc)
}

func TestHandleCreateFile_MagnetError(t *testing.T) {
	creator := &mockTaskCreator{
		magnetReturnErr: fmt.Errorf("download failed"),
	}
	dlClient := &mockDownloadClient{defaultLocation: "/downloads/default"}

	c := NewClient(config.HttpConfig{}, &mockFileStore{}, creator, dlClient)

	body := `{"magnet":"magnet:?xt=urn:btih:abc123","name":"Test"}`
	req := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateFile(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestHandleCreateFile_MagnetInvalidNoHash(t *testing.T) {
	c := NewClient(config.HttpConfig{}, &mockFileStore{}, &mockTaskCreator{}, &mockDownloadClient{})

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
