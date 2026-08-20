package http

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"magnet-feed-sync/app/database"
	download_store "magnet-feed-sync/app/download-store"
	"magnet-feed-sync/app/downloads"
	"magnet-feed-sync/app/migrations"
	"magnet-feed-sync/app/tracker"
	"magnet-feed-sync/app/types"
	watch_store "magnet-feed-sync/app/watch-store"
	"magnet-feed-sync/app/watcher"
)

type mockTaskCreator struct {
	lastURL              string
	lastLocation         string
	lastNotify           bool
	createFromURLCalls   int
	lastDownloadSource   string
	lastDownloadLocation string
	downloadCalls        int
	refreshAllCalls      int
	returnMeta           *tracker.FileMetadata
	returnErr            error
	downloadHash         string
	downloadErr          error
	updateLocationCalls  int
	lastUpdatedLocation  string
	updateLocationErr    error
}

func (m *mockTaskCreator) CreateFromURL(_ context.Context, url, location string, notify bool) (*tracker.FileMetadata, error) {
	m.createFromURLCalls++
	m.lastURL = url
	m.lastLocation = location
	m.lastNotify = notify
	return m.returnMeta, m.returnErr
}

func (m *mockTaskCreator) DownloadNow(_ context.Context, source, location string) (string, error) {
	m.downloadCalls++
	m.lastDownloadSource = source
	m.lastDownloadLocation = location
	return m.downloadHash, m.downloadErr
}

type mockNotifier struct {
	enabled bool
}

func (m *mockNotifier) Enabled() bool { return m.enabled }

func (m *mockTaskCreator) RemoveTask(id string) error { return nil }

func (m *mockTaskCreator) UpdateTaskLocation(id, location string) error {
	m.updateLocationCalls++
	m.lastUpdatedLocation = location
	return m.updateLocationErr
}

func (m *mockTaskCreator) CheckFileForUpdates(_ context.Context, _ string) {}
func (m *mockTaskCreator) RefreshAll(_ context.Context)                    { m.refreshAllCalls++ }

type mockFileStore struct {
	existingFile *tracker.FileMetadata
	files        []*tracker.FileMetadata
	getByIdErr   error
}

func (m *mockFileStore) GetAll() ([]*tracker.FileMetadata, error) { return m.files, nil }
func (m *mockFileStore) GetById(id string) (*tracker.FileMetadata, error) {
	return m.existingFile, m.getByIdErr
}

type mockBreaker struct {
	states map[string]tracker.State
}

func (m *mockBreaker) Snapshot() map[string]tracker.State { return m.states }

type mockRunState struct {
	at  time.Time
	ok  bool
	err error
}

func (m *mockRunState) GetLastRun() (time.Time, bool, error) { return m.at, m.ok, m.err }

type mockDownloadClient struct {
	defaultLocation  string
	locations        []types.Location
	hash             string
	hashErr          error
	setLocationErr   error
	setLocationCalls int
	lastSetLocation  string
}

func (m *mockDownloadClient) SetLocation(taskID, location string) error {
	m.setLocationCalls++
	m.lastSetLocation = location
	return m.setLocationErr
}

func (m *mockDownloadClient) GetLocations() []types.Location { return m.locations }

func (m *mockDownloadClient) GetHashByMagnet(magnet string) (string, error) {
	return m.hash, m.hashErr
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

	c := NewClient(&ClientCtx{Store: store, TaskCreator: creator, DownloadClient: dlClient})

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

func TestHandleCreateFile_MissingURL(t *testing.T) {
	c := NewClient(&ClientCtx{Store: &mockFileStore{}, TaskCreator: &mockTaskCreator{}, DownloadClient: &mockDownloadClient{}})

	body := `{"location":"/downloads/movies"}`
	req := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateFile(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandleCreateFile_InvalidBody(t *testing.T) {
	c := NewClient(&ClientCtx{Store: &mockFileStore{}, TaskCreator: &mockTaskCreator{}, DownloadClient: &mockDownloadClient{}})

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
	c := NewClient(&ClientCtx{Store: &mockFileStore{}, TaskCreator: creator, DownloadClient: &mockDownloadClient{}})

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
	c := NewClient(&ClientCtx{Store: &mockFileStore{}, TaskCreator: creator, DownloadClient: &mockDownloadClient{}})

	body := `{"url":"https://rutracker.org/forum/viewtopic.php?t=123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateFile(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestHandleCreateDownload_Magnet(t *testing.T) {
	creator := &mockTaskCreator{}
	dlClient := &mockDownloadClient{defaultLocation: "/downloads/default"}

	c := NewClient(&ClientCtx{Store: &mockFileStore{}, TaskCreator: creator, DownloadClient: dlClient})

	body := `{"source":"magnet:?xt=urn:btih:abc123","location":"/downloads/movies"}`
	req := httptest.NewRequest(http.MethodPost, "/api/downloads", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateDownload(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, 1, creator.downloadCalls)
	assert.Equal(t, "magnet:?xt=urn:btih:abc123", creator.lastDownloadSource)
	assert.Equal(t, "/downloads/movies", creator.lastDownloadLocation)

	var resp map[string]string
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, "ok", resp["status"])
}

func TestHandleCreateDownload_HTTPSource(t *testing.T) {
	creator := &mockTaskCreator{}
	dlClient := &mockDownloadClient{defaultLocation: "/downloads/default"}

	c := NewClient(&ClientCtx{Store: &mockFileStore{}, TaskCreator: creator, DownloadClient: dlClient})

	body := `{"source":"https://jackett.example.com/dl/tpb?apikey=secret&file=x.torrent"}`
	req := httptest.NewRequest(http.MethodPost, "/api/downloads", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateDownload(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, 1, creator.downloadCalls)
	assert.Equal(t, "https://jackett.example.com/dl/tpb?apikey=secret&file=x.torrent", creator.lastDownloadSource)
	assert.Equal(t, "/downloads/default", creator.lastDownloadLocation)
}

func TestHandleCreateDownload_PlainHTTPSource(t *testing.T) {
	creator := &mockTaskCreator{}
	dlClient := &mockDownloadClient{defaultLocation: "/downloads/default"}

	c := NewClient(&ClientCtx{Store: &mockFileStore{}, TaskCreator: creator, DownloadClient: dlClient})

	body := `{"source":"http://tracker.local/dl/x.torrent"}`
	req := httptest.NewRequest(http.MethodPost, "/api/downloads", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateDownload(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, 1, creator.downloadCalls)
	assert.Equal(t, "http://tracker.local/dl/x.torrent", creator.lastDownloadSource)
	assert.Equal(t, "/downloads/default", creator.lastDownloadLocation)
}

func TestHandleCreateDownload_EmptySource(t *testing.T) {
	creator := &mockTaskCreator{}
	c := NewClient(&ClientCtx{Store: &mockFileStore{}, TaskCreator: creator, DownloadClient: &mockDownloadClient{}})

	body := `{"location":"/downloads/movies"}`
	req := httptest.NewRequest(http.MethodPost, "/api/downloads", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateDownload(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, 0, creator.downloadCalls)
}

func TestHandleCreateDownload_GarbageSource(t *testing.T) {
	creator := &mockTaskCreator{}
	c := NewClient(&ClientCtx{Store: &mockFileStore{}, TaskCreator: creator, DownloadClient: &mockDownloadClient{}})

	body := `{"source":"ftp://not-supported/file.torrent"}`
	req := httptest.NewRequest(http.MethodPost, "/api/downloads", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateDownload(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, 0, creator.downloadCalls)
}

func TestHandleCreateDownload_InvalidBody(t *testing.T) {
	c := NewClient(&ClientCtx{Store: &mockFileStore{}, TaskCreator: &mockTaskCreator{}, DownloadClient: &mockDownloadClient{}})

	req := httptest.NewRequest(http.MethodPost, "/api/downloads", bytes.NewBufferString("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateDownload(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandleCreateDownload_DownloadError(t *testing.T) {
	creator := &mockTaskCreator{downloadErr: fmt.Errorf("qbittorrent unreachable")}
	dlClient := &mockDownloadClient{defaultLocation: "/downloads/default"}

	c := NewClient(&ClientCtx{Store: &mockFileStore{}, TaskCreator: creator, DownloadClient: dlClient})

	body := `{"source":"magnet:?xt=urn:btih:abc123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/downloads", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateDownload(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, 1, creator.downloadCalls)
	assert.Equal(t, "/downloads/default", creator.lastDownloadLocation)
}

// the row assertions go through the real repository so a handler that writes a column the
// migration never added fails here instead of on deploy
func newTestDownloadStore(t *testing.T) *download_store.Repository {
	t.Helper()

	t.Chdir(t.TempDir())

	db, err := database.NewClient("test.db")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	_, err = migrations.Apply(db.DB())
	require.NoError(t, err)

	repo, err := download_store.NewRepository(db)
	require.NoError(t, err)

	return repo
}

type failingDownloadStore struct {
	err error
}

func (f *failingDownloadStore) Create(_ *downloads.Download) error { return f.err }

func (f *failingDownloadStore) NewestBySource(_ string) (*downloads.Download, error) {
	return nil, f.err
}

func (f *failingDownloadStore) CountPending() (int, error) { return 0, f.err }

type mockTorrentLookup struct {
	states      map[string]types.TorrentState
	err         error
	askedHashes [][]string
	lookupCalls int
}

func (m *mockTorrentLookup) TorrentStates(_ context.Context, hashes []string) (map[string]types.TorrentState, error) {
	m.lookupCalls++
	m.askedHashes = append(m.askedHashes, hashes)

	if m.err != nil {
		return nil, m.err
	}

	return m.states, nil
}

func postDownload(t *testing.T, c *Client, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/api/downloads", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.handleCreateDownload(w, req)

	return w
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var body map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&body))

	return body
}

func TestHandleCreateDownload_WithoutNotify_WritesNoRow(t *testing.T) {
	store := newTestDownloadStore(t)
	creator := &mockTaskCreator{downloadHash: "474d1403945c0768506233481557516e7af8d136"}

	c := NewClient(&ClientCtx{
		Store:          &mockFileStore{},
		TaskCreator:    creator,
		DownloadClient: &mockDownloadClient{defaultLocation: "/downloads/default"},
		DownloadStore:  store,
		Notifier:       &mockNotifier{enabled: true},
	})

	w := postDownload(t, c, `{"source":"magnet:?xt=urn:btih:abc123"}`)

	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, map[string]any{"status": "ok"}, decodeBody(t, w))
	assert.Equal(t, 1, creator.downloadCalls)

	pending, err := store.CountPending()
	require.NoError(t, err)
	assert.Equal(t, 0, pending)
}

func TestHandleCreateDownload_NotifyWithoutNotifier(t *testing.T) {
	tests := []struct {
		name     string
		notifier notifier
	}{
		{name: "notifier disabled", notifier: &mockNotifier{enabled: false}},
		{name: "notifier unset", notifier: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestDownloadStore(t)
			creator := &mockTaskCreator{}

			c := NewClient(&ClientCtx{
				Store:          &mockFileStore{},
				TaskCreator:    creator,
				DownloadClient: &mockDownloadClient{defaultLocation: "/downloads/default"},
				DownloadStore:  store,
				Notifier:       tt.notifier,
			})

			w := postDownload(t, c, `{"source":"magnet:?xt=urn:btih:abc123","notify":true}`)

			assert.Equal(t, http.StatusServiceUnavailable, w.Code)
			assert.Equal(t, map[string]any{"error": "notifications are not configured"}, decodeBody(t, w))
			assert.Equal(t, 0, creator.downloadCalls, "qbittorrent must not be touched by a refused request")

			pending, err := store.CountPending()
			require.NoError(t, err)
			assert.Equal(t, 0, pending)
		})
	}
}

