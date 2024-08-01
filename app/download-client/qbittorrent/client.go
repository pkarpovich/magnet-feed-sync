package qbittorrent

import (
	"fmt"
	qbt "github.com/NullpointerW/go-qbittorrent-apiv2"
	"magnet-feed-sync/app/config"
)

type Client struct {
	username    string
	password    string
	baseUrl     string
	destination string
}

func NewClient(config config.QBittorrentConfig) *Client {
	return &Client{
		baseUrl:     config.URL,
		username:    config.Username,
		password:    config.Password,
		destination: config.Destination,
	}
}

func (c *Client) CreateDownloadTask(url string) error {
	client, err := qbt.NewCli(c.baseUrl, c.username, c.password)
	if err != nil {
		return fmt.Errorf("failed to create qbittorrent client: %w", err)
	}

	err = client.AddNewTorrentViaUrl(url, c.destination)
	if err != nil {
		return fmt.Errorf("failed to add torrent: %w", err)
	}

	return nil
}
