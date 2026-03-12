package tracker

import (
	"context"
	"database/sql"
	"fmt"
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

func (p *Parser) Parse(url string, location string) (*FileMetadata, error) {
	provider := p.getProvider(url)
	if provider == nil {
		return nil, fmt.Errorf("provider not found for url: %s", url)
	}

	result, err := provider.Parse(context.Background(), url)
	if err != nil {
		return nil, err
	}

	if location == "" {
		location = p.downloadClient.GetDefaultLocation()
	}

	originalURL := url
	if result.TrackerURL != "" {
		originalURL = result.TrackerURL
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

func (p *Parser) getProvider(url string) providers.Provider {
	for _, provider := range p.providers {
		if provider.CanHandle(url) {
			return provider
		}
	}
	return nil
}
