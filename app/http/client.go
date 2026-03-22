package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/rs/cors"
	"go.opentelemetry.io/otel"
	"magnet-feed-sync/app/config"
	"magnet-feed-sync/app/tracker"
	"magnet-feed-sync/app/types"
	"magnet-feed-sync/app/utils"
)

type TaskCreator interface {
	CreateFromURL(url, location string) (*tracker.FileMetadata, error)
	CreateFromMagnet(hash, magnet, name, location string) (*tracker.FileMetadata, error)
	RemoveTask(id string) error
	UpdateTaskLocation(id, location string) error
	CheckFileForUpdates(fileId string)
	CheckForUpdates()
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

type Client struct {
	config         config.HttpConfig
	store          FileStore
	taskCreator    TaskCreator
	downloadClient DownloadClient
}

func NewClient(
	cfg config.HttpConfig,
	store FileStore,
	taskCreator TaskCreator,
	downloadClient DownloadClient,
) *Client {
	return &Client{
		taskCreator:    taskCreator,
		downloadClient: downloadClient,
		config:         cfg,
		store:          store,
	}
}

func (c *Client) Start(ctx context.Context, done chan struct{}) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/files", c.handleFiles)
	mux.HandleFunc("POST /api/files", c.handleCreateFile)
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
	_, span := otel.Tracer("http").Start(r.Context(), "GET /api/files")
	defer span.End()

	w.Header().Set("Content-Type", "application/json")

	files, err := c.store.GetAll()
	if err != nil {
		slog.Error("failed to get files", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	filesResponse := make([]FileMetadataResponse, 0, len(files))
	for _, f := range files {
		filesResponse = append(filesResponse, toResponse(f))
	}

	err = json.NewEncoder(w).Encode(filesResponse)
	if err != nil {
		slog.Error("failed to encode files", "error", err)
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
	Magnet   string `json:"magnet"`
	Name     string `json:"name"`
}

func (c *Client) handleCreateFile(w http.ResponseWriter, r *http.Request) {
	_, span := otel.Tracer("http").Start(r.Context(), "POST /api/files")
	defer span.End()

	var req CreateFileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.URL == "" && req.Magnet == "" {
		http.Error(w, "url or magnet is required", http.StatusBadRequest)
		return
	}

	var metadata *tracker.FileMetadata

	if req.URL != "" {
		m, err := c.taskCreator.CreateFromURL(req.URL, req.Location)
		if err != nil {
			slog.Error("failed to create file from URL", "error", err)
			if errors.Is(err, tracker.ErrProviderNotFound) {
				http.Error(w, "unsupported URL", http.StatusBadRequest)
				return
			}
			http.Error(w, "failed to create file from URL", http.StatusInternalServerError)
			return
		}
		metadata = m
	} else {
		hash := utils.ExtractBtihHash(req.Magnet)
		if hash == "" {
			http.Error(w, "could not extract hash from magnet link", http.StatusBadRequest)
			return
		}

		location := req.Location
		if location == "" {
			location = c.downloadClient.GetDefaultLocation()
		}

		m, err := c.taskCreator.CreateFromMagnet(hash, req.Magnet, req.Name, location)
		if err != nil {
			slog.Error("failed to create file from magnet", "error", err)
			http.Error(w, "failed to create file from magnet", http.StatusInternalServerError)
			return
		}
		metadata = m
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(toResponse(metadata)); err != nil {
		slog.Error("failed to encode response", "error", err)
	}
}

func (c *Client) handleRemoveFiles(w http.ResponseWriter, r *http.Request) {
	_, span := otel.Tracer("http").Start(r.Context(), "DELETE /api/files/{fileId}")
	defer span.End()

	w.Header().Set("Content-Type", "application/json")

	fileId := r.PathValue("fileId")

	err := c.taskCreator.RemoveTask(fileId)
	if err != nil {
		slog.Error("failed to remove files", "error", err)
		http.Error(w, "failed to remove file", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (c *Client) handleRefreshFile(w http.ResponseWriter, r *http.Request) {
	_, span := otel.Tracer("http").Start(r.Context(), "PATCH /api/files/{fileId}/refresh")
	defer span.End()

	w.Header().Set("Content-Type", "application/json")

	fileId := r.PathValue("fileId")
	c.taskCreator.CheckFileForUpdates(fileId)

	w.WriteHeader(http.StatusOK)
}

func (c *Client) handleRefreshAllFiles(w http.ResponseWriter, r *http.Request) {
	_, span := otel.Tracer("http").Start(r.Context(), "PATCH /api/files/refresh")
	defer span.End()

	w.Header().Set("Content-Type", "application/json")

	c.taskCreator.CheckForUpdates()

	w.WriteHeader(http.StatusOK)
}

func (c *Client) handleGetFileLocations(w http.ResponseWriter, r *http.Request) {
	_, span := otel.Tracer("http").Start(r.Context(), "GET /api/file-locations")
	defer span.End()

	w.Header().Set("Content-Type", "application/json")

	locations := c.downloadClient.GetLocations()
	err := json.NewEncoder(w).Encode(locations)
	if err != nil {
		slog.Error("failed to encode locations", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type SetFileLocationRequest struct {
	FileId   string `json:"fileId"`
	Location string `json:"location"`
}

func (c *Client) handleSetFileLocation(w http.ResponseWriter, r *http.Request) {
	_, span := otel.Tracer("http").Start(r.Context(), "POST /api/file-locations")
	defer span.End()

	w.Header().Set("Content-Type", "application/json")

	var req SetFileLocationRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		slog.Error("failed to decode request", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	file, err := c.store.GetById(req.FileId)
	if err != nil {
		slog.Error("failed to get file by id", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if file == nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	hash, err := c.downloadClient.GetHashByMagnet(file.Magnet)
	if err != nil {
		slog.Error("failed to get hash by magnet", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	err = c.downloadClient.SetLocation(hash, req.Location)
	if err != nil {
		slog.Error("failed to set location", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	err = c.taskCreator.UpdateTaskLocation(req.FileId, req.Location)
	if err != nil {
		slog.Error("failed to update file location", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

type HealthResponse struct {
	Count   int    `json:"count"`
	Message string `json:"message"`
}

func (c *Client) healthHandler(w http.ResponseWriter, r *http.Request) {
	_, span := otel.Tracer("http").Start(r.Context(), "GET /api/health")
	defer span.End()

	files, err := c.store.GetAll()
	if err != nil {
		slog.Error("failed to get files", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	err = json.NewEncoder(w).Encode(HealthResponse{
		Count:   len(files),
		Message: "OK",
	})
	if err != nil {
		slog.Error("failed to encode health response", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (c *Client) fileHandler(w http.ResponseWriter, r *http.Request) {
	fileMatcher := regexp.MustCompile(`^/.*\..+$`)
	if fileMatcher.MatchString(r.URL.Path) {
		http.ServeFile(w, r, fmt.Sprintf("%s/%s", c.config.BaseStaticPath, r.URL.Path[1:]))
		return
	}

	http.ServeFile(w, r, fmt.Sprintf("%s/%s", c.config.BaseStaticPath, "index.html"))
}
