package qbittorrent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	qbt "github.com/autobrr/go-qbittorrent"
	"magnet-feed-sync/app/config"
	"magnet-feed-sync/app/types"
	"magnet-feed-sync/app/utils"
)

// ErrTorrentAddFailed is shared by the 409 and the 415 branch of AddTorrentFromUrl, so only the
// message tail tells a duplicate from a torrent file qBittorrent refused to parse
const duplicateAddMarker = "conflicts detected"

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

func (c *Client) CreateDownloadTask(url, destination string) (string, error) {
	res, err := c.qbt.AddTorrentFromUrl(url, map[string]string{"savepath": destination})
	if err != nil {
		if errors.Is(err, qbt.ErrTorrentAddFailed) && strings.Contains(err.Error(), duplicateAddMarker) {
			return "", fmt.Errorf("add torrent: %w", types.ErrTorrentAlreadyExists)
		}

		return "", fmt.Errorf("add torrent: %w", err)
	}

	if res != nil && len(res.AddedTorrentIds) > 0 {
		return res.AddedTorrentIds[0], nil
	}

	if strings.HasPrefix(strings.ToLower(url), "magnet:") {
		// only a hex infohash can be matched against what torrents/info reports: a base32
		// magnet would be stored as a hash the sweep never finds and published as a false failure
		if hash := utils.ExtractBtihHash(url); isInfoHash(hash) {
			return hash, nil
		}
	}

	// the torrent is added; only `added_torrent_ids` is missing, which every qbittorrent below
	// the WebAPI version that introduced it omits by answering `text/plain`. The add is a success
	// for every caller that does not need the hash, and refusing it here would fail a plain
	// `.torrent` add outright. Callers that promise an event check for the empty hash instead
	return "", nil
}

func isInfoHash(s string) bool {
	if len(s) != 40 {
		return false
	}

	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}

	return true
}

func (c *Client) TorrentStates(ctx context.Context, hashes []string) (map[string]types.TorrentState, error) {
	torrents, err := c.qbt.GetTorrentsCtx(ctx, qbt.TorrentFilterOptions{Hashes: hashes})
	if err != nil {
		return nil, fmt.Errorf("get torrents: %w", err)
	}

	states := make(map[string]types.TorrentState, len(torrents))
	for _, torrent := range torrents {
		states[torrent.Hash] = types.TorrentState{
			Hash:         torrent.Hash,
			Name:         torrent.Name,
			State:        string(torrent.State),
			ContentPath:  torrent.ContentPath,
			Progress:     torrent.Progress,
			CompletionOn: torrent.CompletionOn,
			Size:         torrent.Size,
		}
	}

	return states, nil
}

func (c *Client) GetHashByMagnet(magnet string) (string, error) {
	torrents, err := c.qbt.GetTorrents(qbt.TorrentFilterOptions{})
	if err != nil {
		return "", fmt.Errorf("get torrents: %w", err)
	}

	wanted := utils.ExtractBtihHash(magnet)
	if wanted == "" {
		return "", fmt.Errorf("magnet carries no btih: %w", types.ErrTorrentNotFound)
	}
	for _, torrent := range torrents {
		if utils.ExtractBtihHash(torrent.MagnetURI) == wanted {
			return torrent.Hash, nil
		}
	}

	return "", types.ErrTorrentNotFound
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
		{ID: "/downloads/magazines", Name: "Magazines"},
		{ID: "/downloads/cinema-prep", Name: "Cinema Prep"},
	}
}

func (c *Client) GetDefaultLocation() string {
	return c.defaultDestination
}
