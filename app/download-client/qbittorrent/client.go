package qbittorrent

import (
	"fmt"
	qbt "github.com/NullpointerW/go-qbittorrent-apiv2"
	"log"
	"magnet-feed-sync/app/config"
	"magnet-feed-sync/app/types"
	"net/url"
	"strings"
)

type Client struct {
	username           string
	password           string
	baseUrl            string
	defaultDestination string
}

func NewClient(config config.QBittorrentConfig) *Client {
	return &Client{
		baseUrl:            config.URL,
		username:           config.Username,
		password:           config.Password,
		defaultDestination: config.Destination,
	}
}

func (c *Client) CreateDownloadTask(url, destination string) error {
	client, err := qbt.NewCli(c.baseUrl, c.username, c.password)
	if err != nil {
		return fmt.Errorf("failed to create qbittorrent client: %w", err)
	}

	err = client.AddNewTorrentViaUrl(url, destination)
	if err != nil {
		return fmt.Errorf("failed to add torrent: %w", err)
	}

	return nil
}

func (c *Client) GetHashByMagnet(magnet string) (string, error) {
	client, err := qbt.NewCli(c.baseUrl, c.username, c.password)
	if err != nil {
		return "", fmt.Errorf("failed to create qbittorrent client: %w", err)
	}

	torrents, err := client.TorrentList(qbt.Optional{})
	if err != nil {
		return "", fmt.Errorf("failed to get torrents: %w", err)
	}

	for _, torrent := range torrents {
		cleanMagnet, err := removeDnFromMagnet(torrent.MagnetURI)
		if err != nil {
			log.Printf("failed to remove dn from magnet: %s", err)
			continue
		}

		if strings.EqualFold(cleanMagnet, magnet) {
			return torrent.Hash, nil
		}
	}

	return "", fmt.Errorf("torrent not found")
}

func (c *Client) SetLocation(taskID, location string) error {
	client, err := qbt.NewCli(c.baseUrl, c.username, c.password)
	if err != nil {
		return fmt.Errorf("failed to create qbittorrent client: %w", err)
	}

	err = client.SetLocation(location, taskID)
	if err != nil {
		return fmt.Errorf("failed to set location: %w", err)
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

func removeDnFromMagnet(magnetLink string) (string, error) {
	u, err := url.Parse(magnetLink)
	if err != nil {
		return "", err
	}

	q := u.RawQuery
	params := strings.Split(q, "&")
	var newParams []string

	for _, param := range params {
		if strings.HasPrefix(param, "dn=") {
			continue
		}
		newParams = append(newParams, param)
	}

	u.RawQuery = strings.Join(newParams, "&")

	return u.String(), nil
}

func (c *Client) GetDefaultLocation() string {
	return c.defaultDestination
}
