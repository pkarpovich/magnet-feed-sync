package http

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/rs/cors"
	"go.opentelemetry.io/otel"
	"magnet-feed-sync/app/config"
	"magnet-feed-sync/app/tracker"
	"magnet-feed-sync/app/types"
	watch_store "magnet-feed-sync/app/watch-store"
	"magnet-feed-sync/app/watcher"
)

type TaskCreator interface {
	CreateFromURL(ctx context.Context, url, location string) (*tracker.FileMetadata, error)
	DownloadNow(ctx context.Context, source, location string) error
	RemoveTask(id string) error
	UpdateTaskLocation(id, location string) error
	CheckFileForUpdates(ctx context.Context, fileId string)
	RefreshAll(ctx context.Context)
}

type FileStore interface {
	GetAll() ([]*tracker.FileMetadata, error)
	GetById(id string) (*tracker.FileMetadata, error)
}

type DownloadClient interface {
	SetLocation(taskID, location string) error
	GetLocations() []types.Location
	GetHashByMagnet(magnet string) (string, error)
	GetDefaultLocation() string
}

type BreakerSnapshotter interface {
	Snapshot() map[string]tracker.State
}

type RunStateReader interface {
	GetLastRun() (time.Time, bool, error)
}

type watchStore interface {
	Create(w *watcher.Watch) error
	Update(w *watcher.Watch) error
	Disable(id string) error
	Revive(id string) error
	GetAll() ([]*watcher.Watch, error)
	GetByID(id string) (*watcher.Watch, error)
	WatchesForCycle() ([]*watcher.Watch, error)
	SeenRows(watchID string) ([]watch_store.SeenRow, error)
}

type searchEngine interface {
	Evaluate(ctx context.Context, w watcher.Watch) watcher.RunOutcome
}

type magnetResolver interface {
	Magnet(ctx context.Context, torrentID, query string) (string, error)
}

type Client struct {
	config           config.HttpConfig
	store            FileStore
	taskCreator      TaskCreator
	downloadClient   DownloadClient
	breaker          BreakerSnapshotter
	runState         RunStateReader
	watches          watchStore
	engine           searchEngine
	magnets          magnetResolver
	staleRunAfter    time.Duration
	staleWatchAfter  time.Duration
	startedAt        time.Time
	failureThreshold int
}

type ClientCtx struct {
	Config           config.HttpConfig
	Store            FileStore
	TaskCreator      TaskCreator
	DownloadClient   DownloadClient
	Breaker          BreakerSnapshotter
	RunState         RunStateReader
	WatchStore       watchStore
	Engine           searchEngine
	Magnets          magnetResolver
	StaleRunAfter    time.Duration
	StaleWatchAfter  time.Duration
	StartedAt        time.Time
	FailureThreshold int
}

func NewClient(ctx *ClientCtx) *Client {
	// an unset threshold would make `>= 0` true for every row and pin health to degraded
	threshold := ctx.FailureThreshold
	if threshold < 1 {
		threshold = 1
	}

	// an unset window would make every run older than zero, pinning health to unhealthy
	staleRunAfter := ctx.StaleRunAfter
	if staleRunAfter <= 0 {
		staleRunAfter = defaultStaleRunAfter
	}

	staleWatchAfter := ctx.StaleWatchAfter
	if staleWatchAfter <= 0 {
		staleWatchAfter = defaultStaleRunAfter
	}

	return &Client{
		config:           ctx.Config,
		store:            ctx.Store,
		taskCreator:      ctx.TaskCreator,
		downloadClient:   ctx.DownloadClient,
		breaker:          ctx.Breaker,
		runState:         ctx.RunState,
		watches:          ctx.WatchStore,
		engine:           ctx.Engine,
		magnets:          ctx.Magnets,
		staleRunAfter:    staleRunAfter,
		staleWatchAfter:  staleWatchAfter,
		startedAt:        ctx.StartedAt,
		failureThreshold: threshold,
	}
}

