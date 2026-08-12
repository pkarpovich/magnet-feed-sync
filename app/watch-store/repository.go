package watch_store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"magnet-feed-sync/app/database"
	"magnet-feed-sync/app/watcher"
)

// the schema is declared once, in the migrations; a constructor that also created the
// tables is what let a half-applied schema reach production
var ErrSchemaNotInitialised = errors.New("watch schema not initialised: run the migrate binary (`go run ./cmd/migrate`)")

var ErrNotFound = errors.New("watch not found")

const watchColumns = `id, queries, include_regex, exclude_regex, sources, rev, seeded_at, expires_at, disabled_at, last_run_at, last_status`

// requiredColumns is checked column by column rather than table by table, for the reason
// task-store does the same: the incident behind that check had the table present and the
// columns missing, which a table check passes.
var requiredColumns = map[string][]string{
	"watches":    {"id", "queries", "include_regex", "exclude_regex", "sources", "rev", "seeded_at", "expires_at", "disabled_at", "last_run_at", "last_status"},
	"watch_seen": {"watch_id", "source", "external_id", "title", "first_seen_at"},
}

// SeenRow is one already-announced release, rendered by GET /api/watches/{id} so a silent
// seed is inspectable.
type SeenRow struct {
	Source      string
	ExternalID  string
	Title       string
	FirstSeenAt time.Time
}

type Repository struct {
	db *database.Client
}

func NewRepository(db *database.Client) (*Repository, error) {
	r := &Repository{db: db}

	if err := r.verifySchema(); err != nil {
		return nil, err
	}

	return r, nil
}

func (r *Repository) verifySchema() error {
	for _, table := range []string{"watches", "watch_seen"} {
		// checked before the columns: PRAGMA table_info on a missing table returns no rows
		// and no error, which would report an absent database as an absent column
		if err := r.requireTable(table); err != nil {
			return err
		}

		for _, column := range requiredColumns[table] {
			found, err := r.count(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column)
			if err != nil {
				return err
			}
			if found == 0 {
				return fmt.Errorf("%s.%s is missing: %w", table, column, ErrSchemaNotInitialised)
			}
		}
	}

	return nil
}

func (r *Repository) requireTable(name string) error {
	found, err := r.count(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name)
	if err != nil {
		return err
	}
	if found == 0 {
		return fmt.Errorf("table %s is missing: %w", name, ErrSchemaNotInitialised)
	}

	return nil
}

func (r *Repository) count(query string, args ...any) (int, error) {
	var count int
	if err := r.db.QueryRow(query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("read schema: %w", err)
	}

	return count, nil
}

func (r *Repository) Create(w *watcher.Watch) error {
	queries, err := marshalQueries(w.Queries)
	if err != nil {
		return err
	}

	_, err = r.db.Exec(`
		INSERT INTO watches (id, queries, include_regex, exclude_regex, sources, rev, expires_at)
		VALUES (?, ?, ?, ?, ?, 1, ?)
	`, w.ID, queries, w.IncludeRegex, w.ExcludeRegex, strings.Join(w.Sources, ","), nullTime(w.ExpiresAt))
	if err != nil {
		return fmt.Errorf("create watch: %w", err)
	}

	return nil
}

func (r *Repository) Update(w *watcher.Watch) error {
	queries, err := marshalQueries(w.Queries)
	if err != nil {
		return err
	}

	res, err := r.db.Exec(`
		UPDATE watches
		SET
			queries = ?,
			include_regex = ?,
			exclude_regex = ?,
			sources = ?,
			expires_at = ?,
			rev = rev + 1
		WHERE
			id = ?
	`, queries, w.IncludeRegex, w.ExcludeRegex, strings.Join(w.Sources, ","), nullTime(w.ExpiresAt), w.ID)
	if err != nil {
		return fmt.Errorf("update watch: %w", err)
	}

	return r.requireAffected(res, w.ID)
}

func (r *Repository) Disable(id string) error {
	res, err := r.db.Exec(`UPDATE watches SET disabled_at = CURRENT_TIMESTAMP WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("disable watch: %w", err)
	}

	return r.requireAffected(res, id)
}

// Enable clears the soft delete. Without it a removed or expired id is retired for good:
// the row still exists, so a re-create is a conflict, and nothing else ever writes the
// column back to NULL.
func (r *Repository) Enable(id string) error {
	res, err := r.db.Exec(`UPDATE watches SET disabled_at = NULL WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("enable watch: %w", err)
	}

	return r.requireAffected(res, id)
}