func TestHandleCreateDownload_NotifyInDryMode(t *testing.T) {
	store := newTestDownloadStore(t)
	creator := &mockTaskCreator{}

	c := NewClient(&ClientCtx{
		Store:          &mockFileStore{},
		TaskCreator:    creator,
		DownloadClient: &mockDownloadClient{defaultLocation: "/downloads/default"},
		DownloadStore:  store,
		Notifier:       &mockNotifier{enabled: true},
		DryMode:        true,
	})

	w := postDownload(t, c, `{"source":"magnet:?xt=urn:btih:abc123","notify":true}`)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, map[string]any{
		"error": "dry mode: no download is created, so no event can be published",
	}, decodeBody(t, w))
	assert.Equal(t, 0, creator.downloadCalls)

	pending, err := store.CountPending()
	require.NoError(t, err)
	assert.Equal(t, 0, pending)
}

func TestHandleCreateDownload_NotifyRecordsRow(t *testing.T) {
	store := newTestDownloadStore(t)
	creator := &mockTaskCreator{downloadHash: "474d1403945c0768506233481557516e7af8d136"}

	c := NewClient(&ClientCtx{
		Store:          &mockFileStore{},
		TaskCreator:    creator,
		DownloadClient: &mockDownloadClient{defaultLocation: "/downloads/default"},
		DownloadStore:  store,
		Notifier:       &mockNotifier{enabled: true},
	})

	w := postDownload(t, c, `{"source":"magnet:?xt=urn:btih:abc123","notify":true}`)

	assert.Equal(t, http.StatusCreated, w.Code)

	body := decodeBody(t, w)
	id, _ := body["download_id"].(string)
	assert.Equal(t, "ok", body["status"])
	assert.Regexp(t, `^[0-9a-f]{16}$`, id)
	assert.Equal(t, map[string]any{
		"status":      "ok",
		"download_id": id,
		"subject":     "tuclaw.downloads.completed." + id,
	}, body)

	row, err := store.GetByID(id)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "magnet:?xt=urn:btih:abc123", row.Source)
	assert.Equal(t, "/downloads/default", row.Location)
	assert.Equal(t, "474d1403945c0768506233481557516e7af8d136", row.Hash)
	assert.Empty(t, row.Status)
	assert.Nil(t, row.PublishedAt)

	pending, err := store.CountPending()
	require.NoError(t, err)
	assert.Equal(t, 1, pending)
}

// an add qbittorrent named no torrent for cannot be swept, so it is refused instead of
// recorded: an unmatchable row publishes a false failure on the subject the caller was handed
func TestHandleCreateDownload_NotifyWithoutHashIsRefused(t *testing.T) {
	store := newTestDownloadStore(t)
	creator := &mockTaskCreator{downloadHash: ""}

	c := NewClient(&ClientCtx{
		Store:          &mockFileStore{},
		TaskCreator:    creator,
		DownloadClient: &mockDownloadClient{defaultLocation: "/downloads/default"},
		DownloadStore:  store,
		Notifier:       &mockNotifier{enabled: true},
	})

	w := postDownload(t, c, `{"source":"https://jackett.example/dl/tpb/torrent.torrent","notify":true}`)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, 1, creator.downloadCalls)

	pending, err := store.CountPending()
	require.NoError(t, err)
	assert.Equal(t, 0, pending)
}

// the same add without notify is the fire-and-forget path, which never needed the hash
func TestHandleCreateDownload_WithoutNotifyIgnoresMissingHash(t *testing.T) {
	store := newTestDownloadStore(t)
	creator := &mockTaskCreator{downloadHash: ""}

	c := NewClient(&ClientCtx{
		Store:          &mockFileStore{},
		TaskCreator:    creator,
		DownloadClient: &mockDownloadClient{defaultLocation: "/downloads/default"},
		DownloadStore:  store,
		Notifier:       &mockNotifier{enabled: true},
	})

	w := postDownload(t, c, `{"source":"https://jackett.example/dl/tpb/torrent.torrent"}`)

	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "ok", decodeBody(t, w)["status"])

	pending, err := store.CountPending()
	require.NoError(t, err)
	assert.Equal(t, 0, pending)
}

func TestHandleCreateDownload_NotifyAddFails(t *testing.T) {
	store := newTestDownloadStore(t)
	creator := &mockTaskCreator{downloadErr: errors.New("qbittorrent unreachable")}

	c := NewClient(&ClientCtx{
		Store:          &mockFileStore{},
		TaskCreator:    creator,
		DownloadClient: &mockDownloadClient{defaultLocation: "/downloads/default"},
		DownloadStore:  store,
		Notifier:       &mockNotifier{enabled: true},
	})

	w := postDownload(t, c, `{"source":"magnet:?xt=urn:btih:abc123","notify":true}`)

	assert.Equal(t, http.StatusInternalServerError, w.Code)

	pending, err := store.CountPending()
	require.NoError(t, err)
	assert.Equal(t, 0, pending)
}

// an id handed back without a row is a subject nothing ever publishes on, so the request
// fails rather than promising an event the sweep knows nothing about
func TestHandleCreateDownload_NotifyStoreFails(t *testing.T) {
	creator := &mockTaskCreator{downloadHash: "474d1403945c0768506233481557516e7af8d136"}

	c := NewClient(&ClientCtx{
		Store:          &mockFileStore{},
		TaskCreator:    creator,
		DownloadClient: &mockDownloadClient{defaultLocation: "/downloads/default"},
		DownloadStore:  &failingDownloadStore{err: errors.New("db is down")},
		Notifier:       &mockNotifier{enabled: true},
	})

	w := postDownload(t, c, `{"source":"magnet:?xt=urn:btih:abc123","notify":true}`)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, 1, creator.downloadCalls)
}

const (
	completedHash   = "474d1403945c0768506233481557516e7af8d136"
	completedMagnet = "magnet:?xt=urn:btih:474d1403945c0768506233481557516e7af8d136"
)

func completedTorrent() types.TorrentState {
	return types.TorrentState{
		Hash:         completedHash,
		Name:         "sample.bin",
		State:        "stalledUP",
		ContentPath:  "/downloads/probe/sample.bin",
		Progress:     1,
		CompletionOn: 1786626099,
		Size:         4194304,
	}
}

func unfinishedTorrent() types.TorrentState {
	return types.TorrentState{
		Hash:         completedHash,
		Name:         "ubuntu-24.04.1-desktop-amd64.iso",
		State:        "downloading",
		ContentPath:  "/downloads/probe-magnet/ubuntu-24.04.1-desktop-amd64.iso",
		Progress:     0,
		CompletionOn: -1,
		Size:         5173995520,
	}
}

func duplicateClient(store downloadStore, lookup torrentLookup) *Client {
	return NewClient(&ClientCtx{
		Store:          &mockFileStore{},
		TaskCreator:    &mockTaskCreator{downloadErr: fmt.Errorf("add torrent: %w", types.ErrTorrentAlreadyExists)},
		DownloadClient: &mockDownloadClient{defaultLocation: "/downloads/default"},
		DownloadStore:  store,
		TorrentLookup:  lookup,
		Notifier:       &mockNotifier{enabled: true},
	})
}

func TestHandleCreateDownload_DuplicateWithoutNotify(t *testing.T) {
	store := newTestDownloadStore(t)
	lookup := &mockTorrentLookup{states: map[string]types.TorrentState{completedHash: completedTorrent()}}

	c := duplicateClient(store, lookup)

	w := postDownload(t, c, `{"source":"`+completedMagnet+`"}`)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, map[string]any{
		"status":    "ok",
		"duplicate": true,
		"hash":      completedHash,
		"state":     "stalledUP",
		"completed": true,
	}, decodeBody(t, w))
	assert.Equal(t, [][]string{{completedHash}}, lookup.askedHashes)

	pending, err := store.CountPending()
	require.NoError(t, err)
	assert.Equal(t, 0, pending)
}

func TestHandleCreateDownload_DuplicateNotifyStillDownloading(t *testing.T) {
	store := newTestDownloadStore(t)
	lookup := &mockTorrentLookup{states: map[string]types.TorrentState{completedHash: unfinishedTorrent()}}

	c := duplicateClient(store, lookup)

	w := postDownload(t, c, `{"source":"`+completedMagnet+`","notify":true}`)

	assert.Equal(t, http.StatusOK, w.Code)

	body := decodeBody(t, w)
	id, _ := body["download_id"].(string)
	assert.Regexp(t, `^[0-9a-f]{16}$`, id)
	assert.Equal(t, map[string]any{
		"status":      "ok",
		"duplicate":   true,
		"hash":        completedHash,
		"state":       "downloading",
		"completed":   false,
		"download_id": id,
		"subject":     "tuclaw.downloads.completed." + id,
	}, body)

	pending, err := store.Pending()
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, id, pending[0].ID)
	assert.Equal(t, completedHash, pending[0].Hash)
	assert.Equal(t, "/downloads/default", pending[0].Location)
	assert.Nil(t, pending[0].PublishedAt)
}

// the caller was just told inline that it is finished; a row the sweep would publish on top
// of that wakes the agent twice for one download
func TestHandleCreateDownload_DuplicateNotifyAlreadyComplete(t *testing.T) {
	store := newTestDownloadStore(t)
	lookup := &mockTorrentLookup{states: map[string]types.TorrentState{completedHash: completedTorrent()}}

	c := duplicateClient(store, lookup)

	w := postDownload(t, c, `{"source":"`+completedMagnet+`","notify":true}`)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, map[string]any{
		"status":    "ok",
		"duplicate": true,
		"hash":      completedHash,
		"state":     "stalledUP",
		"completed": true,
	}, decodeBody(t, w))

	pending, err := store.Pending()
	require.NoError(t, err)
	assert.Empty(t, pending)

	row, err := store.NewestBySource(completedMagnet)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "completed", row.Status)
	assert.Equal(t, "sample.bin", row.Name)
	assert.Equal(t, "/downloads/probe/sample.bin", row.ContentPath)
	assert.Equal(t, int64(4194304), row.Size)
	require.NotNil(t, row.CompletedAt)
	assert.Equal(t, time.Unix(1786626099, 0).UTC(), row.CompletedAt.UTC())
	assert.NotNil(t, row.PublishedAt)
}

func TestHandleCreateDownload_DuplicateNotifyTerminalFailure(t *testing.T) {
	tests := []struct {
		name          string
		lookup        *mockTorrentLookup
		expectedState string
	}{
		{
			name: "qbittorrent reports an error state",
			lookup: &mockTorrentLookup{states: map[string]types.TorrentState{
				completedHash: {Hash: completedHash, State: "error", Progress: 1, CompletionOn: 1786626099},
			}},
			expectedState: "error",
		},
		{
			name:          "hash absent from the lookup",
			lookup:        &mockTorrentLookup{states: map[string]types.TorrentState{}},
			expectedState: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestDownloadStore(t)

			c := duplicateClient(store, tt.lookup)

			w := postDownload(t, c, `{"source":"`+completedMagnet+`","notify":true}`)

			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, map[string]any{
				"status":    "ok",
				"duplicate": true,
				"hash":      completedHash,
				"state":     tt.expectedState,
				"completed": false,
			}, decodeBody(t, w))

			row, err := store.NewestBySource(completedMagnet)
			require.NoError(t, err)
			assert.Nil(t, row, "a failed duplicate must not leave a row the sweep would publish")
		})
	}
}

// a lookup that could not be performed is not a torrent that is gone: the 409 proved it is there,
// so the promise is kept with a row the sweep will publish rather than dropped silently
func TestHandleCreateDownload_DuplicateNotifyUnreadableState(t *testing.T) {
	tests := []struct {
		name   string
		lookup *mockTorrentLookup
	}{
		{name: "lookup failed", lookup: &mockTorrentLookup{err: errors.New("qbittorrent unreachable")}},
		{name: "lookup unset"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestDownloadStore(t)

			var lookup torrentLookup
			if tt.lookup != nil {
				lookup = tt.lookup
			}

			c := duplicateClient(store, lookup)

			w := postDownload(t, c, `{"source":"`+completedMagnet+`","notify":true}`)

			assert.Equal(t, http.StatusOK, w.Code)

			body := decodeBody(t, w)
			id, _ := body["download_id"].(string)
			assert.Regexp(t, `^[0-9a-f]{16}$`, id)
			assert.Equal(t, map[string]any{
				"status":      "ok",
				"duplicate":   true,
				"hash":        completedHash,
				"state":       "unknown",
				"completed":   false,
				"download_id": id,
				"subject":     "tuclaw.downloads.completed." + id,
			}, body)

			pending, err := store.Pending()
			require.NoError(t, err)
			require.Len(t, pending, 1)
			assert.Equal(t, id, pending[0].ID)
			assert.Equal(t, completedHash, pending[0].Hash)
		})
	}
}

