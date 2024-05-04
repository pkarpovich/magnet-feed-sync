package download_tasks

import (
	"magnet-feed-sync/app/bot"
	downloadStation "magnet-feed-sync/app/download-station"
	"magnet-feed-sync/app/tracker"
)

type Client struct {
	tracker  *tracker.Parser
	dsClient *downloadStation.Client
}

func NewClient(tracker *tracker.Parser, dsClient *downloadStation.Client) *Client {
	return &Client{
		tracker:  tracker,
		dsClient: dsClient,
	}
}

func (c *Client) OnMessage(msg bot.Message) (bool, error) {
	url := msg.Text

	metadata, err := c.tracker.Parse(url)
	if err != nil {
		return false, err
	}

	err = c.dsClient.CreateDownloadTask(metadata.Magnet)
	if err != nil {
		return false, err
	}

	return true, nil
}
