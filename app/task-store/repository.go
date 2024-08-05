package task_store

import (
	"log"
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
            delete_at TIMESTAMP DEFAULT NULL
	)`)
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
				delete_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		metadata.ID,
		metadata.OriginalUrl,
		metadata.Magnet,
		metadata.Name,
		metadata.LastSyncAt,
		metadata.LastComment,
		metadata.TorrentUpdatedAt,
		metadata.Location,
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
			delete_at
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
			log.Printf("[ERROR] failed to close rows: %s", err)
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
			delete_at
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
	)
	if err != nil {
		return nil, err
	}

	return &m, nil
}

func (r *Repository) Remove(id string) error {
	_, err := r.db.Exec(`UPDATE files SET delete_at = CURRENT_TIMESTAMP WHERE id = ?`, id)
	return err
}
