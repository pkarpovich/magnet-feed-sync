package task_store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"magnet-feed-sync/app/database"
	"magnet-feed-sync/app/tracker"
)

// deliberately not `sql-migrate up`: the CLI skips adoption and can poison an unmanaged database
var ErrSchemaNotInitialised = errors.New("database schema not initialised: run the migrate binary (`go run ./cmd/migrate`)")

// the columns whose absence caused the incident: the table existed, they did not
var requiredFileColumns = []string{"consecutive_failures", "last_error", "last_error_at"}

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
	// checked before the columns: PRAGMA table_info on a missing table returns no rows
	// and no error, which would report an absent database as an absent column
	if err := r.requireTable("files"); err != nil {
		return err
	}

	for _, name := range requiredFileColumns {
		found, err := r.count(`SELECT COUNT(*) FROM pragma_table_info('files') WHERE name = ?`, name)
		if err != nil {
			return err
		}
		if found == 0 {
			return fmt.Errorf("files.%s is missing: %w", name, ErrSchemaNotInitialised)
		}
	}

	return r.requireTable("app_state")
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

func (r *Repository) count(query, arg string) (int, error) {
	var count int
	if err := r.db.QueryRow(query, arg).Scan(&count); err != nil {
		return 0, fmt.Errorf("read schema: %w", err)
	}

	return count, nil
}

func (r *Repository) CreateOrReplace(metadata *tracker.FileMetadata) error {
	_, err := r.db.Exec(`INSERT OR REPLACE INTO files (
				id,
				original_url,
				magnet,
				name,
				last_sync_at,
				last_comment,
				torrent_updated_at,
				location,
				delete_at,
				consecutive_failures,
				last_error,
				last_error_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?)`,
		metadata.ID,
		metadata.OriginalUrl,
		metadata.Magnet,
		metadata.Name,
		metadata.LastSyncAt,
		metadata.LastComment,
		metadata.TorrentUpdatedAt,
		metadata.Location,
		metadata.ConsecutiveFailures,
		metadata.LastError,
		metadata.LastErrorAt,
	)

	return err
}

func (r *Repository) GetAll() ([]*tracker.FileMetadata, error) {
	rows, err := r.db.Query(`
		SELECT
			id,
			original_url,
			magnet,
			name,
			last_comment,
			last_sync_at,
			torrent_updated_at,
			location,
			created_at,
			delete_at,
			consecutive_failures,
			last_error,
			last_error_at
		FROM
			files
		WHERE
			delete_at IS NULL
		ORDER BY torrent_updated_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer func() {
		err := rows.Close()
		if err != nil {
			slog.Error("failed to close rows", "error", err)
		}
	}()

	var metadata []*tracker.FileMetadata
	for rows.Next() {
		var m tracker.FileMetadata
		if err := rows.Scan(
			&m.ID,
			&m.OriginalUrl,
			&m.Magnet,
			&m.Name,
			&m.LastComment,
			&m.LastSyncAt,
			&m.TorrentUpdatedAt,
			&m.Location,
			&m.CreatedAt,
			&m.DeleteAt,
			&m.ConsecutiveFailures,
			&m.LastError,
			&m.LastErrorAt,
		); err != nil {
			return nil, err
		}

		metadata = append(metadata, &m)
	}

	// without this a driver error mid-iteration yields a silently truncated list, which the
	// sweep would treat as the full set and the health endpoint would count as the truth
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read files: %w", err)
	}

	return metadata, nil
}

func (r *Repository) GetById(id string) (*tracker.FileMetadata, error) {
	var m tracker.FileMetadata
	err := r.db.QueryRow(`
		SELECT
			id,
			original_url,
			magnet,
			name,
			last_comment,
			last_sync_at,
			torrent_updated_at,
			location,
			created_at,
			delete_at,
			consecutive_failures,
			last_error,
			last_error_at
		FROM
			files
		WHERE
			id = ?
	`, id).Scan(
		&m.ID,
		&m.OriginalUrl,
		&m.Magnet,
		&m.Name,
		&m.LastComment,
		&m.LastSyncAt,
		&m.TorrentUpdatedAt,
		&m.Location,
		&m.CreatedAt,
		&m.DeleteAt,
		&m.ConsecutiveFailures,
		&m.LastError,
		&m.LastErrorAt,
	)
	if err != nil {
		return nil, err
	}

	return &m, nil
}

type SyncFailure struct {
	Text string
	At   time.Time
}

func (r *Repository) RecordSyncSuccess(id string, syncedAt time.Time) error {
	_, err := r.db.Exec(`
		UPDATE files
		SET
			consecutive_failures = 0,
			last_error = '',
			last_error_at = NULL,
			last_sync_at = ?
		WHERE
			id = ?
	`, syncedAt, id)
	if err != nil {
		return fmt.Errorf("record sync success: %w", err)
	}

	return nil
}

func (r *Repository) RecordSyncFailure(id string, failure SyncFailure) error {
	_, err := r.db.Exec(`
		UPDATE files
		SET
			consecutive_failures = consecutive_failures + 1,
			last_error = ?,
			last_error_at = ?
		WHERE
			id = ?
	`, failure.Text, failure.At, id)
	if err != nil {
		return fmt.Errorf("record sync failure: %w", err)
	}

	return nil
}

const (
	lastRunAtKey = "last_run_at"
	lastRunOkKey = "last_run_ok"
)

// SetLastRun writes both keys in one statement: a half-applied pair would pair a fresh
// timestamp with the previous run's ok flag and mislead the health endpoint until the next sweep.
func (r *Repository) SetLastRun(at time.Time, ok bool) error {
	_, err := r.db.Exec(
		`INSERT OR REPLACE INTO app_state (key, value) VALUES (?, ?), (?, ?)`,
		lastRunAtKey, at.UTC().Format(time.RFC3339),
		lastRunOkKey, strconv.FormatBool(ok),
	)
	if err != nil {
		return fmt.Errorf("set last run: %w", err)
	}

	return nil
}

func (r *Repository) GetLastRun() (time.Time, bool, error) {
	rawAt, err := r.state(lastRunAtKey)
	if err != nil {
		return time.Time{}, false, err
	}
	if rawAt == "" {
		return time.Time{}, false, nil
	}

	at, err := time.Parse(time.RFC3339, rawAt)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse last run at: %w", err)
	}

	rawOk, err := r.state(lastRunOkKey)
	if err != nil {
		return time.Time{}, false, err
	}

	return at, rawOk == "true", nil
}

func (r *Repository) state(key string) (string, error) {
	var value string
	err := r.db.QueryRow(`SELECT value FROM app_state WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get app state %s: %w", key, err)
	}

	return value, nil
}

func (r *Repository) Remove(id string) error {
	_, err := r.db.Exec(`UPDATE files SET delete_at = CURRENT_TIMESTAMP WHERE id = ?`, id)
	return err
}
