-- +migrate Up
-- add column last_comment to files
ALTER TABLE files
    ADD COLUMN last_comment TEXT NOT NULL DEFAULT '';

-- +migrate Down
-- remove column last_comment from files
ALTER TABLE files
    DROP COLUMN last_comment;
