package task_store

import (
	"magnet-feed-sync/app/database"
	"magnet-feed-sync/app/tracker"
)

type Repository struct {
	db *database.Client
}

func NewRepository(db *database.Client) (*Repository, error) {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS events (
    		id TEXT PRIMARY KEY,
    		original_url TEXT,
    		rss_url TEXT,
    		magnet TEXT,
    		name TEXT,
    		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	)`)
	if err != nil {
		return nil, err
	}

	return &Repository{db: db}, nil
}

func (r *Repository) CreateOrReplace(metadata *tracker.FileMetadata) error {
	_, err := r.db.Exec(`INSERT OR REPLACE INTO events (
			id,
			original_url,
			rss_url,
			magnet,
			name,
			created_at
			) VALUES (?, ?, ?, ?, ?, ?)`,
		metadata.ID,
		metadata.OriginalUrl,
		metadata.RssUrl,
		metadata.Magnet,
		metadata.Name,
		metadata.CreatedAt,
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
		if err := rows.Scan(&m.ID, &m.OriginalUrl, &m.RssUrl, &m.Magnet, &m.Name, &m.CreatedAt); err != nil {
			return nil, err
		}

		metadata = append(metadata, &m)
	}

	return metadata, nil
}
