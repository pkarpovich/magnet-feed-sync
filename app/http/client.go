package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/rs/cors"
	"log"
	downloadTasks "magnet-feed-sync/app/bot/download-tasks"
	"magnet-feed-sync/app/config"
	downloadClient "magnet-feed-sync/app/download-client"
	taskStore "magnet-feed-sync/app/task-store"
	"net/http"
	"regexp"
	"time"
)

type Client struct {
	config              config.HttpConfig
	store               *taskStore.Repository
	downloadTasksClient *downloadTasks.Client
	downloadClient      downloadClient.Client
}

func NewClient(
	cfg config.HttpConfig,
	store *taskStore.Repository,
	downloadTasksClient *downloadTasks.Client,
	downloadClient downloadClient.Client,
) *Client {
	return &Client{
		downloadTasksClient: downloadTasksClient,
		downloadClient:      downloadClient,
		config:              cfg,
		store:               store,
	}
}

func (c *Client) Start(ctx context.Context, done chan struct{}) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/files", c.handleFiles)
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
	c.downloadTasksClient.CheckFileForUpdates(fileId)

	w.WriteHeader(http.StatusOK)
}

func (c *Client) handleRefreshAllFiles(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	c.downloadTasksClient.CheckForUpdates()

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
