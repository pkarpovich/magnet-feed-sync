package providers

import (
	"fmt"
	"github.com/PuerkitoBio/goquery"
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

func (p *NnmProvider) GetRssLink(doc *goquery.Document) string {
	var rssLink string

	doc.Find("td a").Each(func(index int, item *goquery.Selection) {
		href, exists := item.Attr("href")
		if exists && strings.HasPrefix(href, "rss.php") {
			rssLink = href
		}
	})

	return fmt.Sprintf("%s/%s", NnmUrl, rssLink)
}

func (p *NnmProvider) GetTitle(doc *goquery.Document) string {
	title := doc.Find("div.postbody span[style='font-size: 20px; line-height: normal'] span[style='font-weight: bold']").Text()
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
