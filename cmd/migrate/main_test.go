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

	require.Error(t, run())
}
