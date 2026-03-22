package tracker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	downloadClient "magnet-feed-sync/app/download-client"
	"magnet-feed-sync/app/tracker/providers"
)

type FileMetadata struct {
	ID               string       `json:"id"`
	OriginalUrl      string       `json:"original_url"`
	Magnet           string       `json:"magnet"`
	Name             string       `json:"name"`
	LastComment      string       `json:"last_comment"`
	LastSyncAt       time.Time    `json:"last_sync_at"`
	TorrentUpdatedAt time.Time    `json:"torrent_updated_at"`
	Location         string       `json:"location"`
	CreatedAt        time.Time    `json:"-"`
	DeleteAt         sql.NullTime `json:"-"`
}

var ErrProviderNotFound = errors.New("provider not found")

type Parser struct {
	downloadClient downloadClient.Client
	providers      []providers.Provider
}

func NewParser(downloadClient downloadClient.Client, providerList ...providers.Provider) *Parser {
	return &Parser{
		downloadClient: downloadClient,
		providers:      providerList,
	}
}

func (p *Parser) Parse(ctx context.Context, url string, location string) (*FileMetadata, error) {
	provider := p.getProvider(url)
	if provider == nil {
		return nil, fmt.Errorf("%w for url: %s", ErrProviderNotFound, url)
	}

	result, err := provider.Parse(ctx, url)
	if err != nil {
		return nil, err
	}

	if location == "" {
		location = p.downloadClient.GetDefaultLocation()
	}

	originalURL := url
	if result.TrackerURL != "" {
		originalURL = result.TrackerURL
	} else if stripped := stripAPIKey(url); stripped != url {
		originalURL = ""
	}

	return &FileMetadata{
		ID:               result.ID,
		OriginalUrl:      originalURL,
		Magnet:           result.Magnet,
		Name:             result.Title,
		LastComment:      result.Comment,
		LastSyncAt:       time.Now(),
		TorrentUpdatedAt: result.UpdatedAt,
		Location:         location,
	}, nil
}

func stripAPIKey(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	changed := false
	for key := range q {
		if strings.Contains(strings.ToLower(key), "apikey") || strings.Contains(strings.ToLower(key), "api_key") {
			q.Del(key)
			changed = true
		}
	}
	if !changed {
		return rawURL
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (p *Parser) getProvider(url string) providers.Provider {
	for _, provider := range p.providers {
		if provider.CanHandle(url) {
			return provider
		}
	}
	return nil
}
