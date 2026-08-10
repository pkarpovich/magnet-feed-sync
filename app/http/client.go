package http

import (
	"context"
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

type Client struct {
	config           config.HttpConfig
	store            FileStore
	taskCreator      TaskCreator
	downloadClient   DownloadClient
	breaker          BreakerSnapshotter
	runState         RunStateReader
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

	return &Client{
		config:           ctx.Config,
		store:            ctx.Store,
		taskCreator:      ctx.TaskCreator,
		downloadClient:   ctx.DownloadClient,
		breaker:          ctx.Breaker,
		runState:         ctx.RunState,
		staleRunAfter:    ctx.StaleRunAfter,
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

	file, err := c.store.GetById(req.FileId)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get file by id", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if file == nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	hash, err := c.downloadClient.GetHashByMagnet(file.Magnet)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get hash by magnet", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	err = c.downloadClient.SetLocation(hash, req.Location)
	if err != nil {
		slog.ErrorContext(ctx, "failed to set location", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	err = c.taskCreator.UpdateTaskLocation(req.FileId, req.Location)
	if err != nil {
		slog.ErrorContext(ctx, "failed to update file location", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

const (
	statusOk        = "ok"
	statusDegraded  = "degraded"
	statusUnhealthy = "unhealthy"
	statusBlocked   = "blocked"
)

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
