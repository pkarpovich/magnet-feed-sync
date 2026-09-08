package watcher

import (
	"context"
	"time"
)

type Watch struct {
	ID           string
	Queries      []string
	Sources      []string
	IncludeRegex string
	ExcludeRegex string
	Rev          int
	CreatedAt    time.Time
	SeededAt     *time.Time
	ExpiresAt    *time.Time
	DisabledAt   *time.Time
	LastRunAt    *time.Time
	LastStatus   string
}

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

type SearchSource interface {
	Name() string
	Search(ctx context.Context, query string) ([]SearchResult, error)
}

const SourceExtto = "extto"

// static on purpose: a source disabled at startup must not make an existing watch unsaveable
func KnownSources() []string {
	return []string{sourceJackett, SourceExtto}
}

const seenKeySeparator = "\x00"

// both sources hand out plain integer ids, so a bare external id collides across them
func (r SearchResult) SeenKey() string {
	return r.Source + seenKeySeparator + r.ExternalID
}
