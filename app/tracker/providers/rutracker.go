package providers

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"go.opentelemetry.io/otel"
	"magnet-feed-sync/app/utils"
)

type RutrackerProvider struct{}

const RutrackerUrl = "https://rutracker.org/forum"

func (p *RutrackerProvider) CanHandle(u string) bool {
	return strings.HasPrefix(u, RutrackerUrl)
}

func (p *RutrackerProvider) Parse(ctx context.Context, pageURL string) (*Result, error) {
	ctx, span := otel.Tracer("tracker").Start(ctx, "RutrackerProvider.Parse")
	defer span.End()

	body, err := fetchPage(ctx, pageURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch rutracker page: %w", err)
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("failed to parse rutracker HTML: %w", err)
	}

	magnet := p.getMagnetLink(doc)
	if magnet == "" {
		return nil, fmt.Errorf("no magnet link found in rutracker page")
	}

	return &Result{
		ID:        p.getID(pageURL),
		Title:     p.getTitle(doc),
		Magnet:    magnet,
		UpdatedAt: p.getLastUpdatedDate(doc),
		Comment:   p.getLastComment(doc),
	}, nil
}

func (p *RutrackerProvider) getMagnetLink(doc *goquery.Document) string {
	magnetLink, exists := doc.Find("a.magnet-link").Attr("href")
	if !exists {
		slog.Warn("magnet link not found in rutracker page")
	}
	return magnetLink
}

func (p *RutrackerProvider) getTitle(doc *goquery.Document) string {
	attempt1 := doc.Find("a#topic-title").Text()
	if len(attempt1) > 0 {
		return strings.TrimSpace(attempt1)
	}

	attempt2 := doc.Find("div.post_body > span + hr").Prev().Find("span.post-b").Text()
	if len(attempt2) > 0 {
		return strings.TrimSpace(attempt2)
	}

	attempt3 := doc.Find("div.post_body > span").Find("span.post-b").First().Text()
	if len(attempt3) > 0 {
		return strings.TrimSpace(attempt3)
	}

	slog.Warn("title not found in rutracker page")
	return ""
}

func (p *RutrackerProvider) getID(originalUrl string) string {
	u, err := url.Parse(originalUrl)
	if err != nil {
		slog.Error("failed to parse rutracker url", "url", originalUrl, "error", err)
		return ""
	}
	return u.Query().Get("t")
}

func (p *RutrackerProvider) getLastUpdatedDate(doc *goquery.Document) time.Time {
	firstPost := doc.Find("table#topic_main > tbody").FilterFunction(func(i int, s *goquery.Selection) bool {
		_, exists := s.Attr("id")
		return exists
	}).First()

	editedText := firstPost.Find("p.post-time > span.posted_since").Text()

	var editedDate string
	prefix := "ред. "
	if pos := strings.Index(editedText, prefix); pos != -1 {
		editedDate = strings.TrimSpace(editedText[pos+len(prefix):])
		if len(editedDate) > 0 && editedDate[len(editedDate)-1] == ')' {
			editedDate = editedDate[:len(editedDate)-1]
		}
	}

	if len(editedDate) > 0 {
		date, err := utils.ParseRussianDate(editedDate)
		if err != nil {
			slog.Error("failed to parse rutracker torrent edited date", "date", editedDate, "error", err)
		} else {
			return date
		}
	}

	postDateText := strings.TrimSpace(firstPost.Find("p.post-time a.p-link").Text())
	if len(postDateText) > 0 {
		date, err := utils.ParseRussianDate(postDateText)
		if err != nil {
			slog.Error("failed to parse rutracker torrent post date", "date", postDateText, "error", err)
		} else {
			return date
		}
	}

	slog.Warn("no date found in rutracker page")
	return time.Time{}
}

func (p *RutrackerProvider) getLastComment(doc *goquery.Document) string {
	slog.Warn("comments not supported in rutracker page")
	return ""
}
