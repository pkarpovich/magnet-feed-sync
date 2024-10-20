package providers

import (
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"github.com/mmcdole/gofeed"
	"log"
	"magnet-feed-sync/app/utils"
	"net/url"
	"strings"
	"time"
)

type NnmProvider struct{}

const NnmUrl = "https://nnmclub.to/forum"

func (p *NnmProvider) GetMagnetLink(doc *goquery.Document) string {
	var magnetLink string

	doc.Find("a").Each(func(index int, item *goquery.Selection) {
		href, exists := item.Attr("href")
		if exists && strings.HasPrefix(href, "magnet:") {
			magnetLink = href
		}
	})

	return magnetLink
}

func (p *NnmProvider) GetTitle(doc *goquery.Document) string {
	title := doc.Find("a.maintitle").Text()

	if title == "" {
		title = doc.Find("div.postbody span[style='font-size: 20px; line-height: normal'] span[style='font-weight: bold']").Text()
	}

	return strings.TrimSpace(title)
}

func (p *NnmProvider) GetId(originalUrl string) string {
	u, err := url.Parse(originalUrl)
	if err != nil {
		log.Printf("[ERROR] Failed to parse nnm url: %s, %v", originalUrl, err)
		return ""
	}

	return u.Query().Get("t")
}

func (p *NnmProvider) GetLastUpdatedDate(doc *goquery.Document) (registrationDate time.Time) {
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

func (p *NnmProvider) GetLastComment(doc *goquery.Document) string {
	rssLink := getRssLink(doc)
	if rssLink == "" {
		log.Printf("[WARN] rss link not found in nnm page")
		return ""
	}

	fp := gofeed.NewParser()
	feed, _ := fp.ParseURL(rssLink)

	commentBody := ""
	if len(feed.Items) > 0 {
		var lastFeedItem *gofeed.Item

		for _, item := range feed.Items {
			if lastFeedItem == nil || item.PublishedParsed.After(*lastFeedItem.PublishedParsed) {
				lastFeedItem = item
			}
		}

		if lastFeedItem != nil {
			commentBody = lastFeedItem.Description
		}
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(commentBody))
	if err != nil {
		log.Printf("[ERROR] Failed to parse comment body: %v", err)
		return ""
	}

	return strings.TrimSpace(doc.Find("span.postbody").Text())
}

func getRssLink(doc *goquery.Document) string {
	var rssLink string

	doc.Find("td a").Each(func(index int, item *goquery.Selection) {
		href, exists := item.Attr("href")
		if exists && strings.HasPrefix(href, "rss.php") {
			rssLink = href
		}
	})

	return fmt.Sprintf("%s/%s", NnmUrl, rssLink)
}
