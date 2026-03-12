package providers

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/mmcdole/gofeed"
	"magnet-feed-sync/app/utils"
)

type NnmProvider struct{}

const NnmUrl = "https://nnmclub.to/forum"

func (p *NnmProvider) CanHandle(u string) bool {
	return strings.HasPrefix(u, NnmUrl)
}

func (p *NnmProvider) Parse(ctx context.Context, pageURL string) (*Result, error) {
	body, err := fetchPage(ctx, pageURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch nnm page: %w", err)
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("failed to parse nnm HTML: %w", err)
	}

	return &Result{
		ID:        p.getID(pageURL),
		Title:     p.getTitle(doc),
		Magnet:    p.getMagnetLink(doc),
		UpdatedAt: p.getLastUpdatedDate(doc),
		Comment:   p.getLastComment(doc),
	}, nil
}

func (p *NnmProvider) getMagnetLink(doc *goquery.Document) string {
	var magnetLink string

	doc.Find("a").Each(func(index int, item *goquery.Selection) {
		href, exists := item.Attr("href")
		if exists && strings.HasPrefix(href, "magnet:") {
			magnetLink = href
		}
	})

	return magnetLink
}

func (p *NnmProvider) getTitle(doc *goquery.Document) string {
	title := doc.Find("a.maintitle").Text()

	if title == "" {
		title = doc.Find("div.postbody span[style='font-size: 20px; line-height: normal'] span[style='font-weight: bold']").Text()
	}

	return strings.TrimSpace(title)
}

func (p *NnmProvider) getID(originalUrl string) string {
	u, err := url.Parse(originalUrl)
	if err != nil {
		log.Printf("[ERROR] Failed to parse nnm url: %s, %v", originalUrl, err)
		return ""
	}
	return u.Query().Get("t")
}

func (p *NnmProvider) getLastUpdatedDate(doc *goquery.Document) (registrationDate time.Time) {
	doc.Find("tr.row1").Each(func(i int, s *goquery.Selection) {
		label := s.Find("td.genmed").First().Text()
		if strings.Contains(label, "Зарегистрирован:") {
			rawDate := strings.TrimSpace(s.Find("td.genmed").Last().Text())
			date, err := utils.ParseRussianDate(rawDate)
			if err != nil {
				log.Printf("[ERROR] Failed to parse nnm torrent registration date: %s, %v", rawDate, err)
			}
			registrationDate = date
		}
	})

	return registrationDate
}

func (p *NnmProvider) getLastComment(doc *goquery.Document) string {
	rssLink := p.getRssLink(doc)
	if rssLink == "" {
		log.Printf("[WARN] rss link not found in nnm page")
		return ""
	}

	fp := gofeed.NewParser()
	feed, err := fp.ParseURL(rssLink)
	if err != nil || feed == nil {
		log.Printf("[ERROR] Failed to parse RSS feed: %v", err)
		return ""
	}

	commentBody := ""
	if len(feed.Items) > 0 {
		var lastFeedItem *gofeed.Item

		for _, item := range feed.Items {
			if item.PublishedParsed == nil {
				continue
			}
			if lastFeedItem == nil || lastFeedItem.PublishedParsed == nil || item.PublishedParsed.After(*lastFeedItem.PublishedParsed) {
				lastFeedItem = item
			}
		}

		if lastFeedItem != nil {
			commentBody = lastFeedItem.Description
		}
	}

	commentDoc, parseErr := goquery.NewDocumentFromReader(strings.NewReader(commentBody))
	if parseErr != nil {
		log.Printf("[ERROR] Failed to parse comment body: %v", parseErr)
		return ""
	}

	return strings.TrimSpace(commentDoc.Find("span.postbody").Text())
}

func (p *NnmProvider) getRssLink(doc *goquery.Document) string {
	var rssLink string

	doc.Find("td a").Each(func(index int, item *goquery.Selection) {
		href, exists := item.Attr("href")
		if exists && strings.HasPrefix(href, "rss.php") {
			rssLink = href
		}
	})

	if rssLink == "" {
		return ""
	}

	return fmt.Sprintf("%s/%s", NnmUrl, rssLink)
}