func TestHandleCreateDownload_DuplicateTorrentURLResolvedFromStore(t *testing.T) {
	store := newTestDownloadStore(t)
	source := "https://tracker.local/download/42.torrent"

	require.NoError(t, store.Create(&downloads.Download{
		ID:        "0011223344556677",
		Source:    source,
		Location:  "/downloads/default",
		Hash:      completedHash,
		CreatedAt: time.Now(),
	}))

	lookup := &mockTorrentLookup{states: map[string]types.TorrentState{completedHash: completedTorrent()}}
	c := duplicateClient(store, lookup)

	w := postDownload(t, c, `{"source":"`+source+`"}`)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, map[string]any{
		"status":    "ok",
		"duplicate": true,
		"hash":      completedHash,
		"state":     "stalledUP",
		"completed": true,
	}, decodeBody(t, w))
}

func TestHandleCreateDownload_DuplicateUnresolvableHash(t *testing.T) {
	store := newTestDownloadStore(t)
	lookup := &mockTorrentLookup{states: map[string]types.TorrentState{completedHash: completedTorrent()}}

	c := duplicateClient(store, lookup)

	w := postDownload(t, c, `{"source":"https://tracker.local/download/unseen.torrent","notify":true}`)

	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, map[string]any{
		"error": "torrent already present and its hash could not be resolved from the source",
	}, decodeBody(t, w))
	assert.Equal(t, 0, lookup.lookupCalls)

	pending, err := store.CountPending()
	require.NoError(t, err)
	assert.Equal(t, 0, pending)
}

// a base32 magnet yields a hash torrents/info never reports, so answering with it would tell the
// caller the download failed and hand back no subject — the same guard the download client applies
const base32Magnet = "magnet:?xt=urn:btih:MFRGGZDFMZTWQ2LKNNWG23TPOBYXE43U&dn=Some.Name"

func TestHandleCreateDownload_DuplicateBase32MagnetIsNotAHash(t *testing.T) {
	store := newTestDownloadStore(t)
	lookup := &mockTorrentLookup{states: map[string]types.TorrentState{completedHash: completedTorrent()}}

	c := duplicateClient(store, lookup)

	w := postDownload(t, c, `{"source":"`+base32Magnet+`","notify":true}`)

	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, map[string]any{
		"error": "torrent already present and its hash could not be resolved from the source",
	}, decodeBody(t, w))
	assert.Equal(t, 0, lookup.lookupCalls)

	pending, err := store.CountPending()
	require.NoError(t, err)
	assert.Equal(t, 0, pending)
}

func TestHandleCreateDownload_DuplicateBase32MagnetFallsBackToStore(t *testing.T) {
	store := newTestDownloadStore(t)

	require.NoError(t, store.Create(&downloads.Download{
		ID:        "0011223344556677",
		Source:    base32Magnet,
		Location:  "/downloads/default",
		Hash:      completedHash,
		CreatedAt: time.Now(),
	}))

	lookup := &mockTorrentLookup{states: map[string]types.TorrentState{completedHash: completedTorrent()}}
	c := duplicateClient(store, lookup)

	w := postDownload(t, c, `{"source":"`+base32Magnet+`"}`)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, map[string]any{
		"status":    "ok",
		"duplicate": true,
		"hash":      completedHash,
		"state":     "stalledUP",
		"completed": true,
	}, decodeBody(t, w))
}

// the 415 "torrent file not valid" branch shares ErrTorrentAddFailed with the 409 one; only a
// duplicate may answer 200, so an over-matched sentinel would report a broken URL as present
func TestHandleCreateDownload_NonDuplicateAddFailureStays500(t *testing.T) {
	store := newTestDownloadStore(t)
	creator := &mockTaskCreator{downloadErr: errors.New("add torrent: torrent file not valid")}

	c := NewClient(&ClientCtx{
		Store:          &mockFileStore{},
		TaskCreator:    creator,
		DownloadClient: &mockDownloadClient{defaultLocation: "/downloads/default"},
		DownloadStore:  store,
		TorrentLookup:  &mockTorrentLookup{},
		Notifier:       &mockNotifier{enabled: true},
	})

	w := postDownload(t, c, `{"source":"https://tracker.local/download/42.torrent","notify":true}`)

	assert.Equal(t, http.StatusInternalServerError, w.Code)

	pending, err := store.CountPending()
	require.NoError(t, err)
	assert.Equal(t, 0, pending)
}

func TestHandleCreateDownload_DuplicateStoreFails(t *testing.T) {
	lookup := &mockTorrentLookup{states: map[string]types.TorrentState{completedHash: unfinishedTorrent()}}

	c := duplicateClient(&failingDownloadStore{err: errors.New("db is down")}, lookup)

	w := postDownload(t, c, `{"source":"`+completedMagnet+`","notify":true}`)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func setupTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	orig := otel.GetTracerProvider()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(orig)
	})
	return exporter
}

func TestHTTPHandlers_CreateTracingSpans(t *testing.T) {
	tests := []struct {
		name         string
		method       string
		path         string
		handler      func(*Client) http.HandlerFunc
		expectedSpan string
	}{
		{"handleFiles", http.MethodGet, "/api/files", func(c *Client) http.HandlerFunc { return c.handleFiles }, "GET /api/files"},
		{"handleCreateFile", http.MethodPost, "/api/files", func(c *Client) http.HandlerFunc { return c.handleCreateFile }, "POST /api/files"},
		{"handleCreateDownload", http.MethodPost, "/api/downloads", func(c *Client) http.HandlerFunc { return c.handleCreateDownload }, "POST /api/downloads"},
		{"handleGetFileLocations", http.MethodGet, "/api/file-locations", func(c *Client) http.HandlerFunc { return c.handleGetFileLocations }, "GET /api/file-locations"},
		{"healthHandler", http.MethodGet, "/api/health", func(c *Client) http.HandlerFunc { return c.healthHandler }, "GET /api/health"},
		{"handleRefreshAllFiles", http.MethodPatch, "/api/files/refresh", func(c *Client) http.HandlerFunc { return c.handleRefreshAllFiles }, "PATCH /api/files/refresh"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exporter := setupTestTracer(t)

			store := &mockFileStore{}
			creator := &mockTaskCreator{}
			dlClient := &mockDownloadClient{}
			c := NewClient(&ClientCtx{Store: store, TaskCreator: creator, DownloadClient: dlClient})

			req := httptest.NewRequest(tt.method, tt.path, bytes.NewBufferString("{}"))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			tt.handler(c)(w, req)

			spans := exporter.GetSpans()
			require.GreaterOrEqual(t, len(spans), 1)

			spanNames := make([]string, len(spans))
			for i, s := range spans {
				spanNames[i] = s.Name
			}
			assert.Contains(t, spanNames, tt.expectedSpan)
		})
	}
}

func TestHTTPHandlers_NoopTracingNoCrash(t *testing.T) {
	otel.SetTracerProvider(otel.GetTracerProvider())

	store := &mockFileStore{}
	creator := &mockTaskCreator{}
	dlClient := &mockDownloadClient{}
	c := NewClient(&ClientCtx{Store: store, TaskCreator: creator, DownloadClient: dlClient})

	req := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	w := httptest.NewRecorder()

	c.handleFiles(w, req)
}

func failingFiles(counts ...int) []*tracker.FileMetadata {
	files := make([]*tracker.FileMetadata, 0, len(counts))
	for i, count := range counts {
		files = append(files, &tracker.FileMetadata{
			ID:                  fmt.Sprintf("task-%d", i),
			ConsecutiveFailures: count,
		})
	}

	return files
}

func decodeHealth(t *testing.T, w *httptest.ResponseRecorder) healthResponse {
	t.Helper()

	var resp healthResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))

	return resp
}

func callHealth(t *testing.T, ctx *ClientCtx) (*httptest.ResponseRecorder, healthResponse) {
	t.Helper()

	c := NewClient(ctx)
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	w := httptest.NewRecorder()
	c.healthHandler(w, req)

	return w, decodeHealth(t, w)
}

func TestHealthOK(t *testing.T) {
	w, resp := callHealth(t, &ClientCtx{
		Store:            &mockFileStore{files: failingFiles(0, 1, 2)},
		Breaker:          &mockBreaker{states: map[string]tracker.State{"rutracker": {}, "nnm": {}}},
		RunState:         &mockRunState{at: time.Now().Add(-30 * time.Minute), ok: true},
		StaleRunAfter:    2 * time.Hour,
		StartedAt:        time.Now().Add(-5 * time.Hour),
		FailureThreshold: 3,
	})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ok", resp.Status)
	assert.Equal(t, 3, resp.Tracked)
	assert.Equal(t, 0, resp.Failing)
	assert.Equal(t, map[string]string{"rutracker": "ok", "nnm": "ok"}, resp.Providers)
	require.NotNil(t, resp.LastRunAt)
}

func TestHealthDegraded(t *testing.T) {
	w, resp := callHealth(t, &ClientCtx{
		Store:            &mockFileStore{files: failingFiles(0, 3, 7)},
		Breaker:          &mockBreaker{states: map[string]tracker.State{"rutracker": {}}},
		RunState:         &mockRunState{at: time.Now().Add(-10 * time.Minute), ok: true},
		StaleRunAfter:    2 * time.Hour,
		FailureThreshold: 3,
	})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "degraded", resp.Status)
	assert.Equal(t, 3, resp.Tracked)
	assert.Equal(t, 2, resp.Failing)
}

// a sweep that could not read the task list still refreshes last_run_at, so staleness alone
// would report a cron that checks nothing as healthy
func TestHealthDegradedWhenLastRunFailed(t *testing.T) {
	w, resp := callHealth(t, &ClientCtx{
		Store:            &mockFileStore{files: failingFiles(0, 1)},
		Breaker:          &mockBreaker{states: map[string]tracker.State{"rutracker": {}}},
		RunState:         &mockRunState{at: time.Now().Add(-10 * time.Minute), ok: false},
		StaleRunAfter:    2 * time.Hour,
		FailureThreshold: 3,
	})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "degraded", resp.Status)
	assert.Equal(t, 0, resp.Failing)
}

// an unset threshold would make `>= 0` true for every row and pin health to degraded forever
func TestHealthThresholdDefaultsWhenUnset(t *testing.T) {
	w, resp := callHealth(t, &ClientCtx{
		Store:         &mockFileStore{files: failingFiles(0, 0)},
		Breaker:       &mockBreaker{states: map[string]tracker.State{"rutracker": {}}},
		RunState:      &mockRunState{at: time.Now().Add(-10 * time.Minute), ok: true},
		StaleRunAfter: 2 * time.Hour,
	})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ok", resp.Status)
	assert.Equal(t, 0, resp.Failing)
}

func TestHealthUnhealthyBreaker(t *testing.T) {
	nextProbe := time.Now().Add(time.Hour)
	w, resp := callHealth(t, &ClientCtx{
		Store: &mockFileStore{files: failingFiles(0)},
		Breaker: &mockBreaker{states: map[string]tracker.State{
			"rutracker": {Tripped: true, NextProbeAt: nextProbe, Cooldown: time.Hour},
			"nnm":       {},
		}},
		RunState:         &mockRunState{at: time.Now().Add(-10 * time.Minute), ok: true},
		StaleRunAfter:    2 * time.Hour,
		FailureThreshold: 3,
	})

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "unhealthy", resp.Status)
	assert.Equal(t, map[string]string{"rutracker": "blocked", "nnm": "ok"}, resp.Providers)
}

