package tracker

import (
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html/charset"
	"io"
	"log"
	"magnet-feed-sync/app/tracker/providers"
	"net/http"
	"strings"
)

type FileMetadata struct {
	RssUrl string
	Magnet string
	Name   string
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
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	magnet := provider.GetMagnetLink(doc)
	rss := provider.GetRssLink(doc)
	title := provider.GetTitle(doc)

	return &FileMetadata{
		Name:   title,
		Magnet: magnet,
		RssUrl: rss,
	}, nil
}

func getProviderByUrl(url string) providers.Service {
	if strings.HasPrefix(url, providers.NnmUrl) {
		return &providers.NnmProvider{}
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
