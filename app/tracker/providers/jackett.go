package providers

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/url"
	"strings"
	"time"

	"magnet-feed-sync/app/utils"
)

type JackettProvider struct {
	baseURL string
}

func NewJackettProvider(baseURL string) *JackettProvider {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return &JackettProvider{baseURL: strings.TrimRight(baseURL, "/")}
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	if idx := strings.Index(u.Path, "/api/v2.0/"); idx >= 0 {
		u.Path = u.Path[:idx]
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return &JackettProvider{baseURL: u.String()}
}

func (p *JackettProvider) CanHandle(u string) bool {
	if p.baseURL == "" {
		return false
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return false
	}
	base, err := url.Parse(p.baseURL)
	if err != nil || base.Host == "" {
		return false
	}
	if parsed.Scheme != base.Scheme || parsed.Host != base.Host {
		return false
	}
	if base.Path != "" && parsed.Path != base.Path && !strings.HasPrefix(parsed.Path, base.Path+"/") {
		return false
	}
	remainder := parsed.Path
	if base.Path != "" {
		remainder = strings.TrimPrefix(parsed.Path, base.Path)
	}
	if !strings.HasPrefix(remainder, "/api/v2.0/") {
		return false
	}
	return true
}

func (p *JackettProvider) Parse(ctx context.Context, pageURL string) (*Result, error) {
	body, err := fetchPage(ctx, pageURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch jackett page: %w", err)
	}

	return p.parseXML(body, pageURL)
}

func (p *JackettProvider) parseXML(data []byte, originalURL string) (*Result, error) {
	var rss torznabRSS
	if err := xml.Unmarshal(data, &rss); err != nil {
		return nil, fmt.Errorf("failed to parse jackett XML: %w", err)
	}

	if len(rss.Channel.Items) == 0 {
		return nil, fmt.Errorf("no items found in jackett response")
	}

	item := rss.Channel.Items[0]

	magnet := p.extractMagnet(item)
	if magnet == "" {
		return nil, fmt.Errorf("no magnet link found in jackett response")
	}

	trackerURL := p.extractTrackerURL(item)
	id := p.extractID(trackerURL, originalURL)
	if id == "" {
		id = utils.ExtractBtihHash(magnet)
	}

	var updatedAt time.Time
	if item.PubDate != "" {
		parsed, err := time.Parse(time.RFC1123Z, item.PubDate)
		if err != nil {
			parsed, _ = time.Parse(time.RFC1123, item.PubDate)
		}
		updatedAt = parsed
	}

	return &Result{
		ID:         id,
		Title:      item.Title,
		Magnet:     magnet,
		UpdatedAt:  updatedAt,
		TrackerURL: trackerURL,
	}, nil
}

func (p *JackettProvider) extractMagnet(item torznabItem) string {
	if strings.HasPrefix(item.Link, "magnet:") {
		return item.Link
	}
	if item.Enclosure.URL != "" && strings.HasPrefix(item.Enclosure.URL, "magnet:") {
		return item.Enclosure.URL
	}
	return ""
}

func (p *JackettProvider) extractTrackerURL(item torznabItem) string {
	if item.Comments != "" && strings.HasPrefix(item.Comments, "http") {
		return item.Comments
	}
	if item.GUID != "" && strings.HasPrefix(item.GUID, "http") {
		return item.GUID
	}
	return ""
}

func (p *JackettProvider) extractID(trackerURL, originalURL string) string {
	if trackerURL != "" {
		if u, err := url.Parse(trackerURL); err == nil {
			if t := u.Query().Get("t"); t != "" {
				return t
			}
		}
	}

	if u, err := url.Parse(originalURL); err == nil {
		if id := u.Query().Get("id"); id != "" {
			return id
		}
	}

	return ""
}

type torznabRSS struct {
	XMLName xml.Name        `xml:"rss"`
	Channel torznabChannel  `xml:"channel"`
}

type torznabChannel struct {
	Items []torznabItem `xml:"item"`
}

type torznabItem struct {
	Title     string           `xml:"title"`
	GUID      string           `xml:"guid"`
	Comments  string           `xml:"comments"`
	Link      string           `xml:"link"`
	PubDate   string           `xml:"pubDate"`
	Enclosure torznabEnclosure `xml:"enclosure"`
}

type torznabEnclosure struct {
	URL string `xml:"url,attr"`
}
