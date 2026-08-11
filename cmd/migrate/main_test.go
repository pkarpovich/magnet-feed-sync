package main

import (
	"database/sql"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func appliedCount(t *testing.T) int {
	t.Helper()

	db, err := sql.Open("sqlite", ".db/tasks.db")
	require.NoError(t, err)
	defer func() {
		require.NoError(t, db.Close())
	}()

	var count int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM gorp_migrations").Scan(&count))

	return count
}

func TestRunAppliesMigrations(t *testing.T) {
	t.Chdir(t.TempDir())

	require.NoError(t, run())

	first := appliedCount(t)
	assert.Positive(t, first)

	require.NoError(t, run())
	assert.Equal(t, first, appliedCount(t), "second run must apply nothing")
}

func TestRunFailsOnUnusableDatabasePath(t *testing.T) {
	t.Chdir(t.TempDir())

	// .db as a regular file: the client cannot create the folder or the database inside it
	require.NoError(t, os.WriteFile(".db", []byte("not a folder"), 0o600))

	err := run()
	require.Error(t, err)
	assert.ErrorContains(t, err, "open database")
}

// the exit code is the whole contract with compose: a migration that cannot be applied
// must not come back as success
func TestRunFailsWhenMigrationsCannotBeApplied(t *testing.T) {
	t.Chdir(t.TempDir())

	require.NoError(t, os.Mkdir(".db", 0o755))
	db, err := sql.Open("sqlite", ".db/tasks.db")
	require.NoError(t, err)

	// a `files` table that no migration can be reconciled with, recorded as fully migrated
	_, err = db.Exec(`CREATE TABLE files (id TEXT PRIMARY KEY)`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE gorp_migrations (id varchar(255) PRIMARY KEY, applied_at datetime)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO gorp_migrations (id, applied_at) VALUES (?, CURRENT_TIMESTAMP)`,
		"20240101000000-create-files.sql")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	err = run()
	require.Error(t, err)
	assert.ErrorContains(t, err, "apply migrations")
}
