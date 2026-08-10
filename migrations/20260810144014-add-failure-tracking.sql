-- +migrate Up
ALTER TABLE files ADD COLUMN consecutive_failures INTEGER NOT NULL DEFAULT 0;
ALTER TABLE files ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE files ADD COLUMN last_error_at TIMESTAMP;

-- +migrate Down
ALTER TABLE files DROP COLUMN consecutive_failures;
ALTER TABLE files DROP COLUMN last_error;
ALTER TABLE files DROP COLUMN last_error_at;
