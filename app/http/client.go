package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/rs/cors"
	"magnet-feed-sync/app/config"
	"magnet-feed-sync/app/tracker"
	"magnet-feed-sync/app/types"
)

type TaskCreator interface {
	CreateFromURL(url, location string) (*tracker.FileMetadata, error)
	CheckFileForUpdates(fileId string)
	CheckForUpdates()
}

type FileStore interface {
	GetAll() ([]*tracker.FileMetadata, error)
	GetById(id string) (*tracker.FileMetadata, error)
	CreateOrReplace(metadata *tracker.FileMetadata) error
	Remove(id string) error
}

type DownloadClient interface {
	CreateDownloadTask(url, destination string) error
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
		log.Printf("[INFO] Starting HTTP server on %s", server.Addr)
		if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[ERROR] HTTP server error: %v", err)
		}
		log.Printf("[INFO] HTTP server stopped")
	}()

	<-ctx.Done()

	shutdownCtx, shutdownRelease := context.WithTimeout(ctx, 10*time.Second)
	defer shutdownRelease()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("[ERROR] HTTP server error: %v", err)
	}
	log.Printf("[INFO] HTTP server shutdown")

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
	w.Header().Set("Content-Type", "application/json")

	files, err := c.store.GetAll()
	if err != nil {
		log.Printf("[ERROR] failed to get files: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var filesResponse []FileMetadataResponse
	for _, f := range files {
		filesResponse = append(filesResponse, FileMetadataResponse{
			ID:               f.ID,
			Name:             f.Name,
			Magnet:           f.Magnet,
			Location:         f.Location,
			LastSyncAt:       f.LastSyncAt,
			OriginalUrl:      f.OriginalUrl,
			LastComment:      f.LastComment,
			TorrentUpdatedAt: f.TorrentUpdatedAt,
		})
	}

	err = json.NewEncoder(w).Encode(filesResponse)
	if err != nil {
		log.Printf("[ERROR] failed to encode files: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

type CreateFileRequest struct {
	URL      string `json:"url"`
	Location string `json:"location"`
	Magnet   string `json:"magnet"`
	Name     string `json:"name"`
}

func (c *Client) handleCreateFile(w http.ResponseWriter, r *http.Request) {
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
			log.Printf("[ERROR] failed to create file from URL: %s", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		metadata = m
	} else {
		location := req.Location
		if location == "" {
			location = c.downloadClient.GetDefaultLocation()
		}

		hash := extractHashFromMagnet(req.Magnet)

		metadata = &tracker.FileMetadata{
			ID:         hash,
			Name:       req.Name,
			Magnet:     req.Magnet,
			Location:   location,
			LastSyncAt: time.Now(),
		}

		if err := c.store.CreateOrReplace(metadata); err != nil {
			log.Printf("[ERROR] failed to save file: %s", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if err := c.downloadClient.CreateDownloadTask(req.Magnet, location); err != nil {
			log.Printf("[ERROR] failed to create download task: %s", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(FileMetadataResponse{
		ID:               metadata.ID,
		OriginalUrl:      metadata.OriginalUrl,
		Name:             metadata.Name,
		LastComment:      metadata.LastComment,
		LastSyncAt:       metadata.LastSyncAt,
		Magnet:           metadata.Magnet,
		TorrentUpdatedAt: metadata.TorrentUpdatedAt,
		Location:         metadata.Location,
	})
}

func extractHashFromMagnet(magnet string) string {
	lower := strings.ToLower(magnet)
	idx := strings.Index(lower, "urn:btih:")
	if idx == -1 {
		return ""
	}
	hash := magnet[idx+len("urn:btih:"):]
	if ampIdx := strings.Index(hash, "&"); ampIdx != -1 {
		hash = hash[:ampIdx]
	}
	return hash
}

func (c *Client) handleRemoveFiles(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	fileId := r.PathValue("fileId")

	err := c.store.Remove(fileId)
	if err != nil {
		log.Printf("[ERROR] failed to remove files: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (c *Client) handleRefreshFile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	fileId := r.PathValue("fileId")
	c.taskCreator.CheckFileForUpdates(fileId)

	w.WriteHeader(http.StatusOK)
}

func (c *Client) handleRefreshAllFiles(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	c.taskCreator.CheckForUpdates()

	w.WriteHeader(http.StatusOK)
}

func (c *Client) handleGetFileLocations(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	locations := c.downloadClient.GetLocations()
	err := json.NewEncoder(w).Encode(locations)
	if err != nil {
		log.Printf("[ERROR] failed to encode locations: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type SetFileLocationRequest struct {
	FileId   string `json:"fileId"`
	Location string `json:"location"`
}

func (c *Client) handleSetFileLocation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var req SetFileLocationRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		log.Printf("[ERROR] failed to decode request: %s", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	file, err := c.store.GetById(req.FileId)
	if err != nil {
		log.Printf("[ERROR] failed to get file by id: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if file == nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	hash, err := c.downloadClient.GetHashByMagnet(file.Magnet)
	if err != nil {
		log.Printf("[ERROR] failed to get hash by magnet: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	err = c.downloadClient.SetLocation(hash, req.Location)
	if err != nil {
		log.Printf("[ERROR] failed to set location: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	file.Location = req.Location
	err = c.store.CreateOrReplace(file)
	if err != nil {
		log.Printf("[ERROR] failed to update file location: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

type HealthResponse struct {
	Count   int    `json:"count"`
	Message string `json:"message"`
}

func (c *Client) healthHandler(w http.ResponseWriter, r *http.Request) {
	files, err := c.store.GetAll()
	if err != nil {
		log.Printf("[ERROR] failed to get files: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	err = json.NewEncoder(w).Encode(HealthResponse{
		Count:   len(files),
		Message: "OK",
	})
	if err != nil {
		log.Printf("[ERROR] failed to encode health response: %s", err)
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
