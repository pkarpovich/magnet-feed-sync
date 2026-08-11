migrations-prerequisites:
	@echo "Installing sql-migrate..."
	go install github.com/rubenv/sql-migrate/...@latest

# the runner, not `sql-migrate up`: the CLI has no adoption step, so on a database the old
# server created it records the baseline and then dies on `DROP COLUMN rss_url` — with the
# baseline recorded, the runner can no longer repair it either
apply-migrations:
	@echo "Applying migrations..."
	go run ./cmd/migrate

new-migration: migrations-prerequisites
	@echo "Creating new migration..."
	sql-migrate new $(name)
