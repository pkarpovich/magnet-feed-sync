package providers

import "github.com/PuerkitoBio/goquery"

type Service interface {
	GetMagnetLink(doc *goquery.Document) string
	GetRssLink(doc *goquery.Document) string
	GetTitle(doc *goquery.Document) string
	GetId(url string) string
}
