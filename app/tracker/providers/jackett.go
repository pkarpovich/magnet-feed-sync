package providers

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type JackettProvider struct {
	baseURL string
}

func NewJackettProvider(baseURL string) *JackettProvider {
	return &JackettProvider{baseURL: strings.TrimRight(baseURL, "/")}
}

func (p *JackettProvider) CanHandle(u string) bool {
	if p.baseURL == "" {
		return false
	}
	return strings.HasPrefix(u, p.baseURL)
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
		id = extractBtihHash(magnet)
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

func extractBtihHash(magnet string) string {
	lower := strings.ToLower(magnet)
	idx := strings.Index(lower, "urn:btih:")
	if idx == -1 {
		return ""
	}
	hash := magnet[idx+len("urn:btih:"):]
	if ampIdx := strings.Index(hash, "&"); ampIdx != -1 {
		hash = hash[:ampIdx]
	}
	return strings.ToLower(hash)
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
	Size      string           `xml:"size"`
	Enclosure torznabEnclosure `xml:"enclosure"`
}

type torznabEnclosure struct {
	URL string `xml:"url,attr"`
}
