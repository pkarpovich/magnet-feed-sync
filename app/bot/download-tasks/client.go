package download_tasks

import (
	"encoding/json"
	"fmt"
	"log"
	"magnet-feed-sync/app/bot"
	downloadStation "magnet-feed-sync/app/download-station"
	taskStore "magnet-feed-sync/app/task-store"
	"magnet-feed-sync/app/tracker"
)

type Client struct {
	tracker  *tracker.Parser
	dsClient *downloadStation.Client
	store    *taskStore.Repository
	dryMode  bool
}

type ClientCtx struct {
	Tracker  *tracker.Parser
	DSClient *downloadStation.Client
	Store    *taskStore.Repository
	DryMode  bool
}

func NewClient(ctx *ClientCtx) *Client {
	return &Client{
		tracker:  ctx.Tracker,
		dsClient: ctx.DSClient,
		dryMode:  ctx.DryMode,
		store:    ctx.Store,
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

	err = c.dsClient.CreateDownloadTask(metadata.Magnet)
	if err != nil {
		return false, "", err
	}

	log.Printf("[INFO] Download task created: %s", metadata.Name)

	return true, replyMsg, nil
}

func MetadataToMsg(metadata *tracker.FileMetadata) (string, error) {
	jsonData, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("```json\n%s\n```", string(jsonData)), nil
}