func (r *Repository) MarkSeeded(id string) error {
	res, err := r.db.Exec(`UPDATE watches SET seeded_at = CURRENT_TIMESTAMP WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("mark watch seeded: %w", err)
	}

	return r.requireAffected(res, id)
}

func (r *Repository) RecordRun(watchID, status string) error {
	res, err := r.db.Exec(`
		UPDATE watches SET last_run_at = CURRENT_TIMESTAMP, last_status = ? WHERE id = ?
	`, status, watchID)
	if err != nil {
		return fmt.Errorf("record watch run: %w", err)
	}

	return r.requireAffected(res, watchID)
}

func (r *Repository) requireAffected(res sql.Result, id string) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update watch %s: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("watch %s: %w", id, ErrNotFound)
	}

	return nil
}

func (r *Repository) GetAll() ([]*watcher.Watch, error) {
	return r.list(`SELECT ` + watchColumns + ` FROM watches ORDER BY id`)
}

// WatchesForCycle filters on disabled_at only: an expired watch must still reach the cycle
// so it can be skipped *and disabled* there, otherwise disabled_at is never set.
func (r *Repository) WatchesForCycle() ([]*watcher.Watch, error) {
	return r.list(`SELECT ` + watchColumns + ` FROM watches WHERE disabled_at IS NULL ORDER BY id`)
}

func (r *Repository) list(query string) ([]*watcher.Watch, error) {
	rows, err := r.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("load watches: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.Error("failed to close rows", "error", err)
		}
	}()

	var watches []*watcher.Watch
	for rows.Next() {
		w, err := r.scanWatch(rows)
		if err != nil {
			return nil, err
		}

		watches = append(watches, w)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load watches: %w", err)
	}

	return watches, nil
}

func (r *Repository) GetByID(id string) (*watcher.Watch, error) {
	w, err := r.scanWatch(r.db.QueryRow(`SELECT `+watchColumns+` FROM watches WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("watch %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}

	return w, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (r *Repository) scanWatch(row rowScanner) (*watcher.Watch, error) {
	var (
		w                                          watcher.Watch
		queries, sources                           string
		seededAt, expiresAt, disabledAt, lastRunAt sql.NullTime
	)

	err := row.Scan(
		&w.ID,
		&queries,
		&w.IncludeRegex,
		&w.ExcludeRegex,
		&sources,
		&w.Rev,
		&seededAt,
		&expiresAt,
		&disabledAt,
		&lastRunAt,
		&w.LastStatus,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}

		return nil, fmt.Errorf("load watch: %w", err)
	}

	if err := json.Unmarshal([]byte(queries), &w.Queries); err != nil {
		return nil, fmt.Errorf("parse queries of watch %s: %w", w.ID, err)
	}

	w.Sources = splitSources(sources)
	w.SeededAt = timePtr(seededAt)
	w.ExpiresAt = timePtr(expiresAt)
	w.DisabledAt = timePtr(disabledAt)
	w.LastRunAt = timePtr(lastRunAt)

	return &w, nil
}

func (r *Repository) SeenKeys(watchID string) (map[string]struct{}, error) {
	rows, err := r.db.Query(`SELECT source, external_id FROM watch_seen WHERE watch_id = ?`, watchID)
	if err != nil {
		return nil, fmt.Errorf("load seen keys: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.Error("failed to close rows", "error", err)
		}
	}()

	keys := make(map[string]struct{})
	for rows.Next() {
		var seen watcher.SearchResult
		if err := rows.Scan(&seen.Source, &seen.ExternalID); err != nil {
			return nil, fmt.Errorf("load seen keys: %w", err)
		}

		keys[seen.SeenKey()] = struct{}{}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load seen keys: %w", err)
	}

	return keys, nil
}

func (r *Repository) SeenRows(watchID string) ([]SeenRow, error) {
	rows, err := r.db.Query(`
		SELECT source, external_id, title, first_seen_at
		FROM watch_seen
		WHERE watch_id = ?
		ORDER BY first_seen_at, source, external_id
	`, watchID)
	if err != nil {
		return nil, fmt.Errorf("load seen rows: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.Error("failed to close rows", "error", err)
		}
	}()

	var seen []SeenRow
	for rows.Next() {
		var row SeenRow
		if err := rows.Scan(&row.Source, &row.ExternalID, &row.Title, &row.FirstSeenAt); err != nil {
			return nil, fmt.Errorf("load seen rows: %w", err)
		}

		seen = append(seen, row)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load seen rows: %w", err)
	}

	return seen, nil
}

func (r *Repository) MarkSeen(watchID string, results []watcher.SearchResult) error {
	for _, res := range results {
		_, err := r.db.Exec(`
			INSERT OR IGNORE INTO watch_seen (watch_id, source, external_id, title) VALUES (?, ?, ?, ?)
		`, watchID, res.Source, res.ExternalID, res.Title)
		if err != nil {
			return fmt.Errorf("mark seen: %w", err)
		}
	}

	return nil
}

func marshalQueries(queries []string) (string, error) {
	if queries == nil {
		queries = []string{}
	}

	encoded, err := json.Marshal(queries)
	if err != nil {
		return "", fmt.Errorf("encode queries: %w", err)
	}

	return string(encoded), nil
}

func splitSources(raw string) []string {
	var sources []string
	for _, name := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			sources = append(sources, trimmed)
		}
	}

	return sources
}

func nullTime(at *time.Time) sql.NullTime {
	if at == nil {
		return sql.NullTime{}
	}

	return sql.NullTime{Time: *at, Valid: true}
}

func timePtr(at sql.NullTime) *time.Time {
	if !at.Valid {
		return nil
	}

	value := at.Time

	return &value
}