func TestHealthUnhealthyStale(t *testing.T) {
	tests := []struct {
		name       string
		lastRunAt  time.Time
		wantStatus string
		wantCode   int
	}{
		{name: "stale", lastRunAt: time.Now().Add(-3 * time.Hour), wantStatus: "unhealthy", wantCode: http.StatusServiceUnavailable},
		{name: "fresh", lastRunAt: time.Now().Add(-90 * time.Minute), wantStatus: "ok", wantCode: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, resp := callHealth(t, &ClientCtx{
				Store:            &mockFileStore{},
				Breaker:          &mockBreaker{states: map[string]tracker.State{"rutracker": {}}},
				RunState:         &mockRunState{at: tt.lastRunAt, ok: true},
				StaleRunAfter:    2 * time.Hour,
				StartedAt:        time.Now().Add(-10 * time.Hour),
				FailureThreshold: 3,
			})

			assert.Equal(t, tt.wantCode, w.Code)
			assert.Equal(t, tt.wantStatus, resp.Status)
		})
	}
}

func TestHealthNeverRanWithinGrace(t *testing.T) {
	tests := []struct {
		name       string
		startedAt  time.Time
		wantStatus string
		wantCode   int
	}{
		{name: "within grace", startedAt: time.Now().Add(-10 * time.Minute), wantStatus: "ok", wantCode: http.StatusOK},
		{name: "past grace", startedAt: time.Now().Add(-3 * time.Hour), wantStatus: "unhealthy", wantCode: http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, resp := callHealth(t, &ClientCtx{
				Store:            &mockFileStore{},
				Breaker:          &mockBreaker{states: map[string]tracker.State{"rutracker": {}}},
				RunState:         &mockRunState{},
				StaleRunAfter:    2 * time.Hour,
				StartedAt:        tt.startedAt,
				FailureThreshold: 3,
			})

			assert.Equal(t, tt.wantCode, w.Code)
			assert.Equal(t, tt.wantStatus, resp.Status)
			assert.Nil(t, resp.LastRunAt)
		})
	}
}

func watchWithRun(id string, lastRunAt *time.Time, status string) *watcher.Watch {
	return &watcher.Watch{
		ID:         id,
		Queries:    []string{"One Night Only 2026"},
		Sources:    []string{"jackett"},
		Rev:        1,
		LastRunAt:  lastRunAt,
		LastStatus: status,
	}
}

func ago(d time.Duration) *time.Time {
	at := time.Now().Add(-d)

	return &at
}

// watchHealthCtx keeps the files half of health unconditionally ok, so any status the
// watcher tests observe can only have come from the watches themselves.
func watchHealthCtx(store watchStore, startedAt time.Time) *ClientCtx {
	return &ClientCtx{
		Store:            &mockFileStore{},
		Breaker:          &mockBreaker{states: map[string]tracker.State{"rutracker": {}}},
		RunState:         &mockRunState{at: time.Now().Add(-10 * time.Minute), ok: true},
		WatchStore:       store,
		StaleRunAfter:    2 * time.Hour,
		StaleWatchAfter:  2 * time.Hour,
		StartedAt:        startedAt,
		FailureThreshold: 3,
	}
}

func TestHealthWatchesOKWhenNone(t *testing.T) {
	w, resp := callHealth(t, watchHealthCtx(newMockWatchStore(), time.Now().Add(-5*time.Hour)))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ok", resp.Status)
	require.NotNil(t, resp.Watches)
	assert.Equal(t, 0, resp.Watches.Active)
	assert.Equal(t, 0, resp.Watches.WithErrors)
	assert.Nil(t, resp.Watches.OldestRunAt)
}

func TestHealthWatchesOKWhenFresh(t *testing.T) {
	oldest := ago(40 * time.Minute)
	store := newMockWatchStore(
		watchWithRun("recent", ago(5*time.Minute), ""),
		watchWithRun("older", oldest, ""),
	)

	w, resp := callHealth(t, watchHealthCtx(store, time.Now().Add(-5*time.Hour)))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ok", resp.Status)
	require.NotNil(t, resp.Watches)
	assert.Equal(t, 2, resp.Watches.Active)
	assert.Equal(t, 0, resp.Watches.WithErrors)
	require.NotNil(t, resp.Watches.OldestRunAt)
	assert.WithinDuration(t, *oldest, *resp.Watches.OldestRunAt, time.Second)
}

func TestHealthWatchesDegradedWhenStale(t *testing.T) {
	store := newMockWatchStore(
		watchWithRun("recent", ago(5*time.Minute), ""),
		watchWithRun("stale", ago(3*time.Hour), ""),
	)

	w, resp := callHealth(t, watchHealthCtx(store, time.Now().Add(-5*time.Hour)))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "degraded", resp.Status)
	require.NotNil(t, resp.Watches)
	assert.Equal(t, 2, resp.Watches.Active)
}

func TestHealthWatchesDegradedOnErrorStatus(t *testing.T) {
	store := newMockWatchStore(
		watchWithRun("clean", ago(5*time.Minute), ""),
		watchWithRun("broken", ago(5*time.Minute), "jackett: search failed"),
	)

	w, resp := callHealth(t, watchHealthCtx(store, time.Now().Add(-5*time.Hour)))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "degraded", resp.Status)
	require.NotNil(t, resp.Watches)
	assert.Equal(t, 1, resp.Watches.WithErrors)
}

// a watch created through the API seconds ago has no last_run_at yet, and reporting the
// service degraded before its first tick could possibly have run would be a false alarm
func TestHealthWatchesNeverRanWithinGrace(t *testing.T) {
	tests := []struct {
		name       string
		startedAt  time.Time
		wantStatus string
	}{
		{name: "within grace", startedAt: time.Now().Add(-10 * time.Minute), wantStatus: "ok"},
		{name: "past grace", startedAt: time.Now().Add(-3 * time.Hour), wantStatus: "degraded"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newMockWatchStore(watchWithRun("fresh", nil, ""))

			w, resp := callHealth(t, watchHealthCtx(store, tt.startedAt))

			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, tt.wantStatus, resp.Status)
			require.NotNil(t, resp.Watches)
			assert.Equal(t, 1, resp.Watches.Active)
			assert.Nil(t, resp.Watches.OldestRunAt)
		})
	}
}

// disabled and expired watches are nobody's problem: a soft-deleted or elapsed watch left in
// an error state would otherwise pin the service to degraded forever
func TestHealthWatchesIgnoreExpiredAndDisabled(t *testing.T) {
	expired := watchWithRun("expired", ago(3*time.Hour), "jackett: search failed")
	expired.ExpiresAt = ago(time.Minute)
	store := newMockWatchStore(expired, watchWithRun("removed", ago(3*time.Hour), "boom"))
	require.NoError(t, store.Disable("removed"))

	w, resp := callHealth(t, watchHealthCtx(store, time.Now().Add(-5*time.Hour)))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ok", resp.Status)
	require.NotNil(t, resp.Watches)
	assert.Equal(t, 0, resp.Watches.Active)
	assert.Equal(t, 0, resp.Watches.WithErrors)
}

// the watcher check may only raise ok to degraded; a tripped breaker stays unhealthy
func TestHealthWatchesDoNotLowerUnhealthy(t *testing.T) {
	ctx := watchHealthCtx(newMockWatchStore(watchWithRun("broken", ago(5*time.Minute), "boom")), time.Now().Add(-5*time.Hour))
	ctx.Breaker = &mockBreaker{states: map[string]tracker.State{"rutracker": {Tripped: true}}}

	w, resp := callHealth(t, ctx)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "unhealthy", resp.Status)
	require.NotNil(t, resp.Watches)
	assert.Equal(t, 1, resp.Watches.WithErrors)
}

// a watch store that cannot be read is a real fault, but it must not take the whole health
// endpoint down with it
func TestHealthWatchesStoreErrorDegrades(t *testing.T) {
	store := newMockWatchStore()
	store.cycleErr = errors.New("db is down")

	w, resp := callHealth(t, watchHealthCtx(store, time.Now().Add(-5*time.Hour)))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "degraded", resp.Status)
	assert.Nil(t, resp.Watches)
}

var knownLocations = []types.Location{
	{ID: "/downloads/tv shows", Name: "TV Shows"},
	{ID: "/downloads/magazines", Name: "Magazines"},
}

func callSetLocation(t *testing.T, creator *mockTaskCreator, dClient *mockDownloadClient, store *mockFileStore, location string) (*httptest.ResponseRecorder, SetFileLocationResponse) {
	t.Helper()

	payload, err := json.Marshal(SetFileLocationRequest{FileId: "6810475", Location: location})
	require.NoError(t, err)

	c := NewClient(&ClientCtx{Store: store, TaskCreator: creator, DownloadClient: dClient})
	req := httptest.NewRequest(http.MethodPost, "/api/file-locations", bytes.NewReader(payload))
	w := httptest.NewRecorder()
	c.handleSetFileLocation(w, req)

	var resp SetFileLocationResponse
	if w.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	}

	return w, resp
}

func trackedFile() *mockFileStore {
	return &mockFileStore{existingFile: &tracker.FileMetadata{ID: "6810475", Magnet: "magnet:?xt=urn:btih:abc123"}}
}

// the weekly cleanup removes completed torrents from the client, so a missing torrent is
// the normal case - it must not block recording where the next download goes
func TestSetFileLocationPersistsWhenTorrentGone(t *testing.T) {
	creator := &mockTaskCreator{}
	dClient := &mockDownloadClient{locations: knownLocations, hashErr: types.ErrTorrentNotFound}

	w, resp := callSetLocation(t, creator, dClient, trackedFile(), "/downloads/magazines")

	assert.Equal(t, http.StatusOK, w.Code)
	assert.False(t, resp.Moved)
	assert.NotEmpty(t, resp.Reason)
	assert.Equal(t, "/downloads/magazines", resp.Location)
	assert.Equal(t, 1, creator.updateLocationCalls)
	assert.Equal(t, "/downloads/magazines", creator.lastUpdatedLocation)
	assert.Equal(t, 0, dClient.setLocationCalls)
}

func TestSetFileLocationMovesFilesWhenTorrentPresent(t *testing.T) {
	creator := &mockTaskCreator{}
	dClient := &mockDownloadClient{locations: knownLocations, hash: "abc123"}

	w, resp := callSetLocation(t, creator, dClient, trackedFile(), "/downloads/magazines")

	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, resp.Moved)
	assert.Empty(t, resp.Reason)
	assert.Equal(t, 1, creator.updateLocationCalls)
	assert.Equal(t, 1, dClient.setLocationCalls)
	assert.Equal(t, "/downloads/magazines", dClient.lastSetLocation)
}

func TestSetFileLocationPersistsWhenMoveFails(t *testing.T) {
	creator := &mockTaskCreator{}
	dClient := &mockDownloadClient{locations: knownLocations, hash: "abc123", setLocationErr: errors.New("qbittorrent unreachable")}

	w, resp := callSetLocation(t, creator, dClient, trackedFile(), "/downloads/magazines")

	assert.Equal(t, http.StatusOK, w.Code)
	assert.False(t, resp.Moved)
	assert.Contains(t, resp.Reason, "qbittorrent unreachable")
	assert.Equal(t, 1, creator.updateLocationCalls)
}

func TestSetFileLocationRejectsUnknownLocation(t *testing.T) {
	creator := &mockTaskCreator{}
	dClient := &mockDownloadClient{locations: knownLocations}

	w, _ := callSetLocation(t, creator, dClient, trackedFile(), "/downloads/typo")

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, 0, creator.updateLocationCalls)
}

func TestSetFileLocationUnknownFile(t *testing.T) {
	creator := &mockTaskCreator{}
	dClient := &mockDownloadClient{locations: knownLocations}
	store := &mockFileStore{getByIdErr: sql.ErrNoRows}

	w, _ := callSetLocation(t, creator, dClient, store, "/downloads/magazines")

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, 0, creator.updateLocationCalls)
}

func TestSetFileLocationStoreFailureIsAnError(t *testing.T) {
	creator := &mockTaskCreator{updateLocationErr: errors.New("db is down")}
	dClient := &mockDownloadClient{locations: knownLocations, hash: "abc123"}

	w, _ := callSetLocation(t, creator, dClient, trackedFile(), "/downloads/magazines")

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, 0, dClient.setLocationCalls)
}