func (c *Client) Start(ctx context.Context, done chan struct{}) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/files", c.handleFiles)
	mux.HandleFunc("POST /api/files", c.handleCreateFile)
	mux.HandleFunc("POST /api/downloads", c.handleCreateDownload)
	mux.HandleFunc("PATCH /api/files/{fileId}/refresh", c.handleRefreshFile)
	mux.HandleFunc("PATCH /api/files/refresh", c.handleRefreshAllFiles)
	mux.HandleFunc("DELETE /api/files/{fileId}", c.handleRemoveFiles)
	mux.HandleFunc("GET /api/file-locations", c.handleGetFileLocations)
	mux.HandleFunc("POST /api/file-locations", c.handleSetFileLocation)
	mux.HandleFunc("POST /api/watches", c.handleCreateWatch)
	mux.HandleFunc("GET /api/watches", c.handleWatches)
	mux.HandleFunc("GET /api/watches/{watchId}", c.handleWatch)
	mux.HandleFunc("PATCH /api/watches/{watchId}", c.handleUpdateWatch)
	mux.HandleFunc("DELETE /api/watches/{watchId}", c.handleRemoveWatch)
	mux.HandleFunc("POST /api/watches/{watchId}/search", c.handleWatchSearch)
	mux.HandleFunc("POST /api/search", c.handleSearch)
	mux.HandleFunc("GET /api/health", c.healthHandler)
	mux.HandleFunc("GET /", c.fileHandler)

	server := &http.Server{
		Addr: fmt.Sprintf(":%d", c.config.Port),
		Handler: cors.New(cors.Options{
			AllowedOrigins: []string{"*"},
			AllowedMethods: []string{"GET", "POST", "PATCH", "DELETE"},
		}).Handler(mux),
	}

	go func() {
		slog.Info("starting HTTP server", "addr", server.Addr)
		if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTP server error", "error", err)
		}
		slog.Info("HTTP server stopped")
	}()

	<-ctx.Done()

	shutdownCtx, shutdownRelease := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownRelease()

	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server shutdown error", "error", err)
	}
	slog.Info("HTTP server shutdown")

	close(done)
}

type FileMetadataResponse struct {
	ID               string    `json:"id"`
	OriginalUrl      string    `json:"originalUrl"`
	Name             string    `json:"name"`
	LastComment      string    `json:"lastComment"`
	LastSyncAt       time.Time `json:"lastSyncAt"`
	Magnet           string    `json:"magnet"`
	TorrentUpdatedAt time.Time `json:"torrentUpdatedAt"`
	Location         string    `json:"location"`
}

