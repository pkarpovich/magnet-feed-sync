package watcher

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	sourceJackett        = "jackett"
	jackettSearchPath    = "/api/v2.0/indexers/all/results/torznab/api"
	jackettSearchTimeout = 120 * time.Second
)

// JackettOptions carries the three URLs/keys the source needs; they are all strings and
// therefore a swap hazard as positional parameters.
type JackettOptions struct {
	BaseURL   string
	PublicURL string
	APIKey    string
}

// JackettSource searches every configured indexer through Jackett's torznab endpoint.
type JackettSource struct {
	baseURL   string
	publicURL *url.URL
	apiKey    string
	client    *http.Client
}

// NewJackettSource builds a source for the given Jackett instance. An empty PublicURL falls
// back to BaseURL.
func NewJackettSource(o JackettOptions) *JackettSource {
	public := o.PublicURL
	if public == "" {
		public = o.BaseURL
	}

	parsedPublic, err := url.Parse(normalizeJackettBase(public))
	if err != nil {
		parsedPublic = nil
	}

	return &JackettSource{
		baseURL:   normalizeJackettBase(o.BaseURL),
		publicURL: parsedPublic,
		apiKey:    o.APIKey,
		client:    &http.Client{Timeout: jackettSearchTimeout},
	}
}

func normalizeJackettBase(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.TrimRight(raw, "/")
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	if idx := strings.Index(u.Path, "/api/v2.0/"); idx >= 0 {
		u.Path = u.Path[:idx]
	}
	u.Path = strings.TrimRight(u.Path, "/")

	return u.String()
}

func (s *JackettSource) Name() string {
	return sourceJackett
}

func (s *JackettSource) Search(ctx context.Context, query string) ([]SearchResult, error) {
	if s.baseURL == "" {
		return nil, errors.New("search jackett: base url is not configured")
	}
	if s.apiKey == "" {
		return nil, errors.New("search jackett: api key is not configured")
	}

	ctx, cancel := context.WithTimeout(ctx, jackettSearchTimeout)
	defer cancel()

	body, err := s.fetch(ctx, query)
	if err != nil {
		return nil, err
	}

	return s.parse(body, query)
}

func (s *JackettSource) fetch(ctx context.Context, query string) ([]byte, error) {
	params := url.Values{}
	params.Set("apikey", s.apiKey)
	params.Set("t", "search")
	params.Set("q", query)
	endpoint := s.baseURL + jackettSearchPath + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("search jackett: %w", err)
	}

	res, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search jackett: %w", err)
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			slog.Warn("failed to close jackett response body", "error", err)
		}
	}()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("search jackett: read response: %w", err)
	}

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("search jackett: unexpected status %d", res.StatusCode)
	}

	return body, nil
}

func (s *JackettSource) parse(body []byte, query string) ([]SearchResult, error) {
	var rss torznabRSS
	if err := xml.Unmarshal(body, &rss); err != nil {
		return nil, fmt.Errorf("search jackett: parse response: %w", err)
	}

	results := make([]SearchResult, 0, len(rss.Channel.Items))
	for _, item := range rss.Channel.Items {
		pageURL := s.pageURL(item)
		externalID := s.externalID(item, pageURL)
		if externalID == "" {
			slog.Warn("skipping jackett item without an identity", "title", item.Title, "query", query)
			continue
		}

		results = append(results, SearchResult{
			Source:      sourceJackett,
			ExternalID:  externalID,
			Title:       item.Title,
			PageURL:     pageURL,
			DownloadURL: s.downloadURL(item),
			Query:       query,
			Seeders:     s.seeders(item),
			PublishedAt: s.publishedAt(item),
		})
	}

	return results, nil
}

func (s *JackettSource) pageURL(item torznabItem) string {
	if strings.HasPrefix(item.Comments, "http") {
		return item.Comments
	}
	if strings.HasPrefix(item.GUID, "http") {
		return item.GUID
	}

	return ""
}

func (s *JackettSource) externalID(item torznabItem, pageURL string) string {
	if pageURL != "" {
		if u, err := url.Parse(pageURL); err == nil {
			if t := u.Query().Get("t"); t != "" {
				return t
			}
		}
	}

	return item.GUID
}

// downloadURL rewrites the scheme and host Jackett emits — its own internal base — to the
// configured public one, so the link resolves for whoever receives it.
func (s *JackettSource) downloadURL(item torznabItem) string {
	raw := item.Link
	if raw == "" {
		raw = item.Enclosure.URL
	}
	if raw == "" || s.publicURL == nil || s.publicURL.Host == "" {
		return raw
	}

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	u.Scheme = s.publicURL.Scheme
	u.Host = s.publicURL.Host

	return u.String()
}

func (s *JackettSource) seeders(item torznabItem) int {
	for _, attr := range item.Attrs {
		if attr.Name != "seeders" {
			continue
		}
		seeders, err := strconv.Atoi(strings.TrimSpace(attr.Value))
		if err != nil {
			return 0
		}

		return seeders
	}

	return 0
}

func (s *JackettSource) publishedAt(item torznabItem) time.Time {
	if item.PubDate == "" {
		return time.Time{}
	}

	parsed, err := time.Parse(time.RFC1123Z, item.PubDate)
	if err != nil {
		parsed, err = time.Parse(time.RFC1123, item.PubDate)
		if err != nil {
			return time.Time{}
		}
	}

	return parsed
}

type torznabRSS struct {
	XMLName xml.Name       `xml:"rss"`
	Channel torznabChannel `xml:"channel"`
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
	// encoding/xml matches on the namespace URL, not the prefix: a `torznab:attr` tag
	// compiles and silently matches nothing.
	Attrs []torznabAttr `xml:"http://torznab.com/schemas/2015/feed attr"`
}

type torznabEnclosure struct {
	URL string `xml:"url,attr"`
}

type torznabAttr struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}