// mockWatchStore stands in for the repository on both seams at once: the http handlers and
// the engine share one instance, which is what lets TestCronAndEndpointAgree run a cycle and
// then an endpoint against the same state.
type mockWatchStore struct {
	watches   map[string]*watcher.Watch
	order     []string
	seen      []watch_store.SeenRow
	marked    map[string]map[string]watcher.SearchResult
	statuses  map[string]string
	disabled  []string
	revived   []string
	createErr error
	updateErr error
	getAllErr error
	seenErr   error
	cycleErr  error
}

func newMockWatchStore(watches ...*watcher.Watch) *mockWatchStore {
	store := &mockWatchStore{
		watches:  make(map[string]*watcher.Watch, len(watches)),
		marked:   make(map[string]map[string]watcher.SearchResult),
		statuses: make(map[string]string),
	}
	for _, w := range watches {
		store.watches[w.ID] = w
		store.order = append(store.order, w.ID)
	}

	return store
}

func (m *mockWatchStore) Create(w *watcher.Watch) error {
	if m.createErr != nil {
		return m.createErr
	}

	stored := *w
	stored.Rev = 1
	m.watches[w.ID] = &stored
	m.order = append(m.order, w.ID)

	return nil
}

func (m *mockWatchStore) Update(w *watcher.Watch) error {
	if m.updateErr != nil {
		return m.updateErr
	}

	current, ok := m.watches[w.ID]
	if !ok {
		return fmt.Errorf("watch %s: %w", w.ID, watch_store.ErrNotFound)
	}

	// the repository's UPDATE names the request's columns only, so the run lifecycle survives
	// it — the mock has to carry it over or a revive would look reset by accident
	stored := *w
	stored.Rev = current.Rev + 1
	stored.SeededAt = current.SeededAt
	stored.DisabledAt = current.DisabledAt
	stored.LastRunAt = current.LastRunAt
	stored.LastStatus = current.LastStatus
	m.watches[w.ID] = &stored

	return nil
}

func (m *mockWatchStore) Disable(id string) error {
	w, ok := m.watches[id]
	if !ok {
		return fmt.Errorf("watch %s: %w", id, watch_store.ErrNotFound)
	}

	at := time.Now()
	w.DisabledAt = &at
	m.disabled = append(m.disabled, id)

	return nil
}

func (m *mockWatchStore) Revive(id string) error {
	w, ok := m.watches[id]
	if !ok {
		return fmt.Errorf("watch %s: %w", id, watch_store.ErrNotFound)
	}

	w.DisabledAt = nil
	w.SeededAt = nil
	w.LastRunAt = nil
	w.LastStatus = ""
	m.revived = append(m.revived, id)

	return nil
}

func (m *mockWatchStore) GetAll() ([]*watcher.Watch, error) {
	if m.getAllErr != nil {
		return nil, m.getAllErr
	}

	watches := make([]*watcher.Watch, 0, len(m.order))
	for _, id := range m.order {
		watches = append(watches, m.watches[id])
	}

	return watches, nil
}

func (m *mockWatchStore) GetByID(id string) (*watcher.Watch, error) {
	w, ok := m.watches[id]
	if !ok {
		return nil, fmt.Errorf("watch %s: %w", id, watch_store.ErrNotFound)
	}

	return w, nil
}

func (m *mockWatchStore) SeenRows(watchID string) ([]watch_store.SeenRow, error) {
	return m.seen, m.seenErr
}

// WatchesForCycle excludes soft-deleted rows, mirroring the repository: it is what the
// search endpoints load through, so a disabled watch is a 404 there.
func (m *mockWatchStore) WatchesForCycle() ([]*watcher.Watch, error) {
	if m.cycleErr != nil {
		return nil, m.cycleErr
	}

	watches := make([]*watcher.Watch, 0, len(m.order))
	for _, id := range m.order {
		if m.watches[id].DisabledAt != nil {
			continue
		}

		watches = append(watches, m.watches[id])
	}

	return watches, nil
}

func (m *mockWatchStore) SeenKeys(watchID string) (map[string]struct{}, error) {
	keys := make(map[string]struct{}, len(m.marked[watchID]))
	for key := range m.marked[watchID] {
		keys[key] = struct{}{}
	}

	return keys, nil
}

func (m *mockWatchStore) MarkSeen(watchID string, results []watcher.SearchResult) error {
	if m.marked[watchID] == nil {
		m.marked[watchID] = make(map[string]watcher.SearchResult)
	}
	for _, result := range results {
		m.marked[watchID][result.SeenKey()] = result
	}

	return nil
}

func (m *mockWatchStore) MarkSeeded(id string) error {
	if w, ok := m.watches[id]; ok {
		now := time.Now()
		w.SeededAt = &now
	}

	return nil
}

func (m *mockWatchStore) RecordRun(watchID, status string) error {
	m.statuses[watchID] = status

	return nil
}

func acceptanceWatch() *watcher.Watch {
	return &watcher.Watch{
		ID:           "one-night-only-en",
		Queries:      []string{"One Night Only 2026", "Только на одну ночь 2026"},
		Sources:      []string{"jackett", "extto"},
		IncludeRegex: `(?i)one[ ._-]night[ ._-]only.*2026|только[ ._-]на[ ._-]одну[ ._-]ночь.*2026`,
		ExcludeRegex: `(?i)bee gees|def leppard|one desire|streisand|rupaul|top chef|concert|chinese|\b2016\b`,
		Rev:          1,
	}
}

func newWatchClient(store watchStore) *Client {
	return NewClient(&ClientCtx{
		Store:          &mockFileStore{},
		TaskCreator:    &mockTaskCreator{},
		DownloadClient: &mockDownloadClient{},
		WatchStore:     store,
	})
}

func newWatchRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")

	return req
}

// watchByID builds a request against a single watch, including the path value the router
// would have extracted.
func watchByID(method, id, body string) *http.Request {
	req := newWatchRequest(method, "/api/watches/"+id, body)
	req.SetPathValue("watchId", id)

	return req
}

func decodeWatch(t *testing.T, w *httptest.ResponseRecorder) watchResponse {
	t.Helper()

	var resp watchResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	return resp
}

func TestCreateWatch(t *testing.T) {
	store := newMockWatchStore()

	body := `{"id":"one-night-only-en","queries":["One Night Only 2026","Только на одну ночь 2026"],
		"sources":["jackett","extto"],"include_regex":"(?i)one[ ._-]night[ ._-]only.*2026","exclude_regex":"(?i)bee gees"}`
	w := httptest.NewRecorder()

	newWatchClient(store).handleCreateWatch(w, newWatchRequest(http.MethodPost, "/api/watches", body))

	assert.Equal(t, http.StatusCreated, w.Code)

	resp := decodeWatch(t, w)
	assert.Equal(t, "one-night-only-en", resp.ID)
	assert.Equal(t, []string{"One Night Only 2026", "Только на одну ночь 2026"}, resp.Queries)
	assert.Equal(t, []string{"jackett", "extto"}, resp.Sources)
	assert.Equal(t, "(?i)one[ ._-]night[ ._-]only.*2026", resp.IncludeRegex)
	assert.Equal(t, 1, resp.Rev)
	assert.Nil(t, resp.SeededAt)

	require.Contains(t, store.watches, "one-night-only-en")
}

// an omitted sources list is the schema default, not an error: a watch without sources would
// silently never search anything
func TestCreateWatchDefaultsSources(t *testing.T) {
	store := newMockWatchStore()
	w := httptest.NewRecorder()

	newWatchClient(store).handleCreateWatch(w, newWatchRequest(http.MethodPost, "/api/watches", `{"id":"anime-dub","queries":["some anime"]}`))

	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, watcher.KnownSources(), decodeWatch(t, w).Sources)
}

