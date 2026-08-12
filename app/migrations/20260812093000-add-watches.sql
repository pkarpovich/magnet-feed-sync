
-- +migrate Up
CREATE TABLE watches (
    id            TEXT PRIMARY KEY,
    queries       TEXT NOT NULL,
    include_regex TEXT NOT NULL DEFAULT '',
    exclude_regex TEXT NOT NULL DEFAULT '',
    sources       TEXT NOT NULL DEFAULT 'jackett,extto',
    rev           INTEGER NOT NULL DEFAULT 1,
    seeded_at     TIMESTAMP DEFAULT NULL,
    created_at    TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    expires_at    TIMESTAMP DEFAULT NULL,
    last_run_at   TIMESTAMP DEFAULT NULL,
    last_status   TEXT NOT NULL DEFAULT '',
    disabled_at   TIMESTAMP DEFAULT NULL
);

CREATE TABLE watch_seen (
    watch_id      TEXT NOT NULL,
    source        TEXT NOT NULL,
    external_id   TEXT NOT NULL,
    title         TEXT NOT NULL,
    first_seen_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (watch_id, source, external_id)
);

-- +migrate Down
DROP TABLE watch_seen;
DROP TABLE watches;
