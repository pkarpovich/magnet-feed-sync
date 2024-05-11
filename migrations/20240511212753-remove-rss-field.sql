-- +migrate Up
-- remove column rss_url from files
ALTER TABLE files
    DROP COLUMN rss_url;

-- +migrate Down
-- add column rss_url to files
ALTER TABLE files
    ADD COLUMN rss_url TEXT NOT NULL;
