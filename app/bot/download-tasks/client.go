package download_tasks

import (
	"encoding/json"
	"fmt"
	"log"
	"magnet-feed-sync/app/bot"
	downloadClient "magnet-feed-sync/app/download-client"
	taskStore "magnet-feed-sync/app/task-store"
	"magnet-feed-sync/app/tracker"
	"time"
)

type Client struct {
	messagesForSend chan string
	tracker         *tracker.Parser
	dClient         downloadClient.Client
	store           *taskStore.Repository
	dryMode         bool
}

type ClientCtx struct {
	MessagesForSend chan string
	Tracker         *tracker.Parser
	DClient         downloadClient.Client
	Store           *taskStore.Repository
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

func (c *Client) OnMessage(msg bot.Message) (bool, string, error) {
	url := msg.Text

	metadata, err := c.tracker.Parse(url)
	if err != nil {
		return false, "", err
	}

	msgJSON, errJSON := json.Marshal(metadata)
	if errJSON != nil {
		return false, "", fmt.Errorf("failed to marshal metadata to json: %w", errJSON)
	}
	log.Printf("[DEBUG] Metadata: %s", string(msgJSON))

	formatedMsg, err := MetadataToMsg(metadata)
	if err != nil {
		return false, "", err
	}

	replyMsg := fmt.Sprintf("✅ Download task created:\n\n%s", formatedMsg)

	err = c.store.CreateOrReplace(metadata)
	if err != nil {
		return false, "", err
	}

	if c.dryMode {
		return true, replyMsg, nil
	}

	err = c.dClient.CreateDownloadTask(metadata.Magnet, metadata.Location)
	if err != nil {
		return false, "", err
	}

	log.Printf("[INFO] Download task created: %s", metadata.Name)

	return true, replyMsg, nil
}

func (c *Client) processFileMetadata(fileMetadata *tracker.FileMetadata) {
	updatedMetadata, err := c.tracker.Parse(fileMetadata.OriginalUrl)
	if err != nil {
		log.Printf("[ERROR] Error parsing metadata: %s", err)
		return
	}

	if fileMetadata.Location != "" {
		updatedMetadata.Location = fileMetadata.Location
	}

	updatedMetadata.LastSyncAt = time.Now()
	if fileMetadata.TorrentUpdatedAt == updatedMetadata.TorrentUpdatedAt {
		log.Printf("[INFO] Metadata is up to date: %s", fileMetadata.ID)

		if err := c.store.CreateOrReplace(updatedMetadata); err != nil {
			log.Printf("[ERROR] Error updating last sync at: %s", err)
		}

		return
	}
	log.Printf("[INFO] Metadata is outdated: %s", fileMetadata.ID)

	if err := c.store.CreateOrReplace(updatedMetadata); err != nil {
		log.Printf("[ERROR] Error updating metadata: %s", err)
	}
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
	}

	log.Printf("[INFO] Download task created: %s", updatedMetadata.Name)
}

func (c *Client) CheckForUpdates() {
	log.Printf("[INFO] Checking for updates")

	filesMetadata, err := c.store.GetAll()
	if err != nil {
		log.Fatalf("[ERROR] Error getting files metadata: %s", err)
	}

	for _, metadata := range filesMetadata {
		c.processFileMetadata(metadata)
	}
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
	jsonData, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("```json\n%s\n```", string(jsonData)), nil
}
