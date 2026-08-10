-- +migrate Up
CREATE TABLE IF NOT EXISTS app_state (key TEXT PRIMARY KEY, value TEXT);

-- +migrate Down
DROP TABLE app_state;
