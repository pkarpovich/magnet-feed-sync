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

func hasTable(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()

	var count int
	err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&count)
	require.NoError(t, err)

	return count > 0
}

// how many migrations the set holds. Derived rather than written out, so adding one does
// not silently turn every count assertion below into a failing literal.
func totalMigrations(t *testing.T) int {
	t.Helper()

	all, err := source().FindMigrations()
	require.NoError(t, err)

	return len(all)
}

// the migrations a database built by the old server already satisfies: everything up to
// and including 20260810145500-add-app-state. Later migrations create their own tables and
// can never be reflected by that schema.
const legacyAdopted = 6

func appliedIDs(t *testing.T, db *sql.DB) []string {
	t.Helper()

	rows, err := db.Query("SELECT id FROM gorp_migrations ORDER BY id")
	require.NoError(t, err)
	defer func() {
		require.NoError(t, rows.Close())
	}()

	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())

	return ids
}

func assertDownloadsSchema(t *testing.T, db *sql.DB) {
	t.Helper()

	require.True(t, hasTable(t, db, "downloads"))
	assert.ElementsMatch(t, []string{
		"id", "source", "location", "hash", "name", "content_path", "size",
		"status", "reason", "created_at", "completed_at", "published_at",
	}, columns(t, db, "downloads"))

	assert.Contains(t, columns(t, db, "files"), "notify")
}