func TestCreateWatchValidation(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		contains string
	}{
		{
			name:     "bad id charset",
			body:     `{"id":"One Night.Only","queries":["x"]}`,
			contains: "id must match",
		},
		{
			name:     "no queries",
			body:     `{"id":"ok-id","queries":[]}`,
			contains: "non-empty query",
		},
		{
			name:     "blank queries only",
			body:     `{"id":"ok-id","queries":["   "]}`,
			contains: "non-empty query",
		},
		{
			name:     "invalid include regex",
			body:     `{"id":"ok-id","queries":["x"],"include_regex":"(unclosed"}`,
			contains: "invalid include_regex",
		},
		{
			name:     "invalid exclude regex",
			body:     `{"id":"ok-id","queries":["x"],"exclude_regex":"["}`,
			contains: "invalid exclude_regex",
		},
		{
			name:     "unknown source",
			body:     `{"id":"ok-id","queries":["x"],"sources":["rutracker"]}`,
			contains: `unknown source "rutracker"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newMockWatchStore()
			w := httptest.NewRecorder()

			newWatchClient(store).handleCreateWatch(w, newWatchRequest(http.MethodPost, "/api/watches", tt.body))

			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Contains(t, w.Body.String(), tt.contains)
			assert.Empty(t, store.watches)
		})
	}
}

func TestCreateWatchDuplicateID(t *testing.T) {
	store := newMockWatchStore(acceptanceWatch())
	w := httptest.NewRecorder()

	body := `{"id":"one-night-only-en","queries":["One Night Only 2026"]}`
	newWatchClient(store).handleCreateWatch(w, newWatchRequest(http.MethodPost, "/api/watches", body))

	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Len(t, store.order, 1)
}

func TestCreateWatchInvalidBody(t *testing.T) {
	w := httptest.NewRecorder()

	newWatchClient(newMockWatchStore()).handleCreateWatch(w, newWatchRequest(http.MethodPost, "/api/watches", "not json"))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateWatchStoreFailure(t *testing.T) {
	store := newMockWatchStore()
	store.createErr = errors.New("db is down")
	w := httptest.NewRecorder()

	newWatchClient(store).handleCreateWatch(w, newWatchRequest(http.MethodPost, "/api/watches", `{"id":"ok-id","queries":["x"]}`))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestListWatches(t *testing.T) {
	lastRun := time.Now().Add(-20 * time.Minute).UTC()
	seeded := time.Now().Add(-2 * time.Hour).UTC()

	first := acceptanceWatch()
	first.SeededAt = &seeded
	first.LastRunAt = &lastRun
	first.LastStatus = `jackett "One Night Only 2026": 502`
	first.Rev = 3

	second := &watcher.Watch{ID: "house-of-the-dragon", Queries: []string{"House of the Dragon"}, Sources: []string{"jackett"}, Rev: 1}

	store := newMockWatchStore(first, second)
	w := httptest.NewRecorder()

	newWatchClient(store).handleWatches(w, httptest.NewRequest(http.MethodGet, "/api/watches", nil))

	assert.Equal(t, http.StatusOK, w.Code)

	var resp []watchResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp, 2)

	assert.Equal(t, "one-night-only-en", resp[0].ID)
	assert.Equal(t, 3, resp[0].Rev)
	assert.Equal(t, `jackett "One Night Only 2026": 502`, resp[0].LastStatus)
	require.NotNil(t, resp[0].LastRunAt)
	require.NotNil(t, resp[0].SeededAt)

	assert.Equal(t, "house-of-the-dragon", resp[1].ID)
	assert.Nil(t, resp[1].LastRunAt)
	assert.Empty(t, resp[1].Seen)
}

func TestListWatchesStoreFailure(t *testing.T) {
	store := newMockWatchStore()
	store.getAllErr = errors.New("db is down")
	w := httptest.NewRecorder()

	newWatchClient(store).handleWatches(w, httptest.NewRequest(http.MethodGet, "/api/watches", nil))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// a silent seed announces nothing, so the rows it recorded are the only way to tell it apart
// from a watch that is quietly broken
func TestGetWatchIncludesSeenRows(t *testing.T) {
	firstSeen := time.Now().Add(-time.Hour).UTC()
	store := newMockWatchStore(acceptanceWatch())
	store.seen = []watch_store.SeenRow{
		{Source: "jackett", ExternalID: "1883913", Title: "Только на одну ночь / One Night Only (2026) TSRip", FirstSeenAt: firstSeen},
		{Source: "extto", ExternalID: "20151803", Title: "One.Night.Only.2026.1080p.WEB-DL", FirstSeenAt: firstSeen},
	}
	w := httptest.NewRecorder()

	newWatchClient(store).handleWatch(w, watchByID(http.MethodGet, "one-night-only-en", ""))

	assert.Equal(t, http.StatusOK, w.Code)

	resp := decodeWatch(t, w)
	assert.Equal(t, "one-night-only-en", resp.ID)
	require.Len(t, resp.Seen, 2)
	assert.Equal(t, "jackett", resp.Seen[0].Source)
	assert.Equal(t, "1883913", resp.Seen[0].ID)
	assert.Equal(t, "extto", resp.Seen[1].Source)
	assert.Equal(t, "20151803", resp.Seen[1].ID)
}

func TestGetWatchNotFound(t *testing.T) {
	w := httptest.NewRecorder()

	newWatchClient(newMockWatchStore()).handleWatch(w, watchByID(http.MethodGet, "missing", ""))

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGetWatchSeenRowsFailure(t *testing.T) {
	store := newMockWatchStore(acceptanceWatch())
	store.seenErr = errors.New("db is down")
	w := httptest.NewRecorder()

	newWatchClient(store).handleWatch(w, watchByID(http.MethodGet, "one-night-only-en", ""))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestUpdateWatchBumpsRev(t *testing.T) {
	store := newMockWatchStore(acceptanceWatch())
	w := httptest.NewRecorder()

	body := `{"queries":["One Night Only 2026"],"exclude_regex":"(?i)bee gees|rupaul"}`
	newWatchClient(store).handleUpdateWatch(w, watchByID(http.MethodPatch, "one-night-only-en", body))

	assert.Equal(t, http.StatusOK, w.Code)

	resp := decodeWatch(t, w)
	assert.Equal(t, 2, resp.Rev)
	assert.Equal(t, []string{"One Night Only 2026"}, resp.Queries)
	assert.Equal(t, "(?i)bee gees|rupaul", resp.ExcludeRegex)
	// an omitted field keeps its stored value rather than being reset
	assert.Equal(t, acceptanceWatch().IncludeRegex, resp.IncludeRegex)
	assert.Equal(t, []string{"jackett", "extto"}, resp.Sources)
}

func TestUpdateWatchNotFound(t *testing.T) {
	w := httptest.NewRecorder()

	newWatchClient(newMockWatchStore()).handleUpdateWatch(w, watchByID(http.MethodPatch, "missing", `{"queries":["x"]}`))

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUpdateWatchValidation(t *testing.T) {
	store := newMockWatchStore(acceptanceWatch())
	w := httptest.NewRecorder()

	newWatchClient(store).handleUpdateWatch(w, watchByID(http.MethodPatch, "one-night-only-en", `{"include_regex":"(unclosed"}`))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid include_regex")
	assert.Equal(t, 1, store.watches["one-night-only-en"].Rev)
}

func TestUpdateWatchStoreFailure(t *testing.T) {
	store := newMockWatchStore(acceptanceWatch())
	store.updateErr = errors.New("db is down")
	w := httptest.NewRecorder()

	newWatchClient(store).handleUpdateWatch(w, watchByID(http.MethodPatch, "one-night-only-en", `{"queries":["x"]}`))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestRemoveWatchSoftDeletes(t *testing.T) {
	store := newMockWatchStore(acceptanceWatch())
	w := httptest.NewRecorder()

	newWatchClient(store).handleRemoveWatch(w, watchByID(http.MethodDelete, "one-night-only-en", ""))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []string{"one-night-only-en"}, store.disabled)
	// soft delete: the row and its seen set survive
	assert.Contains(t, store.watches, "one-night-only-en")
}

func TestRemoveWatchNotFound(t *testing.T) {
	w := httptest.NewRecorder()

	newWatchClient(newMockWatchStore()).handleRemoveWatch(w, watchByID(http.MethodDelete, "missing", ""))

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// the routes are registered unconditionally, so an unconfigured store must answer rather
// than panic
func TestWatchHandlersWithoutStore(t *testing.T) {
	c := newWatchClient(nil)

	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"create", c.handleCreateWatch},
		{"list", c.handleWatches},
		{"get", c.handleWatch},
		{"update", c.handleUpdateWatch},
		{"remove", c.handleRemoveWatch},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()

			tt.handler(w, newWatchRequest(http.MethodPost, "/api/watches", "{}"))

			assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		})
	}
}

// scriptedSource answers every query with the same rows, so a test can assert on filtering
// and on the delta without depending on which query produced what.
type scriptedSource struct {
	name    string
	results []watcher.SearchResult
	err     error
	calls   int
}

func (s *scriptedSource) Name() string { return s.name }

func (s *scriptedSource) Search(_ context.Context, query string) ([]watcher.SearchResult, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}

	results := make([]watcher.SearchResult, 0, len(s.results))
	for _, result := range s.results {
		result.Query = query
		results = append(results, result)
	}

	return results, nil
}

type mockPublisher struct {
	published []watcher.RunOutcome
}

func (p *mockPublisher) Publish(_ context.Context, _ watcher.Watch, o watcher.RunOutcome) error {
	p.published = append(p.published, o)

	return nil
}

type mockMagnets struct {
	magnets map[string]string
	err     error
	queries []string
}

func (m *mockMagnets) Magnet(_ context.Context, torrentID, query string) (string, error) {
	m.queries = append(m.queries, query)
	if m.err != nil {
		return "", m.err
	}

	magnet, ok := m.magnets[torrentID]
	if !ok {
		return "", fmt.Errorf("no magnet for %s", torrentID)
	}

	return magnet, nil
}

// acceptanceTitles is the measured junk set from the plan's Acceptance watch block: one
// EN-shaped release and the four titles the 226-result page was full of.
var acceptanceTitles = []string{
	"One.Night.Only.2026.1080p.WEB-DL.DDP5.1.H264-GROUP",
	"Bee Gees One Night Only 1998 WEBRip 1080p x264 AAC ENG Lulloz",
	"Def Leppard - One Night Only: Live At The Leadmill [2024, Classic Rock, Hard Rock, Blu-ray, 1080i]",
	"RuPauls Drag Race S15E02 One Night Only Part 2 1080p AMZN WEB DL DDP2 0 H 264 FLUX TGx",
	"One Night Only / Tian Liang Zhi Qian [2016, BDRemux 1080p] VO + DVO + Sub Rus, Eng + Original Chi",
}

func searchResults(source string, titles ...string) []watcher.SearchResult {
	results := make([]watcher.SearchResult, 0, len(titles))
	for i, title := range titles {
		id := fmt.Sprintf("188391%d", i)
		results = append(results, watcher.SearchResult{
			Source:      source,
			ExternalID:  id,
			Title:       title,
			PageURL:     "https://nnmclub.to/forum/viewtopic.php?t=" + id,
			DownloadURL: "https://jackett.example.com/dl/nnm?file=" + id,
			Seeders:     10 + i,
			PublishedAt: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
		})
	}

	return results
}

func jackettWatch() *watcher.Watch {
	w := acceptanceWatch()
	w.Sources = []string{"jackett"}

	return w
}

func newSearchEngine(store *mockWatchStore, source watcher.SearchSource, pub *mockPublisher) *watcher.Engine {
	return watcher.NewEngine(watcher.EngineDeps{
		Sources:   []watcher.SearchSource{source},
		Store:     store,
		Publisher: pub,
	})
}

func newSearchClient(store watchStore, engine searchEngine, magnets magnetResolver) *Client {
	return NewClient(&ClientCtx{
		Store:          &mockFileStore{},
		TaskCreator:    &mockTaskCreator{},
		DownloadClient: &mockDownloadClient{},
		WatchStore:     store,
		Engine:         engine,
		Magnets:        magnets,
	})
}

// searchByID builds a search request against one watch, including the path value the router
// would have extracted.
func searchByID(id, query string) *http.Request {
	req := newWatchRequest(http.MethodPost, "/api/watches/"+id+"/search"+query, "")
	req.SetPathValue("watchId", id)

	return req
}

func decodeSearch(t *testing.T, w *httptest.ResponseRecorder) searchResponse {
	t.Helper()

	var resp searchResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	return resp
}

func TestWatchSearchAppliesStoredRegexes(t *testing.T) {
	store := newMockWatchStore(jackettWatch())
	source := &scriptedSource{name: "jackett", results: searchResults("jackett", acceptanceTitles...)}
	w := httptest.NewRecorder()

	newSearchClient(store, newSearchEngine(store, source, &mockPublisher{}), nil).
		handleWatchSearch(w, searchByID("one-night-only-en", ""))

	assert.Equal(t, http.StatusOK, w.Code)

	resp := decodeSearch(t, w)
	assert.Equal(t, "one-night-only-en", resp.WatchID)
	assert.Equal(t, 5, resp.Total)
	assert.Equal(t, 1, resp.Matched)
	assert.False(t, resp.Raw)
	assert.Empty(t, resp.Errors)

	require.Len(t, resp.Items, 1)
	assert.Equal(t, acceptanceTitles[0], resp.Items[0].Title)
	assert.Equal(t, "1883910", resp.Items[0].ID)
	assert.Equal(t, "https://nnmclub.to/forum/viewtopic.php?t=1883910", resp.Items[0].PageURL)
	assert.Equal(t, "https://jackett.example.com/dl/nnm?file=1883910", resp.Items[0].DownloadURL)
	assert.Equal(t, 10, resp.Items[0].Seeders)
	assert.True(t, resp.Items[0].New)
	// the watch runs two queries against the one source it names
	assert.Equal(t, 2, source.calls)
}

// the pre-filter set is the only way to answer "why was I woken with this junk" and "why was
// I not woken", so it stays reachable
func TestWatchSearchRawReturnsUnfiltered(t *testing.T) {
	store := newMockWatchStore(jackettWatch())
	source := &scriptedSource{name: "jackett", results: searchResults("jackett", acceptanceTitles...)}
	w := httptest.NewRecorder()

	newSearchClient(store, newSearchEngine(store, source, &mockPublisher{}), nil).
		handleWatchSearch(w, searchByID("one-night-only-en", "?raw=true"))

	assert.Equal(t, http.StatusOK, w.Code)

	resp := decodeSearch(t, w)
	assert.True(t, resp.Raw)
	assert.Equal(t, 5, resp.Total)
	assert.Equal(t, 1, resp.Matched)
	require.Len(t, resp.Items, 5)

	assert.True(t, resp.Items[0].New)
	for _, item := range resp.Items[1:] {
		assert.False(t, item.New, item.Title)
	}
}

// the key set is a contract with an agent in another repository, so it is asserted rather
// than left to the struct tags
func TestWatchSearchItemKeys(t *testing.T) {
	store := newMockWatchStore(jackettWatch())
	source := &scriptedSource{name: "jackett", results: searchResults("jackett", acceptanceTitles[0])}
	w := httptest.NewRecorder()

	newSearchClient(store, newSearchEngine(store, source, &mockPublisher{}), nil).
		handleWatchSearch(w, searchByID("one-night-only-en", ""))

	var decoded struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &decoded))
	require.Len(t, decoded.Items, 1)

	keys := make([]string, 0, len(decoded.Items[0]))
	for key := range decoded.Items[0] {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	assert.Equal(t, []string{
		"download_url", "id", "magnet", "magnet_error", "new", "page_url", "published_at", "seeders", "source", "title",
	}, keys)
}

func TestWatchSearchFlagsSeenItems(t *testing.T) {
	store := newMockWatchStore(jackettWatch())
	results := searchResults("jackett", acceptanceTitles...)
	require.NoError(t, store.MarkSeen("one-night-only-en", results[:1]))

	source := &scriptedSource{name: "jackett", results: results}
	w := httptest.NewRecorder()

	newSearchClient(store, newSearchEngine(store, source, &mockPublisher{}), nil).
		handleWatchSearch(w, searchByID("one-night-only-en", ""))

	resp := decodeSearch(t, w)
	require.Len(t, resp.Items, 1)
	assert.False(t, resp.Items[0].New)
}

// TestCronAndEndpointAgree backs the claim that the reproduction endpoint cannot drift from
// what woke the agent: both paths go through Evaluate over the same stored parameters.
func TestCronAndEndpointAgree(t *testing.T) {
	watch := jackettWatch()
	seeded := time.Now().Add(-2 * time.Hour)
	watch.SeededAt = &seeded

	store := newMockWatchStore(watch)
	source := &scriptedSource{name: "jackett", results: searchResults("jackett", acceptanceTitles...)}
	pub := &mockPublisher{}
	engine := newSearchEngine(store, source, pub)

	require.NoError(t, engine.RunCycle(context.Background()))
	require.Len(t, pub.published, 1)

	w := httptest.NewRecorder()
	newSearchClient(store, engine, nil).handleWatchSearch(w, searchByID("one-night-only-en", ""))
	assert.Equal(t, http.StatusOK, w.Code)

	type projection struct {
		Source string
		ID     string
		Title  string
	}

	fromCron := make([]projection, 0, len(pub.published[0].Matched))
	for _, result := range pub.published[0].Matched {
		fromCron = append(fromCron, projection{Source: result.Source, ID: result.ExternalID, Title: result.Title})
	}

	resp := decodeSearch(t, w)
	fromEndpoint := make([]projection, 0, len(resp.Items))
	for _, item := range resp.Items {
		fromEndpoint = append(fromEndpoint, projection{Source: item.Source, ID: item.ID, Title: item.Title})
	}

	require.NotEmpty(t, fromCron)
	assert.True(t, reflect.DeepEqual(fromCron, fromEndpoint), "cron %+v endpoint %+v", fromCron, fromEndpoint)
	// the cycle published and marked, so the same item is no longer new to the endpoint
	require.Len(t, resp.Items, 1)
	assert.False(t, resp.Items[0].New)
}

// a partial run is still answered — withholding what one working source found is the silent
// failure this whole design exists to prevent
func TestWatchSearchReportsSourceError(t *testing.T) {
	store := newMockWatchStore(jackettWatch())
	source := &scriptedSource{name: "jackett", err: errors.New("jackett returned 502")}
	w := httptest.NewRecorder()

	newSearchClient(store, newSearchEngine(store, source, &mockPublisher{}), nil).
		handleWatchSearch(w, searchByID("one-night-only-en", ""))

	assert.Equal(t, http.StatusOK, w.Code)

	resp := decodeSearch(t, w)
	require.Len(t, resp.Errors, 1)
	assert.Contains(t, resp.Errors[0], "jackett returned 502")
	assert.Empty(t, resp.Items)
	assert.Equal(t, 0, resp.Total)
}

func TestWatchSearchUnknownWatch(t *testing.T) {
	store := newMockWatchStore()
	w := httptest.NewRecorder()

	newSearchClient(store, newSearchEngine(store, &scriptedSource{name: "jackett"}, &mockPublisher{}), nil).
		handleWatchSearch(w, searchByID("missing", ""))

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// a soft-deleted watch is gone as far as the API is concerned, even though its row and its
// seen set survive
func TestWatchSearchDisabledWatch(t *testing.T) {
	store := newMockWatchStore(jackettWatch())
	require.NoError(t, store.Disable("one-night-only-en"))

	source := &scriptedSource{name: "jackett", results: searchResults("jackett", acceptanceTitles...)}
	w := httptest.NewRecorder()

	newSearchClient(store, newSearchEngine(store, source, &mockPublisher{}), nil).
		handleWatchSearch(w, searchByID("one-night-only-en", ""))

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, 0, source.calls)
}

func TestWatchSearchStoreFailure(t *testing.T) {
	store := newMockWatchStore(jackettWatch())
	store.cycleErr = errors.New("db is down")
	w := httptest.NewRecorder()

	newSearchClient(store, newSearchEngine(store, &scriptedSource{name: "jackett"}, &mockPublisher{}), nil).
		handleWatchSearch(w, searchByID("one-night-only-en", ""))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func exttoWatch() *watcher.Watch {
	return &watcher.Watch{
		ID:      "extto-only",
		Queries: []string{"One Night Only 2026"},
		Sources: []string{watcher.SourceExtto},
		Rev:     1,
	}
}

func TestWatchSearchResolvesExttoMagnets(t *testing.T) {
	store := newMockWatchStore(exttoWatch())
	results := searchResults(watcher.SourceExtto, "One.Night.Only.2026.1080p.WEB-DL", "Dune.Prophecy.S01.2160p")
	source := &scriptedSource{name: watcher.SourceExtto, results: results}
	magnets := &mockMagnets{magnets: map[string]string{"1883910": "magnet:?xt=urn:btih:abc123"}}
	w := httptest.NewRecorder()

	newSearchClient(store, newSearchEngine(store, source, &mockPublisher{}), magnets).
		handleWatchSearch(w, searchByID("extto-only", ""))

	assert.Equal(t, http.StatusOK, w.Code)

	resp := decodeSearch(t, w)
	require.Len(t, resp.Items, 2)
	assert.Equal(t, "magnet:?xt=urn:btih:abc123", resp.Items[0].Magnet)
	assert.Empty(t, resp.Items[0].MagnetError)

	// a per-item failure is reported on the item, never as a failed request
	assert.Empty(t, resp.Items[1].Magnet)
	assert.Contains(t, resp.Items[1].MagnetError, "no magnet for 1883911")

	// the signature needs the tokens of the query that produced the row
	assert.Equal(t, []string{"One Night Only 2026", "One Night Only 2026"}, magnets.queries)
}

func TestWatchSearchWithoutMagnetResolver(t *testing.T) {
	store := newMockWatchStore(exttoWatch())
	source := &scriptedSource{name: watcher.SourceExtto, results: searchResults(watcher.SourceExtto, "One.Night.Only.2026.1080p.WEB-DL")}
	w := httptest.NewRecorder()

	newSearchClient(store, newSearchEngine(store, source, &mockPublisher{}), nil).
		handleWatchSearch(w, searchByID("extto-only", ""))

	assert.Equal(t, http.StatusOK, w.Code)

	resp := decodeSearch(t, w)
	require.Len(t, resp.Items, 1)
	assert.Empty(t, resp.Items[0].Magnet)
	assert.Equal(t, magnetUnavailable, resp.Items[0].MagnetError)
}

// a jackett row carries a usable download url, so no magnet is fetched for it — the keys are
// still present and empty
func TestWatchSearchSkipsMagnetForJackett(t *testing.T) {
	store := newMockWatchStore(jackettWatch())
	source := &scriptedSource{name: "jackett", results: searchResults("jackett", acceptanceTitles[0])}
	magnets := &mockMagnets{magnets: map[string]string{"1883910": "magnet:?xt=urn:btih:abc123"}}
	w := httptest.NewRecorder()

	newSearchClient(store, newSearchEngine(store, source, &mockPublisher{}), magnets).
		handleWatchSearch(w, searchByID("one-night-only-en", ""))

	resp := decodeSearch(t, w)
	require.Len(t, resp.Items, 1)
	assert.Empty(t, resp.Items[0].Magnet)
	assert.Empty(t, resp.Items[0].MagnetError)
	assert.Empty(t, magnets.queries)
}

// an ad-hoc search has no id and therefore no seen set: everything it matched is new
func TestAdHocSearch(t *testing.T) {
	store := newMockWatchStore(jackettWatch())
	require.NoError(t, store.MarkSeen("one-night-only-en", searchResults("jackett", acceptanceTitles[0])))

	source := &scriptedSource{name: "jackett", results: searchResults("jackett", acceptanceTitles...)}
	w := httptest.NewRecorder()

	body := `{"queries":["One Night Only 2026"],"sources":["jackett"],
		"include_regex":"(?i)one[ ._-]night[ ._-]only.*2026","exclude_regex":"(?i)bee gees"}`
	newSearchClient(store, newSearchEngine(store, source, &mockPublisher{}), nil).
		handleSearch(w, newWatchRequest(http.MethodPost, "/api/search", body))

	assert.Equal(t, http.StatusOK, w.Code)

	resp := decodeSearch(t, w)
	assert.Empty(t, resp.WatchID)
	assert.Equal(t, 5, resp.Total)
	assert.Equal(t, 1, resp.Matched)
	require.Len(t, resp.Items, 1)
	assert.True(t, resp.Items[0].New)
	assert.Equal(t, 1, source.calls)
}

func TestAdHocSearchDefaultsSources(t *testing.T) {
	store := newMockWatchStore()
	source := &scriptedSource{name: "jackett", results: searchResults("jackett", acceptanceTitles[0])}
	w := httptest.NewRecorder()

	newSearchClient(store, newSearchEngine(store, source, &mockPublisher{}), nil).
		handleSearch(w, newWatchRequest(http.MethodPost, "/api/search", `{"queries":["One Night Only 2026"]}`))

	assert.Equal(t, http.StatusOK, w.Code)

	// the unconfigured ext.to source of the default set reports an error rather than silence
	resp := decodeSearch(t, w)
	assert.Equal(t, 1, resp.Total)
	require.Len(t, resp.Errors, 1)
	assert.Contains(t, resp.Errors[0], "source is not configured")
}

func TestAdHocSearchValidation(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		contains string
	}{
		{name: "no queries", body: `{"queries":[]}`, contains: "non-empty query"},
		{name: "invalid include regex", body: `{"queries":["x"],"include_regex":"(unclosed"}`, contains: "invalid include_regex"},
		{name: "invalid exclude regex", body: `{"queries":["x"],"exclude_regex":"["}`, contains: "invalid exclude_regex"},
		{name: "unknown source", body: `{"queries":["x"],"sources":["rutracker"]}`, contains: `unknown source "rutracker"`},
		{name: "invalid body", body: "not json", contains: "invalid request body"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newMockWatchStore()
			source := &scriptedSource{name: "jackett"}
			w := httptest.NewRecorder()

			newSearchClient(store, newSearchEngine(store, source, &mockPublisher{}), nil).
				handleSearch(w, newWatchRequest(http.MethodPost, "/api/search", tt.body))

			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Contains(t, w.Body.String(), tt.contains)
			assert.Equal(t, 0, source.calls)
		})
	}
}

// the routes are registered unconditionally, so an unconfigured engine must answer rather
// than panic
func TestSearchHandlersWithoutEngine(t *testing.T) {
	c := newSearchClient(newMockWatchStore(jackettWatch()), nil, nil)

	tests := []struct {
		name    string
		handler http.HandlerFunc
		request *http.Request
	}{
		{"watch search", c.handleWatchSearch, searchByID("one-night-only-en", "")},
		{"ad-hoc search", c.handleSearch, newWatchRequest(http.MethodPost, "/api/search", `{"queries":["x"]}`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()

			tt.handler(w, tt.request)

			assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		})
	}
}

// DELETE is a soft delete, so the row survives and a plain existence check would retire the
// id for good — the operator could never re-create the watch they just removed
func TestCreateWatchRevivesASoftDeletedID(t *testing.T) {
	store := newMockWatchStore(acceptanceWatch())
	// what the dead watch left behind: a seed, a run and the error status that run recorded
	seeded := time.Now().Add(-72 * time.Hour)
	store.watches["one-night-only-en"].SeededAt = &seeded
	store.watches["one-night-only-en"].LastRunAt = &seeded
	store.watches["one-night-only-en"].LastStatus = `jackett "One Night Only 2026": 502`
	require.NoError(t, store.Disable("one-night-only-en"))

	w := httptest.NewRecorder()
	body := `{"id":"one-night-only-en","queries":["One Night Only 2026"],"sources":["jackett"]}`
	newWatchClient(store).handleCreateWatch(w, newWatchRequest(http.MethodPost, "/api/watches", body))

	require.Equal(t, http.StatusCreated, w.Code)

	resp := decodeWatch(t, w)
	assert.Nil(t, resp.DisabledAt)
	assert.Equal(t, []string{"One Night Only 2026"}, resp.Queries)
	// a re-create is a create: it seeds silently again, and /api/health must not read the
	// dead watch's status and stale run as this one's
	assert.Nil(t, resp.SeededAt)
	assert.Nil(t, resp.LastRunAt)
	assert.Empty(t, resp.LastStatus)
	assert.Equal(t, []string{"one-night-only-en"}, store.revived)
	assert.Len(t, store.order, 1, "the revived watch is the same row, not a second one")

	// and the cron picks it up again
	forCycle, err := store.WatchesForCycle()
	require.NoError(t, err)
	assert.Len(t, forCycle, 1)
}

// a watch the cron has stopped running must not read as a healthy one that finds nothing
func TestWatchResponseReportsTheSoftDelete(t *testing.T) {
	store := newMockWatchStore(acceptanceWatch())
	require.NoError(t, store.Disable("one-night-only-en"))

	w := httptest.NewRecorder()
	newWatchClient(store).handleWatches(w, newWatchRequest(http.MethodGet, "/api/watches", ""))

	require.Equal(t, http.StatusOK, w.Code)

	var resp []watchResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.NotNil(t, resp[0].DisabledAt)
}

func TestCreateWatchRoundTripsExpiresAt(t *testing.T) {
	store := newMockWatchStore()
	w := httptest.NewRecorder()

	body := `{"id":"ok-id","queries":["x"],"sources":["jackett"],"expires_at":"2026-12-31T23:00:00Z"}`
	newWatchClient(store).handleCreateWatch(w, newWatchRequest(http.MethodPost, "/api/watches", body))

	require.Equal(t, http.StatusCreated, w.Code)

	resp := decodeWatch(t, w)
	require.NotNil(t, resp.ExpiresAt)
	assert.Equal(t, time.Date(2026, time.December, 31, 23, 0, 0, 0, time.UTC), resp.ExpiresAt.UTC())
}

// a nil expires_at means "absent", not "clear it": a PATCH of the queries must not silently
// un-expire the watch
func TestUpdateWatchKeepsAnOmittedExpiry(t *testing.T) {
	stored := acceptanceWatch()
	expires := time.Date(2026, time.December, 31, 23, 0, 0, 0, time.UTC)
	stored.ExpiresAt = &expires
	store := newMockWatchStore(stored)
	w := httptest.NewRecorder()

	newWatchClient(store).handleUpdateWatch(w, watchByID(http.MethodPatch, "one-night-only-en", `{"queries":["x"]}`))

	require.Equal(t, http.StatusOK, w.Code)

	resp := decodeWatch(t, w)
	require.NotNil(t, resp.ExpiresAt)
	assert.Equal(t, expires, resp.ExpiresAt.UTC())
}

// a typed nil in the interface field passes the handler's `== nil` check, so the resolver
// itself has to answer rather than panic
func TestWatchSearchWithATypedNilMagnetResolver(t *testing.T) {
	store := newMockWatchStore(exttoWatch())
	source := &scriptedSource{name: watcher.SourceExtto, results: searchResults(watcher.SourceExtto, "One.Night.Only.2026.1080p.WEB-DL")}
	w := httptest.NewRecorder()

	newSearchClient(store, newSearchEngine(store, source, &mockPublisher{}), (*watcher.ExttoSource)(nil)).
		handleWatchSearch(w, searchByID("extto-only", ""))

	require.Equal(t, http.StatusOK, w.Code)

	resp := decodeSearch(t, w)
	require.Len(t, resp.Items, 1)
	assert.Empty(t, resp.Items[0].Magnet)
	assert.Contains(t, resp.Items[0].MagnetError, "flaresolverr is not configured")
}

// the raw view exists to show what the regexes threw away; paying a signed ext.to round trip
// for every discarded row would hammer the site for rows nobody will download
func TestWatchSearchRawResolvesMagnetsForMatchedRowsOnly(t *testing.T) {
	watch := exttoWatch()
	watch.IncludeRegex = `(?i)one[ ._-]night[ ._-]only`
	store := newMockWatchStore(watch)
	results := searchResults(watcher.SourceExtto, "One.Night.Only.2026.1080p.WEB-DL", "Dune.Prophecy.S01.2160p")
	source := &scriptedSource{name: watcher.SourceExtto, results: results}
	magnets := &mockMagnets{magnets: map[string]string{"1883910": "magnet:?xt=urn:btih:abc123"}}
	w := httptest.NewRecorder()

	newSearchClient(store, newSearchEngine(store, source, &mockPublisher{}), magnets).
		handleWatchSearch(w, searchByID("extto-only", "?raw=true"))

	require.Equal(t, http.StatusOK, w.Code)

	resp := decodeSearch(t, w)
	require.Len(t, resp.Items, 2)
	assert.Equal(t, "magnet:?xt=urn:btih:abc123", resp.Items[0].Magnet)
	assert.Empty(t, resp.Items[1].Magnet)
	assert.Empty(t, resp.Items[1].MagnetError)
	assert.Len(t, magnets.queries, 1, "the filtered-out row costs no magnet call")
}

func TestHandleCreateFile_NotifyWithDisabledNotifier(t *testing.T) {
	creator := &mockTaskCreator{}
	c := NewClient(&ClientCtx{
		Store:          &mockFileStore{},
		TaskCreator:    creator,
		DownloadClient: &mockDownloadClient{},
		Notifier:       &mockNotifier{enabled: false},
	})

	body := `{"url":"https://rutracker.org/forum/viewtopic.php?t=1","notify":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	c.handleCreateFile(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, 0, creator.createFromURLCalls)

	var resp map[string]string
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, map[string]string{"error": notifyUnavailable}, resp)
}

func TestHandleCreateFile_NotifyReachesTaskCreator(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		expected bool
	}{
		{name: "flag set", body: `{"url":"https://rutracker.org/forum/viewtopic.php?t=1","notify":true}`, expected: true},
		{name: "flag absent", body: `{"url":"https://rutracker.org/forum/viewtopic.php?t=1"}`, expected: false},
		{name: "flag false", body: `{"url":"https://rutracker.org/forum/viewtopic.php?t=1","notify":false}`, expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			creator := &mockTaskCreator{
				returnMeta: &tracker.FileMetadata{ID: "1", Name: "release", Notify: tt.expected},
			}
			c := NewClient(&ClientCtx{
				Store:          &mockFileStore{},
				TaskCreator:    creator,
				DownloadClient: &mockDownloadClient{},
				Notifier:       &mockNotifier{enabled: true},
			})

			req := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString(tt.body))
			w := httptest.NewRecorder()

			c.handleCreateFile(w, req)

			require.Equal(t, http.StatusCreated, w.Code)
			assert.Equal(t, 1, creator.createFromURLCalls)
			assert.Equal(t, tt.expected, creator.lastNotify)

			var resp map[string]any
			require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
			assert.Equal(t, tt.expected, resp["notify"])
		})
	}
}

