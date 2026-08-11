-- +migrate Up
CREATE TABLE IF NOT EXISTS files (
    id TEXT PRIMARY KEY,
    original_url TEXT,
    rss_url TEXT,
    magnet TEXT,
    name TEXT,
    last_sync_at TIMESTAMP,
    torrent_updated_at TIMESTAMP,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    delete_at TIMESTAMP DEFAULT NULL
);

-- +migrate Down
DROP TABLE files;
