package qbittorrent

import (
	"fmt"

	qbt "github.com/autobrr/go-qbittorrent"
	"magnet-feed-sync/app/config"
	"magnet-feed-sync/app/types"
	"magnet-feed-sync/app/utils"
)

type Client struct {
	qbt                *qbt.Client
	defaultDestination string
}

func NewClient(config config.QBittorrentConfig) *Client {
	return &Client{
		qbt: qbt.NewClient(qbt.Config{
			Host:     config.URL,
			Username: config.Username,
			Password: config.Password,
		}),
		defaultDestination: config.Destination,
	}
}

func (c *Client) CreateDownloadTask(url, destination string) error {
	if _, err := c.qbt.AddTorrentFromUrl(url, map[string]string{"savepath": destination}); err != nil {
		return fmt.Errorf("add torrent: %w", err)
	}

	return nil
}

func (c *Client) GetHashByMagnet(magnet string) (string, error) {
	torrents, err := c.qbt.GetTorrents(qbt.TorrentFilterOptions{})
	if err != nil {
		return "", fmt.Errorf("get torrents: %w", err)
	}

	wanted := utils.ExtractBtihHash(magnet)
	for _, torrent := range torrents {
		if utils.ExtractBtihHash(torrent.MagnetURI) == wanted {
			return torrent.Hash, nil
		}
	}

	return "", fmt.Errorf("torrent not found")
}

func (c *Client) SetLocation(taskID, location string) error {
	if err := c.qbt.SetLocation([]string{taskID}, location); err != nil {
		return fmt.Errorf("set location: %w", err)
	}

	return nil
}

func (c *Client) GetLocations() []types.Location {
	return []types.Location{
		{ID: "/downloads/tv shows", Name: "TV Shows"},
		{ID: "/downloads/other", Name: "Other"},
		{ID: "/downloads/movies", Name: "Movies"},
		{ID: "/downloads/me", Name: "Me"},
		{ID: "/downloads/books", Name: "Books"},
		{ID: "/downloads/audiobooks", Name: "Audiobooks"},
		{ID: "/downloads/music", Name: "Music"},
		{ID: "/downloads/comics", Name: "Comics"},
		{ID: "/downloads/podcasts", Name: "Podcasts"},
		{ID: "/downloads/anime", Name: "Anime"},
	}
}

func (c *Client) GetDefaultLocation() string {
	return c.defaultDestination
}
