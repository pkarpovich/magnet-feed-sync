package providers

import (
	"github.com/PuerkitoBio/goquery"
	"time"
)

type Service interface {
	GetMagnetLink(doc *goquery.Document) string
	GetRssLink(doc *goquery.Document) string
	GetTitle(doc *goquery.Document) string
	GetId(url string) string
	GetLastUpdatedDate(doc *goquery.Document) time.Time
}
