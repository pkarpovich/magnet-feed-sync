package providers

import (
	"github.com/PuerkitoBio/goquery"
	"log"
	"magnet-feed-sync/app/utils"
	"net/url"
	"strings"
	"time"
)

type RutrackerProvider struct {
}

const RutrackerUrl = "https://rutracker.org/forum"

func (p *RutrackerProvider) GetMagnetLink(doc *goquery.Document) (magnetLink string) {
	magnetLink, exists := doc.Find("a.magnet-link").Attr("href")
	if exists {
		return magnetLink
	}

	log.Printf("[WARN] magnet link not found in rutracker page")

	return magnetLink
}

func (p *RutrackerProvider) GetRssLink(doc *goquery.Document) string {
	var rssLink string

	doc.Find("a").Each(func(index int, item *goquery.Selection) {
		href, exists := item.Attr("href")
		if exists && href == "rss" {
			rssLink = href
		}
	})

	return rssLink
}

func (p *RutrackerProvider) GetTitle(doc *goquery.Document) string {
	return doc.Find("div.post_body > span + hr").Prev().Find("span.post-b").Text()
}

func (p *RutrackerProvider) GetId(originalUrl string) string {
	u, err := url.Parse(originalUrl)
	if err != nil {
		log.Printf("[ERROR] Failed to parse rutracker url: %s, %v", originalUrl, err)
		return ""
	}

	return u.Query().Get("t")
}

func (p *RutrackerProvider) GetLastUpdatedDate(doc *goquery.Document) time.Time {
	editedText := doc.Find("table#topic_main > tbody").FilterFunction(func(i int, s *goquery.Selection) bool {
		_, exists := s.Attr("id")
		if exists {
			return true
		}

		return false
	}).First().Find("p.post-time > span.posted_since").Text()

	var editedDate string
	prefix := "ред. "
	if pos := strings.Index(editedText, prefix); pos != -1 {
		editedDate = strings.TrimSpace(editedText[pos+len(prefix):])

		if len(editedDate) > 0 && editedDate[len(editedDate)-1] == ')' {
			editedDate = editedDate[:len(editedDate)-1]
		}
	}

	if len(editedDate) == 0 {
		log.Printf("[WARN] edited date not found in rutracker page")
		return time.Now()
	}

	date, err := utils.ParseRussianDate(editedDate)
	if err != nil {
		log.Printf("[ERROR] Failed to parse rutracker torrent edited date: %s, %v", editedDate, err)
		return time.Now()
	}

	return date
}
