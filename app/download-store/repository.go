package download_store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"magnet-feed-sync/app/database"
	"magnet-feed-sync/app/downloads"
)

var ErrSchemaNotInitialised = errors.New("download schema not initialised: run the migrate binary (`go run ./cmd/migrate`)")

const downloadColumns = `id, source, location, hash, name, content_path, size, status, reason, created_at, completed_at, published_at`

// column by column, not table by table: a table check passes a table whose columns a
// half-applied migration never added
var requiredColumns = []string{
	"id", "source", "location", "hash", "name", "content_path", "size", "status", "reason",
	"created_at", "completed_at", "published_at",
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
	// before the columns: PRAGMA table_info on a missing table returns no rows and no error
	if err := r.requireTable("downloads"); err != nil {
		return err
	}

	for _, column := range requiredColumns {
		found, err := r.count(`SELECT COUNT(*) FROM pragma_table_info('downloads') WHERE name = ?`, column)
		if err != nil {
			return err
		}
		if found == 0 {
			return fmt.Errorf("downloads.%s is missing: %w", column, ErrSchemaNotInitialised)
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

// created_at is written from Go rather than left to CURRENT_TIMESTAMP, whose one-second
// resolution would make the ordering of two rows added in the same second arbitrary. It is
// stored in UTC because the driver writes a time.Time as RFC3339 text carrying its offset, and
// `ORDER BY created_at` then compares wall clocks: under a DST zone the autumn rollback hour
// would sort a newer row before an older one, which no rowid tiebreak can repair
func (r *Repository) Create(d *downloads.Download) error {
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now()
	}
	d.CreatedAt = d.CreatedAt.UTC()

	_, err := r.db.Exec(`
		INSERT INTO downloads (`+downloadColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		d.ID,
		d.Source,
		d.Location,
		d.Hash,
		d.Name,
		d.ContentPath,
		d.Size,
		d.Status,
		d.Reason,
		d.CreatedAt,
		nullTime(d.CompletedAt),
		nullTime(d.PublishedAt),
	)
	if err != nil {
		return fmt.Errorf("create download: %w", err)
	}

	return nil
}

func (r *Repository) Pending() ([]*downloads.Download, error) {
	return r.list(`
		SELECT ` + downloadColumns + `
		FROM downloads
		WHERE published_at IS NULL
		ORDER BY created_at ASC, rowid ASC
	`)
}

// the published_at guard is what makes publish-then-mark safe to run twice: a repeat after a
// crash-retry updates nothing and reports no error
func (r *Repository) MarkPublished(id string, o downloads.Outcome) error {
	_, err := r.db.Exec(`
		UPDATE downloads
		SET status = ?, reason = ?, name = ?, content_path = ?, size = ?, completed_at = ?, published_at = ?
		WHERE id = ? AND published_at IS NULL
	`, o.Status, o.Reason, o.Name, o.ContentPath, o.Size, o.CompletedAt.UTC(), time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("mark download %s published: %w", id, err)
	}

	return nil
}

func (r *Repository) GetByID(id string) (*downloads.Download, error) {
	return r.scanOne(r.db.QueryRow(`SELECT `+downloadColumns+` FROM downloads WHERE id = ?`, id))
}

func (r *Repository) NewestBySource(source string) (*downloads.Download, error) {
	return r.scanOne(r.db.QueryRow(`
		SELECT `+downloadColumns+`
		FROM downloads
		WHERE source = ?
		ORDER BY created_at DESC, rowid DESC
		LIMIT 1
	`, source))
}

func (r *Repository) CountPending() (int, error) {
	return r.count(`SELECT COUNT(*) FROM downloads WHERE published_at IS NULL`)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (r *Repository) scanOne(row rowScanner) (*downloads.Download, error) {
	d, err := r.scanDownload(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return d, nil
}

func (r *Repository) scanDownload(row rowScanner) (*downloads.Download, error) {
	var (
		d                        downloads.Download
		completedAt, publishedAt sql.NullTime
	)

	err := row.Scan(
		&d.ID,
		&d.Source,
		&d.Location,
		&d.Hash,
		&d.Name,
		&d.ContentPath,
		&d.Size,
		&d.Status,
		&d.Reason,
		&d.CreatedAt,
		&completedAt,
		&publishedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}

		return nil, fmt.Errorf("load download: %w", err)
	}

	d.CompletedAt = timePtr(completedAt)
	d.PublishedAt = timePtr(publishedAt)

	return &d, nil
}

func (r *Repository) list(query string) ([]*downloads.Download, error) {
	rows, err := r.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("load downloads: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.Error("failed to close rows", "error", err)
		}
	}()

	var list []*downloads.Download
	for rows.Next() {
		d, err := r.scanDownload(rows)
		if err != nil {
			return nil, err
		}

		list = append(list, d)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load downloads: %w", err)
	}

	return list, nil
}

func nullTime(at *time.Time) sql.NullTime {
	if at == nil {
		return sql.NullTime{}
	}

	return sql.NullTime{Time: at.UTC(), Valid: true}
}

func timePtr(at sql.NullTime) *time.Time {
	if !at.Valid {
		return nil
	}

	value := at.Time

	return &value
}