func TestHandleCreateFile_NotifyWithoutNotifierConfigured(t *testing.T) {
	creator := &mockTaskCreator{}
	c := NewClient(&ClientCtx{Store: &mockFileStore{}, TaskCreator: creator, DownloadClient: &mockDownloadClient{}})

	body := `{"url":"https://rutracker.org/forum/viewtopic.php?t=1","notify":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	c.handleCreateFile(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, 0, creator.createFromURLCalls)
}

func TestHandleCreateFile_NotifyIsNotRefusedInDryMode(t *testing.T) {
	creator := &mockTaskCreator{returnMeta: &tracker.FileMetadata{ID: "1", Notify: true}}
	c := NewClient(&ClientCtx{
		Store:          &mockFileStore{},
		TaskCreator:    creator,
		DownloadClient: &mockDownloadClient{},
		Notifier:       &mockNotifier{enabled: true},
		DryMode:        true,
	})

	body := `{"url":"https://rutracker.org/forum/viewtopic.php?t=1","notify":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	c.handleCreateFile(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
	assert.True(t, creator.lastNotify)
}

func TestHandleFiles_CarriesNotifyThroughToResponse(t *testing.T) {
	store := &mockFileStore{files: []*tracker.FileMetadata{
		{ID: "1", Name: "armed", Notify: true},
		{ID: "2", Name: "quiet"},
	}}
	c := NewClient(&ClientCtx{Store: store, TaskCreator: &mockTaskCreator{}, DownloadClient: &mockDownloadClient{}})

	req := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	w := httptest.NewRecorder()

	c.handleFiles(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp []map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	require.Len(t, resp, 2)
	assert.Equal(t, true, resp[0]["notify"])
	assert.Equal(t, false, resp[1]["notify"])
}

func downloadsHealthCtx(store downloadStore) *ClientCtx {
	return &ClientCtx{
		Store:            &mockFileStore{files: failingFiles(0, 1)},
		Breaker:          &mockBreaker{states: map[string]tracker.State{"rutracker": {}}},
		RunState:         &mockRunState{at: time.Now().Add(-10 * time.Minute), ok: true},
		StaleRunAfter:    2 * time.Hour,
		StartedAt:        time.Now().Add(-5 * time.Hour),
		FailureThreshold: 3,
		DownloadStore:    store,
	}
}

func callHealthRaw(t *testing.T, ctx *ClientCtx) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	c := NewClient(ctx)
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	w := httptest.NewRecorder()
	c.healthHandler(w, req)

	return w, decodeBody(t, w)
}

func TestHealthDownloadsPending(t *testing.T) {
	tests := []struct {
		name    string
		pending int
	}{
		{name: "none pending", pending: 0},
		{name: "two pending", pending: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestDownloadStore(t)
			for i := 0; i < tt.pending; i++ {
				require.NoError(t, store.Create(&downloads.Download{
					ID:        fmt.Sprintf("001122334455667%d", i),
					Source:    "magnet:?xt=urn:btih:" + completedHash,
					Location:  "/downloads/default",
					Hash:      completedHash,
					CreatedAt: time.Now(),
				}))
			}

			require.NoError(t, store.Create(&downloads.Download{
				ID:        "ffffffffffffffff",
				Source:    "magnet:?xt=urn:btih:" + completedHash,
				Location:  "/downloads/default",
				Hash:      completedHash,
				CreatedAt: time.Now(),
			}))
			require.NoError(t, store.MarkPublished("ffffffffffffffff", downloads.Outcome{
				Status:      "completed",
				Name:        "sample.bin",
				CompletedAt: time.Now(),
			}))

			w, body := callHealthRaw(t, downloadsHealthCtx(store))

			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, "ok", body["status"])
			assert.Equal(t, map[string]any{"pending": float64(tt.pending)}, body["downloads"])
		})
	}
}

