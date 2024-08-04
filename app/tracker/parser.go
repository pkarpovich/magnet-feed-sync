package tracker

import (
	"database/sql"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html/charset"
	"io"
	"log"
	"magnet-feed-sync/app/tracker/providers"
	"net/http"
	"strings"
	"time"
)

type FileMetadata struct {
	ID               string       `json:"id"`
	OriginalUrl      string       `json:"original_url"`
	Magnet           string       `json:"magnet"`
	Name             string       `json:"name"`
	LastComment      string       `json:"last_comment"`
	LastSyncAt       time.Time    `json:"last_sync_at"`
	TorrentUpdatedAt time.Time    `json:"torrent_updated_at"`
	CreatedAt        time.Time    `json:"-"`
	DeleteAt         sql.NullTime `json:"-"`
}

type Parser struct {
}

func NewParser() *Parser {
	return &Parser{}
}

func (p *Parser) Parse(url string) (*FileMetadata, error) {
	body, err := getPageBody(url)
	if err != nil {
		return nil, err
	}

	provider := getProviderByUrl(url)
	if provider == nil {
		return nil, fmt.Errorf("provider not found for url: %s", url)
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}

	magnet := provider.GetMagnetLink(doc)
	title := provider.GetTitle(doc)
	id := provider.GetId(url)
	updatedAt := provider.GetLastUpdatedDate(doc)
	lastComment := provider.GetLastComment(doc)

	return &FileMetadata{
		LastSyncAt:       time.Now(),
		TorrentUpdatedAt: updatedAt,
		LastComment:      lastComment,
		OriginalUrl:      url,
		Magnet:           magnet,
		Name:             title,
		ID:               id,
	}, nil
}

func getProviderByUrl(url string) providers.Service {
	if strings.HasPrefix(url, providers.NnmUrl) {
		return &providers.NnmProvider{}
	}

	if strings.HasPrefix(url, providers.RutrackerUrl) {
		return &providers.RutrackerProvider{}
	}

	return nil
}

func getPageBody(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("[ERROR] error closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status: %s", resp.Status)
	}

	utf8Reader, err := charset.NewReader(resp.Body, resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, err
	}

	body, err := io.ReadAll(utf8Reader)
	if err != nil {
		return nil, err
	}

	return body, nil
}
