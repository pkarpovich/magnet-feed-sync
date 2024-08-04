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
	taskStore "magnet-feed-sync/app/task-store"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"
)

type Client struct {
	config              config.HttpConfig
	store               *taskStore.Repository
	downloadTasksClient *downloadTasks.Client
}

func NewClient(cfg config.HttpConfig, store *taskStore.Repository, downloadTasksClient *downloadTasks.Client) *Client {
	return &Client{
		downloadTasksClient: downloadTasksClient,
		config:              cfg,
		store:               store,
	}
}

func (c *Client) Start(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/files", c.handleFiles)
	mux.HandleFunc("PATCH /api/files/{fileId}/refresh", c.handleRefreshFile)
	mux.HandleFunc("PATCH /api/files/refresh", c.handleRefreshAllFiles)
	mux.HandleFunc("DELETE /api/files/{fileId}", c.handleRemoveFiles)
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

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	shutdownCtx, shutdownRelease := context.WithTimeout(ctx, 10*time.Second)
	defer shutdownRelease()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("[ERROR] HTTP server error: %v", err)
	}
	log.Printf("[INFO] HTTP server shutdown")
}

type FileMetadataResponse struct {
	ID               string    `json:"id"`
	OriginalUrl      string    `json:"originalUrl"`
	Name             string    `json:"name"`
	LastComment      string    `json:"lastComment"`
	LastSyncAt       time.Time `json:"lastSyncAt"`
	Magnet           string    `json:"magnet"`
	TorrentUpdatedAt time.Time `json:"torrentUpdatedAt"`
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

func (c *Client) fileHandler(w http.ResponseWriter, r *http.Request) {
	fileMatcher := regexp.MustCompile(`^/.*\..+$`)
	if fileMatcher.MatchString(r.URL.Path) {
		http.ServeFile(w, r, fmt.Sprintf("%s/%s", c.config.BaseStaticPath, r.URL.Path[1:]))
		return
	}

	http.ServeFile(w, r, fmt.Sprintf("%s/%s", c.config.BaseStaticPath, "index.html"))
}
