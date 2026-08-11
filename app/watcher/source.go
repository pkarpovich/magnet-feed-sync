package watcher

import (
	"context"
	"time"
)

// Watch is a saved hunt: the queries to run, the sources to run them against, the two
// regex filters and the bookkeeping the cron cycle keeps.
type Watch struct {
	ID           string
	Queries      []string
	Sources      []string
	IncludeRegex string
	ExcludeRegex string
	Rev          int
	SeededAt     *time.Time
	ExpiresAt    *time.Time
	LastRunAt    *time.Time
	LastStatus   string
}

// SearchResult is the single currency the engine, the API and the NATS payload speak.
// Query is the query that produced the row: the ext.to magnet call signs with tokens from
// that query's search page, and a watch holds a list of queries.
type SearchResult struct {
	Source      string
	ExternalID  string
	Title       string
	PageURL     string
	DownloadURL string
	Query       string
	Seeders     int
	PublishedAt time.Time
}

// SearchSource is one indexer a watch can be run against.
type SearchSource interface {
	Name() string
	Search(ctx context.Context, query string) ([]SearchResult, error)
}

const seenKeySeparator = "\x00"

// SeenKey identifies a result within a watch. Both sources hand out plain integer ids, so
// a bare external id would collide across them and swallow a real release.
func (r SearchResult) SeenKey() string {
	return r.Source + seenKeySeparator + r.ExternalID
}