// most ClientCtx literals in this package leave the store unset, so a nil one must omit the
// key rather than panic
func TestHealthDownloadsOmittedWithoutStore(t *testing.T) {
	w, body := callHealthRaw(t, downloadsHealthCtx(nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ok", body["status"])
	assert.NotContains(t, body, "downloads")
}

// a pending download is normal operation, and an unreadable count is not the file sweep's
// problem either: neither may move the top-level verdict
func TestHealthDownloadsDoNotChangeStatus(t *testing.T) {
	tests := []struct {
		name          string
		ctx           func(*testing.T) *ClientCtx
		wantStatus    string
		wantCode      int
		wantDownloads any
	}{
		{
			name: "pending rows stay ok",
			ctx: func(t *testing.T) *ClientCtx {
				store := newTestDownloadStore(t)
				require.NoError(t, store.Create(&downloads.Download{
					ID:        "0011223344556677",
					Source:    "magnet:?xt=urn:btih:" + completedHash,
					Location:  "/downloads/default",
					Hash:      completedHash,
					CreatedAt: time.Now(),
				}))

				return downloadsHealthCtx(store)
			},
			wantStatus:    "ok",
			wantCode:      http.StatusOK,
			wantDownloads: map[string]any{"pending": float64(1)},
		},
		{
			name: "count error stays ok",
			ctx: func(_ *testing.T) *ClientCtx {
				return downloadsHealthCtx(&failingDownloadStore{err: errors.New("db is down")})
			},
			wantStatus: "ok",
			wantCode:   http.StatusOK,
		},
		{
			name: "count error does not lower a degraded verdict",
			ctx: func(_ *testing.T) *ClientCtx {
				ctx := downloadsHealthCtx(&failingDownloadStore{err: errors.New("db is down")})
				ctx.Store = &mockFileStore{files: failingFiles(3, 4)}

				return ctx
			},
			wantStatus: "degraded",
			wantCode:   http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, body := callHealthRaw(t, tt.ctx(t))

			assert.Equal(t, tt.wantCode, w.Code)
			assert.Equal(t, tt.wantStatus, body["status"])
			assert.Equal(t, tt.wantDownloads, body["downloads"])
		})
	}
}
