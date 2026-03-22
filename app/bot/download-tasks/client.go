package download_tasks

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"magnet-feed-sync/app/bot"
	downloadClient "magnet-feed-sync/app/download-client"
	"magnet-feed-sync/app/tracker"
)

type FileParser interface {
	Parse(url, location string) (*tracker.FileMetadata, error)
}

type FileStore interface {
	GetById(id string) (*tracker.FileMetadata, error)
	CreateOrReplace(metadata *tracker.FileMetadata) error
	GetAll() ([]*tracker.FileMetadata, error)
	Remove(id string) error
}

type Client struct {
	mu              sync.Mutex
	messagesForSend chan string
	tracker         FileParser
	dClient         downloadClient.Client
	store           FileStore
	dryMode         bool
}

type ClientCtx struct {
	MessagesForSend chan string
	Tracker         FileParser
	DClient         downloadClient.Client
	Store           FileStore
	DryMode         bool
}

func NewClient(ctx *ClientCtx) *Client {
	return &Client{
		messagesForSend: ctx.MessagesForSend,
		tracker:         ctx.Tracker,
		dClient:         ctx.DClient,
		dryMode:         ctx.DryMode,
		store:           ctx.Store,
	}
}

func (c *Client) OnMessage(msg bot.Message, location string) (bool, string, error) {
	metadata, err := c.CreateFromURL(msg.Text, location)
	if err != nil {
		return false, "", err
	}

	formatedMsg, err := MetadataToMsg(metadata)
	if err != nil {
		return false, "", err
	}

	replyMsg := fmt.Sprintf("✅ Download task created:\n\n%s", formatedMsg)
	return true, replyMsg, nil
}

func (c *Client) CreateFromURL(url, location string) (*tracker.FileMetadata, error) {
	metadata, err := c.tracker.Parse(url, location)
	if err != nil {
		return nil, err
	}

	log.Printf("[DEBUG] Metadata: %+v", metadata)

	return c.createWithLock(metadata)
}

func (c *Client) CreateFromMagnet(hash, magnet, name, location string) (*tracker.FileMetadata, error) {
	metadata := &tracker.FileMetadata{
		ID:         hash,
		Name:       name,
		Magnet:     magnet,
		Location:   location,
		LastSyncAt: time.Now(),
	}

	return c.createWithLock(metadata)
}

func (c *Client) createWithLock(metadata *tracker.FileMetadata) (*tracker.FileMetadata, error) {
	c.mu.Lock()

	existing, getErr := c.store.GetById(metadata.ID)
	if getErr != nil && !errors.Is(getErr, sql.ErrNoRows) {
		c.mu.Unlock()
		return nil, fmt.Errorf("check existing task: %w", getErr)
	}
	hadActiveRow := existing != nil && !existing.DeleteAt.Valid

	err := c.store.CreateOrReplace(metadata)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}

	c.mu.Unlock()

	if c.dryMode {
		return metadata, nil
	}

	err = c.dClient.CreateDownloadTask(metadata.Magnet, metadata.Location)
	if err != nil {
		c.rollbackCreate(metadata.ID, existing, hadActiveRow)
		return nil, err
	}

	log.Printf("[INFO] Download task created: %s", metadata.Name)

	return metadata, nil
}

func (c *Client) rollbackCreate(id string, existing *tracker.FileMetadata, hadActiveRow bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	current, err := c.store.GetById(id)
	if err != nil {
		log.Printf("[ERROR] failed to read task for rollback: %s", err)
		return
	}

	if current.DeleteAt.Valid {
		return
	}

	if hadActiveRow {
		if restoreErr := c.store.CreateOrReplace(existing); restoreErr != nil {
			log.Printf("[ERROR] failed to restore previous file after download error: %s", restoreErr)
		}
	} else {
		if removeErr := c.store.Remove(id); removeErr != nil {
			log.Printf("[ERROR] failed to remove file after download error: %s", removeErr)
		}
	}
}

