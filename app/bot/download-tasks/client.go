package download_tasks

import (
	"encoding/json"
	"fmt"
	"log"
	"magnet-feed-sync/app/bot"
	downloadStation "magnet-feed-sync/app/download-station"
	"magnet-feed-sync/app/tracker"
)

type Client struct {
	tracker  *tracker.Parser
	dsClient *downloadStation.Client
	dryMode  bool
}

func NewClient(tracker *tracker.Parser, dsClient *downloadStation.Client, dryMode bool) *Client {
	return &Client{
		tracker:  tracker,
		dsClient: dsClient,
		dryMode:  dryMode,
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

	jsonData, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return false, "", err
	}

	replyMsg := fmt.Sprintf("✅ Download task created:\n\n```json\n%s\n```", string(jsonData))

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
