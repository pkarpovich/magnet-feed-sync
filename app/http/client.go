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

// watchStore is the consumer-side view of the watch repository, holding only what the watch
// handlers call.
type watchStore interface {
	Create(w *watcher.Watch) error
	Update(w *watcher.Watch) error
	Disable(id string) error
	GetAll() ([]*watcher.Watch, error)
	GetByID(id string) (*watcher.Watch, error)
	SeenRows(watchID string) ([]watch_store.SeenRow, error)
}

type Client struct {
	config           config.HttpConfig
	store            FileStore
	taskCreator      TaskCreator
	downloadClient   DownloadClient
	breaker          BreakerSnapshotter
	runState         RunStateReader
	watches          watchStore
	staleRunAfter    time.Duration
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
	StaleRunAfter    time.Duration
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

	return &Client{
		config:           ctx.Config,
		store:            ctx.Store,
		taskCreator:      ctx.TaskCreator,
		downloadClient:   ctx.DownloadClient,
		breaker:          ctx.Breaker,
		runState:         ctx.RunState,
		watches:          ctx.WatchStore,
		staleRunAfter:    staleRunAfter,
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

// watchIDPatternSource is doubled as the 400 message, so an operator sees the rule that
// rejected the id. The id becomes a NATS subject token: a dot or a space would corrupt it.
const watchIDPatternSource = `^[a-z0-9_-]+$`

var watchIDPattern = regexp.MustCompile(watchIDPatternSource)

// watchRequest is the body of both POST /api/watches and PATCH /api/watches/{watchId}. The
// pointer fields separate "absent" from "explicitly empty", which PATCH needs and create
// treats as the zero value. A nil expires_at leaves an existing expiry untouched.
type watchRequest struct {
	ID           string     `json:"id"`
	Queries      []string   `json:"queries"`
	Sources      []string   `json:"sources"`
	IncludeRegex *string    `json:"include_regex"`
	ExcludeRegex *string    `json:"exclude_regex"`
	ExpiresAt    *time.Time `json:"expires_at"`
}

type watchResponse struct {
	ID           string          `json:"id"`
	Queries      []string        `json:"queries"`
	Sources      []string        `json:"sources"`
	IncludeRegex string          `json:"include_regex"`
	ExcludeRegex string          `json:"exclude_regex"`
	Rev          int             `json:"rev"`
	SeededAt     *time.Time      `json:"seeded_at"`
	ExpiresAt    *time.Time      `json:"expires_at"`
	LastRunAt    *time.Time      `json:"last_run_at"`
	LastStatus   string          `json:"last_status"`
	Seen         []watchSeenItem `json:"seen,omitempty"`
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

	exists, err := c.watchExists(watch.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to look up watch", "watch_id", watch.ID, "error", err)
		http.Error(w, "failed to create watch", http.StatusInternalServerError)
		return
	}
	if exists {
		http.Error(w, "watch already exists", http.StatusConflict)
		return
	}

	if err := c.watches.Create(watch); err != nil {
		slog.ErrorContext(ctx, "failed to create watch", "watch_id", watch.ID, "error", err)
		http.Error(w, "failed to create watch", http.StatusInternalServerError)
		return
	}

	c.writeWatch(ctx, w, watch.ID, http.StatusCreated)
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

	// a soft delete, mirroring how files are removed: the seen set survives, so re-creating
	// the watch does not replay every release it already announced
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

// applyWatchRequest overwrites only the fields the caller sent, which is what makes the same
// body work for create and for a partial update.
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

func (c *Client) watchExists(id string) (bool, error) {
	_, err := c.watches.GetByID(id)
	if errors.Is(err, watch_store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	return true, nil
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

// writeWatch answers with the stored row rather than with the request, so rev and the
// timestamps the database owns are what the caller sees.
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

	resp := healthResponse{
		Status:    statusOk,
		Tracked:   len(files),
		Failing:   failing,
		Providers: providerStates,
	}
	if run.present {
		resp.LastRunAt = &run.at
	}

	code := http.StatusOK
	switch {
	case anyBlocked || c.runIsStale(run):
		resp.Status = statusUnhealthy
		code = http.StatusServiceUnavailable
	// a sweep that could not read the task list refreshed last_run_at without checking
	// anything, so staleness alone would report it as healthy
	case failing > 0 || (run.present && !run.ok):
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
