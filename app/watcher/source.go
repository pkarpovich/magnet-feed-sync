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
	// DisabledAt is the soft delete. It is carried on the watch rather than filtered away in
	// the store, because an api that hides it answers "this watch looks fine" for a watch
	// the cron has stopped running.
	DisabledAt *time.Time
	LastRunAt  *time.Time
	LastStatus string
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

// SourceExtto names the ext.to source. It is exported because the http layer resolves
// magnets for exactly its rows — a magnet call against a jackett id would be nonsense — and
// a second spelling of the name there could silently disagree with this one.
const SourceExtto = "extto"

// KnownSources lists every source name a watch may reference. It is deliberately static
// rather than derived from the running source set: a source disabled at startup (no api
// key, no solver) must not make an existing watch unsaveable.
func KnownSources() []string {
	return []string{sourceJackett, SourceExtto}
}

const seenKeySeparator = "\x00"

// SeenKey identifies a result within a watch. Both sources hand out plain integer ids, so
// a bare external id would collide across them and swallow a real release.
func (r SearchResult) SeenKey() string {
	return r.Source + seenKeySeparator + r.ExternalID
}
