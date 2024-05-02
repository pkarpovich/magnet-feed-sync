package tracker

import (
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"io"
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
	defer body.Close()

	provider := getProviderByUrl(url)
	doc, err := goquery.NewDocumentFromReader(body)
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

func getPageBody(url string) (io.ReadCloser, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status: %s", resp.Status)
	}

	return resp.Body, nil
}