func TestApplyOnEmptyDatabase(t *testing.T) {
	db := newTestDB(t)

	applied, err := Apply(db)
	require.NoError(t, err)
	assert.Equal(t, totalMigrations(t), applied)

	names := columns(t, db, "files")
	for _, name := range []string{"consecutive_failures", "last_error", "last_error_at", "last_comment", "location"} {
		assert.Contains(t, names, name)
	}
	assert.NotContains(t, names, "rss_url")

	assert.True(t, hasTable(t, db, "app_state"))
	assertDownloadsSchema(t, db)
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

	// 4 = the baseline plus the three 2024 migrations, i.e. everything before
	// 20260810144014-add-failure-tracking
	applied, err := migrate.ExecMax(db, "sqlite3", source(), migrate.Up, 4)
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

// production's history holds the three 2024 ids but not the baseline, so sql-migrate has
// to take its catch-up branch and run the baseline out of order as a no-op. Nothing else
// exercises that path: every other test records the baseline first.
func TestApplyCatchesUpWhenBaselineWasNeverRecorded(t *testing.T) {
	db := newTestDB(t)

	seedProductionSchema(t, db)

	applied, err := Apply(db)
	require.NoError(t, err)
	assert.Equal(t, totalMigrations(t)-3, applied)

	names := columns(t, db, "files")
	for _, name := range []string{"consecutive_failures", "last_error", "last_error_at"} {
		assert.Contains(t, names, name)
	}
	assert.True(t, hasTable(t, db, "app_state"))

	assert.Len(t, appliedIDs(t, db), totalMigrations(t))

	var title string
	err = db.QueryRow("SELECT name FROM files WHERE id = ?", "id-1").Scan(&title)
	require.NoError(t, err)
	assert.Equal(t, "Live Row", title)
}

// every checkout that ran the server before this change has `files` in its modern shape
// and no gorp_migrations at all. Replaying the set there used to fail permanently on
// `DROP COLUMN rss_url`.
func TestApplyAdoptsDatabaseCreatedByTheOldServer(t *testing.T) {
	db := newTestDB(t)

	seedLegacyServerSchema(t, db)

	applied, err := Apply(db)
	require.NoError(t, err)
	assert.Equal(t, totalMigrations(t)-legacyAdopted, applied)

	assert.Len(t, appliedIDs(t, db), totalMigrations(t))
	assertDownloadsSchema(t, db)

	var title string
	err = db.QueryRow("SELECT name FROM files WHERE id = ?", "id-1").Scan(&title)
	require.NoError(t, err)
	assert.Equal(t, "Legacy Row", title)

	second, err := Apply(db)
	require.NoError(t, err)
	assert.Equal(t, 0, second)
}

// the same unmanaged case, but from a server old enough to predate app_state: adoption
// must stop at the failure-tracking migration and let the last one actually run.
func TestApplyAdoptsPartialLegacySchema(t *testing.T) {
	db := newTestDB(t)

	seedLegacyServerSchema(t, db)
	_, err := db.Exec("DROP TABLE app_state")
	require.NoError(t, err)

	applied, err := Apply(db)
	require.NoError(t, err)
	assert.Equal(t, totalMigrations(t)-(legacyAdopted-1), applied)

	assert.True(t, hasTable(t, db, "app_state"))
	assert.Len(t, appliedIDs(t, db), totalMigrations(t))
}

// the historical shape: rss_url still present and nothing after it. Adoption records only
// the baseline, so every later migration still runs for real.
func TestApplyAdoptsPreRssRemovalSchema(t *testing.T) {
	db := newTestDB(t)

	_, err := db.Exec(`CREATE TABLE files (
		id TEXT PRIMARY KEY,
		original_url TEXT,
		rss_url TEXT,
		magnet TEXT,
		name TEXT,
		last_sync_at TIMESTAMP,
		torrent_updated_at TIMESTAMP,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		delete_at TIMESTAMP DEFAULT NULL
	)`)
	require.NoError(t, err)

	applied, err := Apply(db)
	require.NoError(t, err)
	assert.Equal(t, totalMigrations(t)-1, applied)

	names := columns(t, db, "files")
	assert.NotContains(t, names, "rss_url")
	assert.Contains(t, names, "consecutive_failures")
}

// adoption used to record ids one committed transaction at a time, so a kill in the middle
// left history that was neither absent nor complete — adoption never fired again and the
// leftovers replayed into `DROP COLUMN rss_url`. It writes one transaction now, so the only
// states a crash can leave are "no history" and "fully adopted".
func TestAdoptionIsAtomic(t *testing.T) {
	db := newTestDB(t)

	seedLegacyServerSchema(t, db)

	require.NoError(t, adoptUnmanagedSchema(db))

	ids := appliedIDs(t, db)
	require.Len(t, ids, legacyAdopted)

	var recorded int
	err := db.QueryRow("SELECT COUNT(*) FROM gorp_migrations").Scan(&recorded)
	require.NoError(t, err)
	assert.Equal(t, len(ids), recorded)
}

// sql-migrate creates gorp_migrations before it records anything, so an aborted run can
// leave the table empty. An empty table is not history: adoption must still run, or the
// database is stuck replaying migrations its columns already satisfy.
func TestApplyAdoptsDespiteEmptyMigrationTable(t *testing.T) {
	db := newTestDB(t)

	seedLegacyServerSchema(t, db)
	_, err := db.Exec(`CREATE TABLE gorp_migrations (id varchar(255) NOT NULL PRIMARY KEY, applied_at datetime)`)
	require.NoError(t, err)

	applied, err := Apply(db)
	require.NoError(t, err)
	assert.Equal(t, totalMigrations(t)-legacyAdopted, applied)

	assert.Len(t, appliedIDs(t, db), totalMigrations(t))

	var title string
	err = db.QueryRow("SELECT name FROM files WHERE id = ?", "id-1").Scan(&title)
	require.NoError(t, err)
	assert.Equal(t, "Legacy Row", title)
}

// the rows adoption writes have to be readable by sql-migrate itself — it selects them
// back into a time.Time on every subsequent run.
func TestAdoptedRecordsAreReadableBySqlMigrate(t *testing.T) {
	db := newTestDB(t)

	seedLegacyServerSchema(t, db)
	require.NoError(t, adoptUnmanagedSchema(db))

	records, err := migrate.GetMigrationRecords(db, "sqlite3")
	require.NoError(t, err)
	require.Len(t, records, legacyAdopted)
	for _, record := range records {
		assert.NotEmpty(t, record.Id)
		assert.False(t, record.AppliedAt.IsZero())
	}
}

// the production shape at the time of the incident: 10 columns, no failure tracking,
// app_state already created by the old NewRepository, three 2024 ids in gorp_migrations.
func seedProductionSchema(t *testing.T, db *sql.DB) {
	t.Helper()

	_, err := db.Exec(`CREATE TABLE files (
		id TEXT PRIMARY KEY,
		original_url TEXT,
		magnet TEXT,
		name TEXT,
		last_sync_at TIMESTAMP,
		last_comment TEXT NOT NULL DEFAULT '',
		location TEXT NOT NULL DEFAULT '/downloads/tv shows',
		torrent_updated_at TIMESTAMP,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		delete_at TIMESTAMP DEFAULT NULL
	)`)
	require.NoError(t, err)

	_, err = db.Exec(`CREATE TABLE app_state (key TEXT PRIMARY KEY, value TEXT)`)
	require.NoError(t, err)

	_, err = db.Exec(`CREATE TABLE gorp_migrations (id varchar(255) PRIMARY KEY, applied_at datetime)`)
	require.NoError(t, err)

	for _, id := range []string{
		"20240511212753-remove-rss-field.sql",
		"20240803112540-add-last-comment-column.sql",
		"20240805004743-add-location-column.sql",
	} {
		_, err = db.Exec("INSERT INTO gorp_migrations (id, applied_at) VALUES (?, CURRENT_TIMESTAMP)", id)
		require.NoError(t, err)
	}

	_, err = db.Exec(`INSERT INTO files (id, original_url, magnet, name) VALUES (?, ?, ?, ?)`,
		"id-1", "https://example.com/topic", "magnet:?xt=urn:btih:abc", "Live Row")
	require.NoError(t, err)
}

// what master's NewRepository created: the full modern shape, and no migration history.
func seedLegacyServerSchema(t *testing.T, db *sql.DB) {
	t.Helper()

	_, err := db.Exec(`CREATE TABLE files (
		id TEXT PRIMARY KEY,
		original_url TEXT,
		magnet TEXT,
		name TEXT,
		last_sync_at TIMESTAMP,
		last_comment TEXT NOT NULL DEFAULT '',
		location TEXT NOT NULL DEFAULT '/downloads/tv shows',
		torrent_updated_at TIMESTAMP,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		delete_at TIMESTAMP DEFAULT NULL,
		consecutive_failures INTEGER NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT '',
		last_error_at TIMESTAMP
	)`)
	require.NoError(t, err)

	_, err = db.Exec(`CREATE TABLE app_state (key TEXT PRIMARY KEY, value TEXT)`)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO files (id, original_url, magnet, name) VALUES (?, ?, ?, ?)`,
		"id-1", "https://example.com/topic", "magnet:?xt=urn:btih:abc", "Legacy Row")
	require.NoError(t, err)
}
