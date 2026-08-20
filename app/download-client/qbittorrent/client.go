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

		return "", fmt.Errorf("add torrent: %w", withoutSource(err, url))
	}

	// qbittorrent answers 200 with a failure count for a source it refused to take; without this
	// the magnet fallback below would hand back a hash for a torrent that was never added, and the
	// sweep would publish that as a torrent deleted by hand ten minutes later
	if res != nil && res.FailureCount > 0 && len(res.AddedTorrentIds) == 0 {
		return "", errors.New("add torrent: qbittorrent refused the source")
	}

	if res != nil && len(res.AddedTorrentIds) > 0 {
		return res.AddedTorrentIds[0], nil
	}

	if strings.HasPrefix(strings.ToLower(url), "magnet:") {
		// only a hex infohash can be matched against what torrents/info reports: a base32
		// magnet would be stored as a hash the sweep never finds and published as a false failure
		if hash := utils.ExtractBtihHash(url); utils.IsInfoHash(hash) {
			return hash, nil
		}
	}

	// the torrent is added; only `added_torrent_ids` is missing, which every qbittorrent below
	// the WebAPI version that introduced it omits by answering `text/plain`. The add is a success
	// for every caller that does not need the hash, and refusing it here would fail a plain
	// `.torrent` add outright. Callers that promise an event check for the empty hash instead
	return "", nil
}

// every add error the client library builds embeds the source verbatim, and a jackett `.torrent`
// link carries its api key in the query string, so the message is redacted here rather than at
// each caller that logs it. The library error stays underneath, so errors.Is still sees it
func withoutSource(err error, source string) error {
	redacted := utils.RedactURL(source)
	if source == "" || redacted == source {
		return err
	}

	return &sanitisedError{err: err, msg: strings.ReplaceAll(err.Error(), source, redacted)}
}

type sanitisedError struct {
	err error
	msg string
}

func (e *sanitisedError) Error() string { return e.msg }

func (e *sanitisedError) Unwrap() error { return e.err }

// the library joins the hashes into the query string of a GET torrents/info, ~41 chars each, so
// an unbounded pending set would eventually exceed the request-line limit of qbittorrent or any
// proxy in front of it — and one failed lookup aborts the whole sweep, stalling every pending row
const hashBatchSize = 100

func (c *Client) TorrentStates(ctx context.Context, hashes []string) (map[string]types.TorrentState, error) {
	states := make(map[string]types.TorrentState, len(hashes))

	for start := 0; start < len(hashes); start += hashBatchSize {
		end := min(start+hashBatchSize, len(hashes))

		torrents, err := c.qbt.GetTorrentsCtx(ctx, qbt.TorrentFilterOptions{Hashes: hashes[start:end]})
		if err != nil {
			return nil, fmt.Errorf("get torrents: %w", err)
		}

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
