package migrations

import (
	"database/sql"
	"path/filepath"
	"testing"

	migrate "github.com/rubenv/sql-migrate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	return db
}

func columns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()

	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	require.NoError(t, err)
	defer func() {
		require.NoError(t, rows.Close())
	}()

	var names []string
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, colType    string
			dflt             sql.NullString
		)
		require.NoError(t, rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())

	return names
}

func tableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()

	var count int
	err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&count)
	require.NoError(t, err)

	return count > 0
}

func TestApplyOnEmptyDatabase(t *testing.T) {
	db := newTestDB(t)

	applied, err := Apply(db)
	require.NoError(t, err)
	assert.Positive(t, applied)

	fileColumns := columns(t, db, "files")
	for _, name := range []string{"consecutive_failures", "last_error", "last_error_at", "last_comment", "location"} {
		assert.Contains(t, fileColumns, name)
	}
	assert.NotContains(t, fileColumns, "rss_url")

	assert.True(t, tableExists(t, db, "app_state"))
}

func TestApplyIsIdempotent(t *testing.T) {
	db := newTestDB(t)

	first, err := Apply(db)
	require.NoError(t, err)
	assert.Positive(t, first)

	second, err := Apply(db)
	require.NoError(t, err)
	assert.Equal(t, 0, second)
}

func TestApplyAddsFailureColumnsToExistingDatabase(t *testing.T) {
	db := newTestDB(t)

	source := migrate.EmbedFileSystemMigrationSource{FileSystem: files, Root: "."}
	applied, err := migrate.ExecMax(db, "sqlite3", source, migrate.Up, 4)
	require.NoError(t, err)
	require.Equal(t, 4, applied)

	before := columns(t, db, "files")
	require.NotContains(t, before, "consecutive_failures")

	_, err = db.Exec(`INSERT INTO files (id, original_url, magnet, name) VALUES (?, ?, ?, ?)`,
		"id-1", "https://example.com/topic", "magnet:?xt=urn:btih:abc", "Some Release")
	require.NoError(t, err)

	_, err = Apply(db)
	require.NoError(t, err)

	after := columns(t, db, "files")
	for _, name := range []string{"consecutive_failures", "last_error", "last_error_at"} {
		assert.Contains(t, after, name)
	}

	var name string
	var failures int
	err = db.QueryRow("SELECT name, consecutive_failures FROM files WHERE id = ?", "id-1").Scan(&name, &failures)
	require.NoError(t, err)
	assert.Equal(t, "Some Release", name)
	assert.Equal(t, 0, failures)
}
