
-- +migrate Up
ALTER TABLE files ADD COLUMN last_error_kind TEXT NOT NULL DEFAULT '';

-- +migrate Down
ALTER TABLE files DROP COLUMN last_error_kind;
