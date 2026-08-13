
-- +migrate Up
CREATE TABLE downloads (
    id           TEXT PRIMARY KEY,
    source       TEXT NOT NULL,
    location     TEXT NOT NULL,
    hash         TEXT NOT NULL DEFAULT '',
    name         TEXT NOT NULL DEFAULT '',
    content_path TEXT NOT NULL DEFAULT '',
    size         INTEGER NOT NULL DEFAULT 0,
    status       TEXT NOT NULL DEFAULT '',
    reason       TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    completed_at TIMESTAMP DEFAULT NULL,
    published_at TIMESTAMP DEFAULT NULL
);

ALTER TABLE files ADD COLUMN notify BOOLEAN NOT NULL DEFAULT 0;

-- +migrate Down
ALTER TABLE files DROP COLUMN notify;
DROP TABLE downloads;
