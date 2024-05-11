migrations-prerequisites:
	@echo "Installing sql-migrate..."
	go install github.com/rubenv/sql-migrate/...@latest

apply-migrations: migrations-prerequisites
	@echo "Applying migrations..."
	sql-migrate up

new-migration: migrations-prerequisites
	@echo "Creating new migration..."
	sql-migrate new $(name)
