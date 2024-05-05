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
    		rss_url TEXT,
    		magnet TEXT,
    		name TEXT,
    		last_sync_at TIMESTAMP,
    		torrent_updated_at TIMESTAMP,
    		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
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
				rss_url,
				magnet,
				name,
				torrent_updated_at,
				last_sync_at
			) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		metadata.ID,
		metadata.OriginalUrl,
		metadata.RssUrl,
		metadata.Magnet,
		metadata.Name,
		metadata.LastSyncAt,
		metadata.TorrentUpdatedAt,
	)

	return err
}

func (r *Repository) GetAll() ([]*tracker.FileMetadata, error) {
	rows, err := r.db.Query(`SELECT * FROM events`)
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
			&m.RssUrl,
			&m.Magnet,
			&m.Name,
			&m.LastSyncAt,
			&m.TorrentUpdatedAt,
			&m.CreatedAt,
		); err != nil {
			return nil, err
		}

		metadata = append(metadata, &m)
	}

	return metadata, nil
}
