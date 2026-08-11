package migrations

import (
	"database/sql"
	"embed"
	"fmt"

	migrate "github.com/rubenv/sql-migrate"
)

//go:embed *.sql
var files embed.FS

// Apply runs every pending migration and returns how many were applied.
func Apply(db *sql.DB) (int, error) {
	source := migrate.EmbedFileSystemMigrationSource{FileSystem: files, Root: "."}

	applied, err := migrate.Exec(db, "sqlite3", source, migrate.Up)
	if err != nil {
		return applied, fmt.Errorf("apply migrations: %w", err)
	}

	return applied, nil
}
