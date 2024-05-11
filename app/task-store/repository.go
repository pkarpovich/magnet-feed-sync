package task_store

import (
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
				torrent_updated_at,
				delete_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, NULL)`,
		metadata.ID,
		metadata.OriginalUrl,
		metadata.Magnet,
		metadata.Name,
		metadata.LastSyncAt,
		metadata.TorrentUpdatedAt,
	)

	return err
}

func (r *Repository) GetAll() ([]*tracker.FileMetadata, error) {
	rows, err := r.db.Query(`SELECT * FROM files WHERE delete_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var metadata []*tracker.FileMetadata
	for rows.Next() {
		var m tracker.FileMetadata
		if err := rows.Scan(
			&m.ID,
			&m.OriginalUrl,
			&m.Magnet,
			&m.Name,
			&m.LastSyncAt,
			&m.TorrentUpdatedAt,
			&m.CreatedAt,
			&m.DeleteAt,
		); err != nil {
			return nil, err
		}

		metadata = append(metadata, &m)
	}

	return metadata, nil
}

func (r *Repository) Remove(id string) error {
	_, err := r.db.Exec(`UPDATE files SET delete_at = CURRENT_TIMESTAMP WHERE id = ?`, id)
	return err
}