func (c *Client) handleFiles(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("http").Start(r.Context(), "GET /api/files")
	defer span.End()

	w.Header().Set("Content-Type", "application/json")

	files, err := c.store.GetAll()
	if err != nil {
		slog.ErrorContext(ctx, "failed to get files", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	filesResponse := make([]FileMetadataResponse, 0, len(files))
	for _, f := range files {
		filesResponse = append(filesResponse, toResponse(f))
	}

	err = json.NewEncoder(w).Encode(filesResponse)
	if err != nil {
		slog.ErrorContext(ctx, "failed to encode files", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func toResponse(f *tracker.FileMetadata) FileMetadataResponse {
	return FileMetadataResponse{
		ID:               f.ID,
		Name:             f.Name,
		Magnet:           f.Magnet,
		Location:         f.Location,
		LastSyncAt:       f.LastSyncAt,
		OriginalUrl:      f.OriginalUrl,
		LastComment:      f.LastComment,
		TorrentUpdatedAt: f.TorrentUpdatedAt,
	}
}

type CreateFileRequest struct {
	URL      string `json:"url"`
	Location string `json:"location"`
}

func (c *Client) handleCreateFile(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("http").Start(r.Context(), "POST /api/files")
	defer span.End()

	var req CreateFileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.URL == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}

	metadata, err := c.taskCreator.CreateFromURL(ctx, req.URL, req.Location)
	if err != nil {
		slog.ErrorContext(ctx, "failed to create file from URL", "error", err)
		if errors.Is(err, tracker.ErrProviderNotFound) {
			http.Error(w, "unsupported URL", http.StatusBadRequest)
			return
		}
		http.Error(w, "failed to create file from URL", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(toResponse(metadata)); err != nil {
		slog.ErrorContext(ctx, "failed to encode response", "error", err)
	}
}

type CreateDownloadRequest struct {
	Source   string `json:"source"`
	Location string `json:"location"`
}

func (c *Client) handleCreateDownload(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("http").Start(r.Context(), "POST /api/downloads")
	defer span.End()

	var req CreateDownloadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if !isValidDownloadSource(req.Source) {
		http.Error(w, "source is required and must be a magnet or http(s) URL", http.StatusBadRequest)
		return
	}

	location := req.Location
	if location == "" {
		location = c.downloadClient.GetDefaultLocation()
	}

	if err := c.taskCreator.DownloadNow(ctx, req.Source, location); err != nil {
		slog.ErrorContext(ctx, "failed to create one-shot download", "error", err)
		http.Error(w, "failed to create download", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok"}); err != nil {
		slog.ErrorContext(ctx, "failed to encode response", "error", err)
	}
}

func isValidDownloadSource(source string) bool {
	return strings.HasPrefix(source, "magnet:") ||
		strings.HasPrefix(source, "http://") ||
		strings.HasPrefix(source, "https://")
}

func (c *Client) handleRemoveFiles(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("http").Start(r.Context(), "DELETE /api/files/{fileId}")
	defer span.End()

	w.Header().Set("Content-Type", "application/json")

	fileId := r.PathValue("fileId")

	err := c.taskCreator.RemoveTask(fileId)
	if err != nil {
		slog.ErrorContext(ctx, "failed to remove files", "error", err)
		http.Error(w, "failed to remove file", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (c *Client) handleRefreshFile(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("http").Start(r.Context(), "PATCH /api/files/{fileId}/refresh")
	defer span.End()

	w.Header().Set("Content-Type", "application/json")

	fileId := r.PathValue("fileId")
	c.taskCreator.CheckFileForUpdates(context.WithoutCancel(ctx), fileId)

	w.WriteHeader(http.StatusOK)
}

func (c *Client) handleRefreshAllFiles(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("http").Start(r.Context(), "PATCH /api/files/refresh")
	defer span.End()

	w.Header().Set("Content-Type", "application/json")

	c.taskCreator.RefreshAll(context.WithoutCancel(ctx))

	w.WriteHeader(http.StatusOK)
}

func (c *Client) handleGetFileLocations(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("http").Start(r.Context(), "GET /api/file-locations")
	defer span.End()

	w.Header().Set("Content-Type", "application/json")

	locations := c.downloadClient.GetLocations()
	err := json.NewEncoder(w).Encode(locations)
	if err != nil {
		slog.ErrorContext(ctx, "failed to encode locations", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type SetFileLocationRequest struct {
	FileId   string `json:"fileId"`
	Location string `json:"location"`
}

// SetFileLocationResponse reports the stored location and whether the already-downloaded
// files were moved with it. A missing torrent is normal — the weekly cleanup removes
// completed ones from the client — so it is reported, not treated as a failure.
type SetFileLocationResponse struct {
	Location string `json:"location"`
	Moved    bool   `json:"moved"`
	Reason   string `json:"reason,omitempty"`
}

func (c *Client) handleSetFileLocation(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("http").Start(r.Context(), "POST /api/file-locations")
	defer span.End()

	w.Header().Set("Content-Type", "application/json")

	var req SetFileLocationRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		slog.ErrorContext(ctx, "failed to decode request", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if !c.isKnownLocation(req.Location) {
		http.Error(w, "unknown location", http.StatusBadRequest)
		return
	}

	file, err := c.store.GetById(req.FileId)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}

		slog.ErrorContext(ctx, "failed to get file by id", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if file == nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	// the stored location decides where the next download goes, so it is recorded first
	// and never held hostage by the optional move below
	if err := c.taskCreator.UpdateTaskLocation(req.FileId, req.Location); err != nil {
		slog.ErrorContext(ctx, "failed to update file location", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := SetFileLocationResponse{Location: req.Location}
	resp.Moved, resp.Reason = c.moveDownloadedFiles(ctx, file.Magnet, req.Location)

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.ErrorContext(ctx, "failed to encode response", "error", err)
	}
}

func (c *Client) isKnownLocation(location string) bool {
	for _, known := range c.downloadClient.GetLocations() {
		if known.ID == location {
			return true
		}
	}

	return false
}

func (c *Client) moveDownloadedFiles(ctx context.Context, magnet, location string) (bool, string) {
	hash, err := c.downloadClient.GetHashByMagnet(magnet)
	if err != nil {
		if errors.Is(err, types.ErrTorrentNotFound) {
			return false, "torrent is no longer in the download client"
		}

		slog.ErrorContext(ctx, "failed to get hash by magnet", "error", err)
		return false, err.Error()
	}

	if err := c.downloadClient.SetLocation(hash, location); err != nil {
		slog.ErrorContext(ctx, "failed to set location", "error", err)
		return false, err.Error()
	}

	return true, ""
}

// the id becomes a NATS subject token: a dot or a space would corrupt it
const watchIDPatternSource = `^[a-z0-9_-]+$`

var watchIDPattern = regexp.MustCompile(watchIDPatternSource)

type watchRequest struct {
	ID           string     `json:"id"`
	Queries      []string   `json:"queries"`
	Sources      []string   `json:"sources"`
	IncludeRegex *string    `json:"include_regex"`
	ExcludeRegex *string    `json:"exclude_regex"`
	ExpiresAt    *time.Time `json:"expires_at"`
}

type watchResponse struct {
	ID           string     `json:"id"`
	Queries      []string   `json:"queries"`
	Sources      []string   `json:"sources"`
	IncludeRegex string     `json:"include_regex"`
	ExcludeRegex string     `json:"exclude_regex"`
	Rev          int        `json:"rev"`
	SeededAt     *time.Time `json:"seeded_at"`
	ExpiresAt    *time.Time `json:"expires_at"`
	// without this a disabled watch reads exactly like a healthy one that finds nothing
	DisabledAt *time.Time      `json:"disabled_at"`
	LastRunAt  *time.Time      `json:"last_run_at"`
	LastStatus string          `json:"last_status"`
	Seen       []watchSeenItem `json:"seen,omitempty"`
}

type watchSeenItem struct {
	Source      string    `json:"source"`
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	FirstSeenAt time.Time `json:"first_seen_at"`
}

func (c *Client) handleCreateWatch(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("http").Start(r.Context(), "POST /api/watches")
	defer span.End()

	if !c.watchStoreReady(w) {
		return
	}

	req, ok := decodeWatchRequest(w, r)
	if !ok {
		return
	}

	watch := &watcher.Watch{ID: req.ID}
	c.applyWatchRequest(watch, req)

	if err := c.validateWatch(watch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	existing, err := c.existingWatch(watch.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to look up watch", "watch_id", watch.ID, "error", err)
		http.Error(w, "failed to create watch", http.StatusInternalServerError)
		return
	}

	switch {
	case existing == nil:
		if err := c.watches.Create(watch); err != nil {
			slog.ErrorContext(ctx, "failed to create watch", "watch_id", watch.ID, "error", err)
			http.Error(w, "failed to create watch", http.StatusInternalServerError)
			return
		}
	case existing.DisabledAt == nil:
		http.Error(w, "watch already exists", http.StatusConflict)
		return
	default:
		// re-creatable, else DELETE retires the id for good; watch_seen survives so nothing replays
		if !c.reviveWatch(ctx, w, watch) {
			return
		}
	}

	c.writeWatch(ctx, w, watch.ID, http.StatusCreated)
}

func (c *Client) reviveWatch(ctx context.Context, w http.ResponseWriter, watch *watcher.Watch) bool {
	if err := c.watches.Update(watch); err != nil {
		slog.ErrorContext(ctx, "failed to update disabled watch", "watch_id", watch.ID, "error", err)
		http.Error(w, "failed to create watch", http.StatusInternalServerError)

		return false
	}

	if err := c.watches.Revive(watch.ID); err != nil {
		slog.ErrorContext(ctx, "failed to revive watch", "watch_id", watch.ID, "error", err)
		http.Error(w, "failed to create watch", http.StatusInternalServerError)

		return false
	}

	return true
}

func (c *Client) handleWatches(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("http").Start(r.Context(), "GET /api/watches")
	defer span.End()

	if !c.watchStoreReady(w) {
		return
	}

	watches, err := c.watches.GetAll()
	if err != nil {
		slog.ErrorContext(ctx, "failed to get watches", "error", err)
		http.Error(w, "failed to get watches", http.StatusInternalServerError)
		return
	}

	response := make([]watchResponse, 0, len(watches))
	for _, watch := range watches {
		response = append(response, toWatchResponse(watch))
	}

	c.encodeJSON(ctx, w, http.StatusOK, response)
}

func (c *Client) handleWatch(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("http").Start(r.Context(), "GET /api/watches/{watchId}")
	defer span.End()

	if !c.watchStoreReady(w) {
		return
	}

	watch, ok := c.loadWatch(ctx, w, r.PathValue("watchId"))
	if !ok {
		return
	}

	response := toWatchResponse(watch)

	// the seen rows are what makes a silent seed inspectable
	seen, err := c.watches.SeenRows(watch.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get seen rows", "watch_id", watch.ID, "error", err)
		http.Error(w, "failed to get watch", http.StatusInternalServerError)
		return
	}
	for _, row := range seen {
		response.Seen = append(response.Seen, watchSeenItem{
			Source:      row.Source,
			ID:          row.ExternalID,
			Title:       row.Title,
			FirstSeenAt: row.FirstSeenAt,
		})
	}

	c.encodeJSON(ctx, w, http.StatusOK, response)
}

func (c *Client) handleUpdateWatch(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("http").Start(r.Context(), "PATCH /api/watches/{watchId}")
	defer span.End()

	if !c.watchStoreReady(w) {
		return
	}

	req, ok := decodeWatchRequest(w, r)
	if !ok {
		return
	}

	watch, ok := c.loadWatch(ctx, w, r.PathValue("watchId"))
	if !ok {
		return
	}

	c.applyWatchRequest(watch, req)

	if err := c.validateWatch(watch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := c.watches.Update(watch); err != nil {
		if errors.Is(err, watch_store.ErrNotFound) {
			http.Error(w, "watch not found", http.StatusNotFound)
			return
		}

		slog.ErrorContext(ctx, "failed to update watch", "watch_id", watch.ID, "error", err)
		http.Error(w, "failed to update watch", http.StatusInternalServerError)
		return
	}

	c.writeWatch(ctx, w, watch.ID, http.StatusOK)
}

func (c *Client) handleRemoveWatch(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("http").Start(r.Context(), "DELETE /api/watches/{watchId}")
	defer span.End()

	if !c.watchStoreReady(w) {
		return
	}

	watchID := r.PathValue("watchId")

	// soft delete: the seen set survives, so re-creating does not replay past announcements
	if err := c.watches.Disable(watchID); err != nil {
		if errors.Is(err, watch_store.ErrNotFound) {
			http.Error(w, "watch not found", http.StatusNotFound)
			return
		}

		slog.ErrorContext(ctx, "failed to disable watch", "watch_id", watchID, "error", err)
		http.Error(w, "failed to remove watch", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
}

func (c *Client) watchStoreReady(w http.ResponseWriter) bool {
	if c.watches == nil {
		http.Error(w, "watch store is not configured", http.StatusServiceUnavailable)
		return false
	}

	return true
}

func decodeWatchRequest(w http.ResponseWriter, r *http.Request) (watchRequest, bool) {
	var req watchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return req, false
	}

	return req, true
}

func (c *Client) applyWatchRequest(watch *watcher.Watch, req watchRequest) {
	if req.Queries != nil {
		watch.Queries = trimmed(req.Queries)
	}
	if req.Sources != nil {
		watch.Sources = trimmed(req.Sources)
	}
	if req.IncludeRegex != nil {
		watch.IncludeRegex = *req.IncludeRegex
	}
	if req.ExcludeRegex != nil {
		watch.ExcludeRegex = *req.ExcludeRegex
	}
	if req.ExpiresAt != nil {
		watch.ExpiresAt = req.ExpiresAt
	}
	if len(watch.Sources) == 0 {
		watch.Sources = watcher.KnownSources()
	}
}

func (c *Client) validateWatch(watch *watcher.Watch) error {
	if !watchIDPattern.MatchString(watch.ID) {
		return fmt.Errorf("id must match %s", watchIDPatternSource)
	}

	return c.validateSearchParams(watch)
}

func (c *Client) validateSearchParams(watch *watcher.Watch) error {
	if len(watch.Queries) == 0 {
		return errors.New("at least one non-empty query is required")
	}

	if _, err := regexp.Compile(watch.IncludeRegex); err != nil {
		return fmt.Errorf("invalid include_regex: %w", err)
	}

	if _, err := regexp.Compile(watch.ExcludeRegex); err != nil {
		return fmt.Errorf("invalid exclude_regex: %w", err)
	}

	for _, name := range watch.Sources {
		if !isKnownSource(name) {
			return fmt.Errorf("unknown source %q, known sources are %s", name, strings.Join(watcher.KnownSources(), ", "))
		}
	}

	return nil
}

func (c *Client) existingWatch(id string) (*watcher.Watch, error) {
	watch, err := c.watches.GetByID(id)
	if errors.Is(err, watch_store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return watch, nil
}

func (c *Client) loadWatch(ctx context.Context, w http.ResponseWriter, id string) (*watcher.Watch, bool) {
	watch, err := c.watches.GetByID(id)
	if err != nil {
		if errors.Is(err, watch_store.ErrNotFound) {
			http.Error(w, "watch not found", http.StatusNotFound)
			return nil, false
		}

		slog.ErrorContext(ctx, "failed to get watch", "watch_id", id, "error", err)
		http.Error(w, "failed to get watch", http.StatusInternalServerError)

		return nil, false
	}

	return watch, true
}

func (c *Client) writeWatch(ctx context.Context, w http.ResponseWriter, id string, code int) {
	watch, err := c.watches.GetByID(id)
	if err != nil {
		slog.ErrorContext(ctx, "failed to load stored watch", "watch_id", id, "error", err)
		http.Error(w, "failed to load watch", http.StatusInternalServerError)
		return
	}

	c.encodeJSON(ctx, w, code, toWatchResponse(watch))
}

func (c *Client) encodeJSON(ctx context.Context, w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)

	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.ErrorContext(ctx, "failed to encode response", "error", err)
	}
}

func toWatchResponse(watch *watcher.Watch) watchResponse {
	return watchResponse{
		ID:           watch.ID,
		Queries:      watch.Queries,
		Sources:      watch.Sources,
		IncludeRegex: watch.IncludeRegex,
		ExcludeRegex: watch.ExcludeRegex,
		Rev:          watch.Rev,
		SeededAt:     watch.SeededAt,
		ExpiresAt:    watch.ExpiresAt,
		DisabledAt:   watch.DisabledAt,
		LastRunAt:    watch.LastRunAt,
		LastStatus:   watch.LastStatus,
	}
}

func isKnownSource(name string) bool {
	for _, known := range watcher.KnownSources() {
		if known == name {
			return true
		}
	}

	return false
}

func trimmed(values []string) []string {
	var kept []string
	for _, value := range values {
		if trimmedValue := strings.TrimSpace(value); trimmedValue != "" {
			kept = append(kept, trimmedValue)
		}
	}

	return kept
}

const magnetUnavailable = "extto magnet resolver is not configured"

type searchResponse struct {
	WatchID string       `json:"watch_id"`
	Total   int          `json:"total"`
	Matched int          `json:"matched"`
	Raw     bool         `json:"raw"`
	Errors  []string     `json:"errors"`
	Items   []searchItem `json:"items"`
}

type searchItem struct {
	Source      string    `json:"source"`
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	PageURL     string    `json:"page_url"`
	DownloadURL string    `json:"download_url"`
	Magnet      string    `json:"magnet"`
	MagnetError string    `json:"magnet_error"`
	Seeders     int       `json:"seeders"`
	PublishedAt time.Time `json:"published_at"`
	New         bool      `json:"new"`
}

func (c *Client) handleWatchSearch(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("http").Start(r.Context(), "POST /api/watches/{watchId}/search")
	defer span.End()

	if !c.watchStoreReady(w) || !c.engineReady(w) {
		return
	}

	watch, ok := c.loadActiveWatch(ctx, w, r.PathValue("watchId"))
	if !ok {
		return
	}

	c.runSearch(ctx, w, r, *watch)
}

func (c *Client) handleSearch(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("http").Start(r.Context(), "POST /api/search")
	defer span.End()

	if !c.engineReady(w) {
		return
	}

	req, ok := decodeWatchRequest(w, r)
	if !ok {
		return
	}

	// no id: Evaluate then skips the seen set and reports everything matched as new
	watch := &watcher.Watch{}
	c.applyWatchRequest(watch, req)

	if err := c.validateSearchParams(watch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	c.runSearch(ctx, w, r, *watch)
}

func (c *Client) runSearch(ctx context.Context, w http.ResponseWriter, r *http.Request, watch watcher.Watch) {
	o := c.engine.Evaluate(ctx, watch)

	// the pre-filter set is the only way to answer "why was I not woken by this"
	raw := r.URL.Query().Get("raw") == "true"
	results := o.Matched
	if raw {
		results = o.Raw
	}

	c.encodeJSON(ctx, w, http.StatusOK, searchResponse{
		WatchID: watch.ID,
		Total:   len(o.Raw),
		Matched: len(o.Matched),
		Raw:     raw,
		Errors:  errorMessages(o.Errs),
		// matched rows only: a magnet is a sequential signed round trip per row
		Items: c.searchItems(ctx, results, freshKeys(o.New), freshKeys(o.Matched)),
	})
}

func (c *Client) searchItems(
	ctx context.Context,
	results []watcher.SearchResult,
	fresh, matched map[string]struct{},
) []searchItem {
	items := make([]searchItem, 0, len(results))
	for _, result := range results {
		item := searchItem{
			Source:      result.Source,
			ID:          result.ExternalID,
			Title:       result.Title,
			PageURL:     result.PageURL,
			DownloadURL: result.DownloadURL,
			Seeders:     result.Seeders,
			PublishedAt: result.PublishedAt,
		}
		_, item.New = fresh[result.SeenKey()]

		// sequential: the signed call reuses the tokens of the search page just fetched
		if _, keep := matched[result.SeenKey()]; keep && result.Source == watcher.SourceExtto {
			item.Magnet, item.MagnetError = c.resolveMagnet(ctx, result)
		}

		items = append(items, item)
	}

	return items
}

func (c *Client) resolveMagnet(ctx context.Context, result watcher.SearchResult) (string, string) {
	if c.magnets == nil {
		return "", magnetUnavailable
	}

	magnet, err := c.magnets.Magnet(ctx, result.ExternalID, result.Query)
	if err != nil {
		slog.WarnContext(ctx, "failed to resolve magnet", "source", result.Source, "id", result.ExternalID, "error", err)

		return "", err.Error()
	}

	return magnet, ""
}

func (c *Client) loadActiveWatch(ctx context.Context, w http.ResponseWriter, id string) (*watcher.Watch, bool) {
	watches, err := c.watches.WatchesForCycle()
	if err != nil {
		slog.ErrorContext(ctx, "failed to load watches", "watch_id", id, "error", err)
		http.Error(w, "failed to get watch", http.StatusInternalServerError)

		return nil, false
	}

	for _, watch := range watches {
		if watch.ID == id {
			return watch, true
		}
	}

	http.Error(w, "watch not found", http.StatusNotFound)

	return nil, false
}

func (c *Client) engineReady(w http.ResponseWriter) bool {
	if c.engine == nil {
		http.Error(w, "search engine is not configured", http.StatusServiceUnavailable)
		return false
	}

	return true
}

func freshKeys(results []watcher.SearchResult) map[string]struct{} {
	keys := make(map[string]struct{}, len(results))
	for _, result := range results {
		keys[result.SeenKey()] = struct{}{}
	}

	return keys
}

func errorMessages(errs []error) []string {
	messages := make([]string, 0, len(errs))
	for _, err := range errs {
		messages = append(messages, err.Error())
	}

	return messages
}

const (
	statusOk        = "ok"
	statusDegraded  = "degraded"
	statusUnhealthy = "unhealthy"
	statusBlocked   = "blocked"
)

// defaultStaleRunAfter mirrors the fallback main.go uses when it cannot derive the window
// from the cron expression.
const defaultStaleRunAfter = 2 * time.Hour

type healthResponse struct {
	Status    string            `json:"status"`
	Tracked   int               `json:"tracked"`
	Failing   int               `json:"failing"`
	LastRunAt *time.Time        `json:"last_run_at,omitempty"`
	Providers map[string]string `json:"providers"`
	Watches   *watchesHealth    `json:"watches,omitempty"`
}

type watchesHealth struct {
	Active      int        `json:"active"`
	OldestRunAt *time.Time `json:"oldest_run_at,omitempty"`
	WithErrors  int        `json:"with_errors"`
}

func (c *Client) healthHandler(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("http").Start(r.Context(), "GET /api/health")
	defer span.End()

	files, err := c.store.GetAll()
	if err != nil {
		slog.ErrorContext(ctx, "failed to get files", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	failing := 0
	for _, f := range files {
		if f.ConsecutiveFailures >= c.failureThreshold {
			failing++
		}
	}

	providerStates, anyBlocked := c.providerStates()
	run := c.lastRun(ctx)
	watches, watchesDegraded := c.watchHealth(ctx)

	resp := healthResponse{
		Status:    statusOk,
		Tracked:   len(files),
		Failing:   failing,
		Providers: providerStates,
		Watches:   watches,
	}
	if run.present {
		resp.LastRunAt = &run.at
	}

	// degraded arm only, so it can never lower an unhealthy verdict reached elsewhere
	code := http.StatusOK
	switch {
	case anyBlocked || c.runIsStale(run):
		resp.Status = statusUnhealthy
		code = http.StatusServiceUnavailable
	// a sweep that could not read the task list refreshed last_run_at without checking
	// anything, so staleness alone would report it as healthy
	case failing > 0 || (run.present && !run.ok) || watchesDegraded:
		resp.Status = statusDegraded
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.ErrorContext(ctx, "failed to encode health response", "error", err)
	}
}

func (c *Client) providerStates() (map[string]string, bool) {
	states := make(map[string]string)
	if c.breaker == nil {
		return states, false
	}

	anyBlocked := false
	for name, state := range c.breaker.Snapshot() {
		if state.Tripped {
			states[name] = statusBlocked
			anyBlocked = true
			continue
		}
		states[name] = statusOk
	}

	return states, anyBlocked
}

func (c *Client) watchHealth(ctx context.Context) (*watchesHealth, bool) {
	if c.watches == nil {
		return nil, false
	}

	// WatchesForCycle drops soft-deleted rows only, so expiry is filtered here
	watches, err := c.watches.WatchesForCycle()
	if err != nil {
		slog.ErrorContext(ctx, "failed to load watches for health", "error", err)

		return nil, true
	}

	now := time.Now()
	health := &watchesHealth{}
	neverRan := false
	for _, watch := range watches {
		if watch.ExpiresAt != nil && !watch.ExpiresAt.After(now) {
			continue
		}

		health.Active++
		if watch.LastStatus != "" {
			health.WithErrors++
		}
		// a watch created seconds ago has no timestamp: it must not read as the oldest run
		if watch.LastRunAt == nil {
			neverRan = true

			continue
		}
		if health.OldestRunAt == nil || watch.LastRunAt.Before(*health.OldestRunAt) {
			at := *watch.LastRunAt
			health.OldestRunAt = &at
		}
	}

	return health, c.watchesAreDegraded(health, neverRan)
}

func (c *Client) watchesAreDegraded(health *watchesHealth, neverRan bool) bool {
	if health.WithErrors > 0 {
		return true
	}

	if health.OldestRunAt != nil && time.Since(*health.OldestRunAt) > c.staleWatchAfter {
		return true
	}

	// before the grace period a watch that never ran is expected, not a fault
	return neverRan && !c.startedAt.IsZero() && time.Since(c.startedAt) > c.staleWatchAfter
}

type runInfo struct {
	at      time.Time
	ok      bool
	present bool
}

func (c *Client) lastRun(ctx context.Context) runInfo {
	if c.runState == nil {
		return runInfo{}
	}

	at, ok, err := c.runState.GetLastRun()
	if err != nil {
		slog.ErrorContext(ctx, "failed to read last run state", "error", err)
		return runInfo{}
	}

	return runInfo{at: at, ok: ok, present: !at.IsZero()}
}

func (c *Client) runIsStale(run runInfo) bool {
	if run.present {
		return time.Since(run.at) > c.staleRunAfter
	}

	// a freshly booted service has not run yet; the grace period starts at boot
	if c.startedAt.IsZero() {
		return false
	}

	return time.Since(c.startedAt) > c.staleRunAfter
}

func (c *Client) fileHandler(w http.ResponseWriter, r *http.Request) {
	fileMatcher := regexp.MustCompile(`^/.*\..+$`)
	if fileMatcher.MatchString(r.URL.Path) {
		http.ServeFile(w, r, fmt.Sprintf("%s/%s", c.config.BaseStaticPath, r.URL.Path[1:]))
		return
	}

	http.ServeFile(w, r, fmt.Sprintf("%s/%s", c.config.BaseStaticPath, "index.html"))
}
