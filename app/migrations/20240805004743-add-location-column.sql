
-- +migrate Up
ALTER TABLE files ADD COLUMN location TEXT NOT NULL DEFAULT '/downloads/tv shows';

-- +migrate Down
ALTER TABLE files DROP COLUMN location;
