package providers

import (
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"net/url"
	"strings"
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
	u, _ := url.Parse(originalUrl)

	return u.Query().Get("t")
}
