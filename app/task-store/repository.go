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

type Repository struct {
	db *database.Client
}

func NewRepository(db *database.Client) (*Repository, error) {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS files (
    		id TEXT PRIMARY KEY,
    		original_url TEXT,
    		magnet TEXT,
    		name TEXT,
    		last_sync_at TIMESTAMP,
    		last_comment TEXT NOT NULL DEFAULT '',
    		location TEXT NOT NULL DEFAULT '/downloads/tv shows',
    		torrent_updated_at TIMESTAMP,
    		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
            delete_at TIMESTAMP DEFAULT NULL,
    		consecutive_failures INTEGER NOT NULL DEFAULT 0,
    		last_error TEXT NOT NULL DEFAULT '',
    		last_error_at TIMESTAMP
	)`)
	if err != nil {
		return nil, err
	}

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS app_state (key TEXT PRIMARY KEY, value TEXT)`)
	if err != nil {
		return nil, err
	}

	return &Repository{db: db}, nil
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

func (r *Repository) SetLastRun(at time.Time, ok bool) error {
	if err := r.setState(lastRunAtKey, at.UTC().Format(time.RFC3339)); err != nil {
		return err
	}

	return r.setState(lastRunOkKey, strconv.FormatBool(ok))
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

func (r *Repository) setState(key, value string) error {
	_, err := r.db.Exec(`INSERT OR REPLACE INTO app_state (key, value) VALUES (?, ?)`, key, value)
	if err != nil {
		return fmt.Errorf("set app state %s: %w", key, err)
	}

	return nil
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