func (c *Client) processFileMetadata(fileMetadata *tracker.FileMetadata) {
	if fileMetadata.OriginalUrl == "" {
		return
	}

	updatedMetadata, err := c.tracker.Parse(fileMetadata.OriginalUrl, "")
	if err != nil {
		log.Printf("[ERROR] Error parsing metadata: %s", err)
		return
	}

	c.mu.Lock()

	current, err := c.store.GetById(fileMetadata.ID)
	if err != nil {
		c.mu.Unlock()
		log.Printf("[ERROR] Error re-reading metadata: %s", err)
		return
	}

	if current.DeleteAt.Valid {
		c.mu.Unlock()
		return
	}

	if current.Location != "" {
		updatedMetadata.Location = current.Location
	}

	updatedMetadata.LastSyncAt = time.Now()
	if current.TorrentUpdatedAt.Equal(updatedMetadata.TorrentUpdatedAt) {
		log.Printf("[INFO] Metadata is up to date: %s", fileMetadata.ID)

		if err := c.store.CreateOrReplace(updatedMetadata); err != nil {
			log.Printf("[ERROR] Error updating last sync at: %s", err)
		}

		c.mu.Unlock()
		return
	}
	log.Printf("[INFO] Metadata is outdated: %s", fileMetadata.ID)

	if err := c.store.CreateOrReplace(updatedMetadata); err != nil {
		log.Printf("[ERROR] Error updating metadata: %s", err)
		c.mu.Unlock()
		return
	}

	c.mu.Unlock()

	log.Printf("[INFO] Metadata updated: %s", fileMetadata.ID)

	formatedMsg, err := MetadataToMsg(updatedMetadata)
	if err != nil {
		log.Printf("[ERROR] Error formatting metadata: %s", err)
		return
	}

	c.messagesForSend <- fmt.Sprintf("✅ Metadata updated:\n\n%s", formatedMsg)

	if c.dryMode {
		log.Printf("[INFO] Dry mode is enabled, skipping download")
		return
	}

	if err := c.dClient.CreateDownloadTask(updatedMetadata.Magnet, updatedMetadata.Location); err != nil {
		log.Printf("[ERROR] Error creating download task: %s", err)

		c.mu.Lock()
		updatedMetadata.TorrentUpdatedAt = current.TorrentUpdatedAt
		if storeErr := c.store.CreateOrReplace(updatedMetadata); storeErr != nil {
			log.Printf("[ERROR] Error reverting metadata after download failure: %s", storeErr)
		}
		c.mu.Unlock()
		return
	}

	log.Printf("[INFO] Download task created: %s", updatedMetadata.Name)
}

func (c *Client) CheckForUpdates() {
	log.Printf("[INFO] Checking for updates")

	filesMetadata, err := c.store.GetAll()
	if err != nil {
		log.Printf("[ERROR] Error getting files metadata: %s", err)
		return
	}

	for _, metadata := range filesMetadata {
		c.processFileMetadata(metadata)
	}
}

func (c *Client) RemoveTask(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.store.Remove(id)
}

func (c *Client) UpdateTaskLocation(id, location string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	file, err := c.store.GetById(id)
	if err != nil {
		return fmt.Errorf("get task: %w", err)
	}

	if file.DeleteAt.Valid {
		return fmt.Errorf("task %s has been deleted", id)
	}

	file.Location = location
	return c.store.CreateOrReplace(file)
}

func (c *Client) CheckFileForUpdates(fileId string) {
	metadata, err := c.store.GetById(fileId)
	if err != nil {
		log.Printf("[ERROR] Error getting metadata: %s", err)
		return
	}

	c.processFileMetadata(metadata)
}

func MetadataToMsg(metadata *tracker.FileMetadata) (string, error) {
	comment := metadata.LastComment
	runes := []rune(comment)
	if len(runes) > 100 {
		comment = string(runes[:100]) + "..."
	}

	display := *metadata
	display.LastComment = comment

	jsonData, err := json.MarshalIndent(&display, "", "  ")
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("```json\n%s\n```", string(jsonData)), nil
}
