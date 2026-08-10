package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"magnet-feed-sync/app/tracker"
	"magnet-feed-sync/app/types"
)

type mockTaskCreator struct {
	lastURL              string
	lastLocation         string
	lastDownloadSource   string
	lastDownloadLocation string
	downloadCalls        int
	returnMeta           *tracker.FileMetadata
	returnErr            error
	downloadErr          error
}

func (m *mockTaskCreator) CreateFromURL(_ context.Context, url, location string) (*tracker.FileMetadata, error) {
	m.lastURL = url
	m.lastLocation = location
	return m.returnMeta, m.returnErr
}

func (m *mockTaskCreator) DownloadNow(_ context.Context, source, location string) error {
	m.downloadCalls++
	m.lastDownloadSource = source
	m.lastDownloadLocation = location
	return m.downloadErr
}

func (m *mockTaskCreator) RemoveTask(id string) error                      { return nil }
func (m *mockTaskCreator) UpdateTaskLocation(id, location string) error    { return nil }
func (m *mockTaskCreator) CheckFileForUpdates(_ context.Context, _ string) {}
func (m *mockTaskCreator) CheckForUpdates(_ context.Context)               {}

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
	defaultLocation string
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
