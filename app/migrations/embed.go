package migrations

import (
	"database/sql"
	"embed"
	"fmt"

	migrate "github.com/rubenv/sql-migrate"
)

//go:embed *.sql
var files embed.FS

func source() migrate.EmbedFileSystemMigrationSource {
	return migrate.EmbedFileSystemMigrationSource{FileSystem: files, Root: "."}
}

// Apply runs every pending migration and returns how many were applied.
func Apply(db *sql.DB) (int, error) {
	if err := adoptUnmanagedSchema(db); err != nil {
		return 0, err
	}

	applied, err := migrate.Exec(db, "sqlite3", source(), migrate.Up)
	if err != nil {
		return applied, fmt.Errorf("apply migrations: %w", err)
	}

	return applied, nil
}

// A database the app built for itself with `CREATE TABLE IF NOT EXISTS` — every checkout
// that ran the server before this change — has the modern shape and no history at all.
// Replaying the set there dies on `DROP COLUMN rss_url`, and because the baseline is
// recorded first the failure is permanent. Record the ids the live schema already
// reflects instead, so only the genuinely pending ones run.
func adoptUnmanagedSchema(db *sql.DB) error {
	managed, err := tableExists(db, "gorp_migrations")
	if err != nil {
		return err
	}
	if managed {
		return nil
	}

	bootstrapped, err := tableExists(db, "files")
	if err != nil {
		return err
	}
	if !bootstrapped {
		return nil
	}

	skip, err := alreadyReflected(db)
	if err != nil {
		return err
	}

	if _, err := migrate.SkipMax(db, "sqlite3", source(), migrate.Up, skip); err != nil {
		return fmt.Errorf("adopt existing schema: %w", err)
	}

	return nil
}

// how many migrations, counted from the oldest, the live schema already satisfies. The
// app's own `CREATE TABLE` only ever grew, so what it produced is always a prefix of the
// migration set — the loop stops at the first unsatisfied one rather than skipping past it.
func alreadyReflected(db *sql.DB) (int, error) {
	columns, err := fileColumns(db)
	if err != nil {
		return 0, err
	}

	appState, err := tableExists(db, "app_state")
	if err != nil {
		return 0, err
	}

	reflected := []bool{
		true,                            // 20240101000000-create-files: files exists, checked by the caller
		!columns["rss_url"],             // 20240511212753-remove-rss-field
		columns["last_comment"],         // 20240803112540-add-last-comment-column
		columns["location"],             // 20240805004743-add-location-column
		columns["consecutive_failures"], // 20260810144014-add-failure-tracking
		appState,                        // 20260810145500-add-app-state
	}

	count := 0
	for _, done := range reflected {
		if !done {
			break
		}
		count++
	}

	return count, nil
}

func fileColumns(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('files')`)
	if err != nil {
		return nil, fmt.Errorf("read files schema: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	columns := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("read files schema: %w", err)
		}

		columns[name] = true
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read files schema: %w", err)
	}

	return columns, nil
}

func tableExists(db *sql.DB, name string) (bool, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("read %s schema: %w", name, err)
	}

	return count > 0, nil
}
