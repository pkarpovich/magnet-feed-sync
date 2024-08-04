package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/rs/cors"
	"log"
	"magnet-feed-sync/app/config"
	taskStore "magnet-feed-sync/app/task-store"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type Client struct {
	config config.HttpConfig
	store  *taskStore.Repository
}

func NewClient(cfg config.HttpConfig, store *taskStore.Repository) *Client {
	return &Client{
		config: cfg,
		store:  store,
	}
}

func (c *Client) Start(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /files", c.handleFiles)

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", c.config.Port),
		Handler: cors.Default().Handler(mux),
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

func (c *Client) handleFiles(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	files, err := c.store.GetAll()
	if err != nil {
		log.Printf("[ERROR] failed to get files: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	err = json.NewEncoder(w).Encode(files)
	if err != nil {
		log.Printf("[ERROR] failed to encode files: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}
