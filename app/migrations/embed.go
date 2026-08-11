package migrations

import (
	"database/sql"
	"embed"
	"fmt"
	"time"

	migrate "github.com/rubenv/sql-migrate"
)

//go:embed *.sql
var files embed.FS

func source() migrate.EmbedFileSystemMigrationSource {
	return migrate.EmbedFileSystemMigrationSource{FileSystem: files, Root: "."}
}

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

// A database the old server built for itself has the modern shape and no history, so
// replaying the set there dies on `DROP COLUMN rss_url` — permanently, because the
// baseline is recorded first. Record the ids the live schema already reflects instead.
func adoptUnmanagedSchema(db *sql.DB) error {
	bootstrapped, err := tableExists(db, "files")
	if err != nil {
		return err
	}
	if !bootstrapped {
		return nil
	}

	managed, err := hasMigrationHistory(db)
	if err != nil {
		return err
	}
	if managed {
		return nil
	}

	skip, err := alreadyReflected(db)
	if err != nil {
		return err
	}

	return recordAdopted(db, skip)
}

// one transaction, not `migrate.SkipMax`: that commits per record, so an interrupt leaves
// history neither absent nor complete — adoption never fires again and the leftovers
// replay into the `DROP COLUMN rss_url` failure this exists to avoid.
func recordAdopted(db *sql.DB, count int) error {
	all, err := source().FindMigrations()
	if err != nil {
		return fmt.Errorf("adopt existing schema: %w", err)
	}
	if count > len(all) {
		count = len(all)
	}
	if count == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("adopt existing schema: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	// the shape sql-migrate creates for sqlite3; `IF NOT EXISTS` so an empty table left
	// behind by an aborted run is reused rather than fought over
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS gorp_migrations (id varchar(255) NOT NULL PRIMARY KEY, applied_at datetime)`); err != nil {
		return fmt.Errorf("adopt existing schema: %w", err)
	}

	now := time.Now()
	for _, m := range all[:count] {
		if _, err := tx.Exec(`INSERT INTO gorp_migrations (id, applied_at) VALUES (?, ?)`, m.Id, now); err != nil {
			return fmt.Errorf("adopt existing schema: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("adopt existing schema: %w", err)
	}

	return nil
}

// a table with no rows is not history: sql-migrate creates it before recording anything,
// so an aborted run can leave it empty and there is nothing to hand over to sql-migrate.
func hasMigrationHistory(db *sql.DB) (bool, error) {
	present, err := tableExists(db, "gorp_migrations")
	if err != nil || !present {
		return false, err
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM gorp_migrations`).Scan(&count); err != nil {
		return false, fmt.Errorf("read migration history: %w", err)
	}

	return count > 0, nil
}

// how many migrations, from the oldest, the live schema already satisfies. The old
// `CREATE TABLE` only ever grew, so its result is always a prefix of the set.
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
