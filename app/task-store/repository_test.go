package task_store

import (
	"database/sql"
	"testing"
	"time"

	"magnet-feed-sync/app/database"
	"magnet-feed-sync/app/migrations"
	"magnet-feed-sync/app/tracker"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestDB(t *testing.T) *database.Client {
	t.Helper()

	t.Chdir(t.TempDir())

	db, err := database.NewClient("test.db")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	return db
}

func newTestRepo(t *testing.T) *Repository {
	t.Helper()

	db := newTestDB(t)

	_, err := migrations.Apply(db.DB())
	require.NoError(t, err)

	repo, err := NewRepository(db)
	require.NoError(t, err)

	return repo
}

func testMetadata() *tracker.FileMetadata {
	return &tracker.FileMetadata{
		ID:               "task-1",
		OriginalUrl:      "https://rutracker.org/forum/viewtopic.php?t=1",
		Magnet:           "magnet:?xt=urn:btih:aaa",
		Name:             "some release",
		LastComment:      "comment",
		LastSyncAt:       time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
		TorrentUpdatedAt: time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC),
		Location:         "/downloads/tv shows",
	}
}

func TestNewRepositoryOnEmptyDatabase(t *testing.T) {
	db := newTestDB(t)

	_, err := NewRepository(db)
	require.ErrorIs(t, err, ErrSchemaNotInitialised)
	assert.Contains(t, err.Error(), "table files is missing")
}

func TestNewRepositoryWithoutAppState(t *testing.T) {
	db := newTestDB(t)

	_, err := migrations.Apply(db.DB())
	require.NoError(t, err)

	_, err = db.Exec(`DROP TABLE app_state`)
	require.NoError(t, err)

	_, err = NewRepository(db)
	require.ErrorIs(t, err, ErrSchemaNotInitialised)
	assert.Contains(t, err.Error(), "table app_state is missing")
}

func TestNewRepositoryWithoutFailureColumns(t *testing.T) {
	db := newTestDB(t)

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

	_, err = NewRepository(db)
	require.ErrorIs(t, err, ErrSchemaNotInitialised)
	assert.Contains(t, err.Error(), "consecutive_failures")
}

func TestNewRepositoryAfterMigrations(t *testing.T) {
	db := newTestDB(t)

	applied, err := migrations.Apply(db.DB())
	require.NoError(t, err)
	require.Positive(t, applied)

	repo, err := NewRepository(db)
	require.NoError(t, err)
	assert.NotNil(t, repo)
}

func TestCreateOrReplaceRoundTripsFailureFields(t *testing.T) {
	repo := newTestRepo(t)

	errorAt := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	m := testMetadata()
	m.ConsecutiveFailures = 2
	m.LastError = "Blocked: flaresolverr not configured"
	m.LastErrorAt = sql.NullTime{Time: errorAt, Valid: true}
	m.LastErrorKind = "blocked"

	require.NoError(t, repo.CreateOrReplace(m))

	byID, err := repo.GetById(m.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, byID.ConsecutiveFailures)
	assert.Equal(t, "Blocked: flaresolverr not configured", byID.LastError)
	assert.Equal(t, "blocked", byID.LastErrorKind)
	assert.True(t, byID.LastErrorAt.Valid)
	assert.True(t, errorAt.Equal(byID.LastErrorAt.Time))

	all, err := repo.GetAll()
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, 2, all[0].ConsecutiveFailures)
	assert.Equal(t, "Blocked: flaresolverr not configured", all[0].LastError)
	assert.Equal(t, "blocked", all[0].LastErrorKind)
	assert.True(t, all[0].LastErrorAt.Valid)
}

func TestCreateOrReplacePreservesConsecutiveFailures(t *testing.T) {
	repo := newTestRepo(t)

	m := testMetadata()
	require.NoError(t, repo.CreateOrReplace(m))

	failedAt := time.Date(2026, 8, 6, 8, 0, 0, 0, time.UTC)
	require.NoError(t, repo.RecordSyncFailure(m.ID, SyncFailure{Text: "Blocked: boom", At: failedAt}))
	require.NoError(t, repo.RecordSyncFailure(m.ID, SyncFailure{Text: "Blocked: boom", At: failedAt}))

	stored, err := repo.GetById(m.ID)
	require.NoError(t, err)
	require.Equal(t, 2, stored.ConsecutiveFailures)

	stored.Location = "/downloads/movies"
	require.NoError(t, repo.CreateOrReplace(stored))

	reread, err := repo.GetById(m.ID)
	require.NoError(t, err)
	assert.Equal(t, "/downloads/movies", reread.Location)
	assert.Equal(t, 2, reread.ConsecutiveFailures)
	assert.Equal(t, "Blocked: boom", reread.LastError)
	assert.True(t, reread.LastErrorAt.Valid)
}

func TestRecordSyncFailureIncrements(t *testing.T) {
	repo := newTestRepo(t)

	m := testMetadata()
	require.NoError(t, repo.CreateOrReplace(m))

	first := time.Date(2026, 8, 6, 8, 0, 0, 0, time.UTC)
	require.NoError(t, repo.RecordSyncFailure(m.ID, SyncFailure{Text: "Transient: timeout", At: first}))

	stored, err := repo.GetById(m.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, stored.ConsecutiveFailures)
	assert.Equal(t, "Transient: timeout", stored.LastError)
	require.True(t, stored.LastErrorAt.Valid)
	assert.True(t, first.Equal(stored.LastErrorAt.Time))

	second := first.Add(time.Hour)
	require.NoError(t, repo.RecordSyncFailure(m.ID, SyncFailure{Text: "Permanent: 404", Kind: "permanent", At: second}))

	stored, err = repo.GetById(m.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, stored.ConsecutiveFailures)
	assert.Equal(t, "Permanent: 404", stored.LastError)
	assert.Equal(t, "permanent", stored.LastErrorKind)
	assert.True(t, stored.Parked())
	assert.True(t, second.Equal(stored.LastErrorAt.Time))
}

func TestRecordSyncSuccessResetsFailureState(t *testing.T) {
	repo := newTestRepo(t)

	m := testMetadata()
	require.NoError(t, repo.CreateOrReplace(m))

	failedAt := time.Date(2026, 8, 6, 8, 0, 0, 0, time.UTC)
	require.NoError(t, repo.RecordSyncFailure(m.ID, SyncFailure{Text: "Permanent: 404", Kind: "permanent", At: failedAt}))

	syncedAt := failedAt.Add(2 * time.Hour)
	require.NoError(t, repo.RecordSyncSuccess(m.ID, syncedAt))

	stored, err := repo.GetById(m.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, stored.ConsecutiveFailures)
	assert.Empty(t, stored.LastError)
	assert.Empty(t, stored.LastErrorKind, "a success un-parks the task")
	assert.False(t, stored.LastErrorAt.Valid)
	assert.True(t, syncedAt.Equal(stored.LastSyncAt))
}

func TestRecordSyncOnMissingTaskIsNoError(t *testing.T) {
	repo := newTestRepo(t)

	require.NoError(t, repo.RecordSyncSuccess("missing", time.Now()))
	require.NoError(t, repo.RecordSyncFailure("missing", SyncFailure{Text: "Blocked: 403", At: time.Now()}))

	_, err := repo.GetById("missing")
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestGetLastRunWithoutStateIsEmpty(t *testing.T) {
	repo := newTestRepo(t)

	at, ok, err := repo.GetLastRun()
	require.NoError(t, err)
	assert.True(t, at.IsZero())
	assert.False(t, ok)
}

func TestSetLastRunRoundTrips(t *testing.T) {
	repo := newTestRepo(t)

	first := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repo.SetLastRun(first, true))

	at, ok, err := repo.GetLastRun()
	require.NoError(t, err)
	assert.True(t, first.Equal(at))
	assert.True(t, ok)

	second := first.Add(time.Hour)
	require.NoError(t, repo.SetLastRun(second, false))

	at, ok, err = repo.GetLastRun()
	require.NoError(t, err)
	assert.True(t, second.Equal(at))
	assert.False(t, ok)
}

func TestCreateOrReplacePreservesNotify(t *testing.T) {
	repo := newTestRepo(t)

	m := testMetadata()
	m.Notify = true
	require.NoError(t, repo.CreateOrReplace(m))

	stored, err := repo.GetById(m.ID)
	require.NoError(t, err)
	require.True(t, stored.Notify)

	stored.Location = "/downloads/movies"
	require.NoError(t, repo.CreateOrReplace(stored))

	reread, err := repo.GetById(m.ID)
	require.NoError(t, err)
	assert.Equal(t, "/downloads/movies", reread.Location)
	assert.True(t, reread.Notify, "notify must survive the INSERT OR REPLACE column reset")

	all, err := repo.GetAll()
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.True(t, all[0].Notify)
}

func TestNewRepositoryWithoutNotifyColumn(t *testing.T) {
	db := newTestDB(t)

	_, err := migrations.Apply(db.DB())
	require.NoError(t, err)

	_, err = db.Exec(`ALTER TABLE files DROP COLUMN notify`)
	require.NoError(t, err)

	_, err = NewRepository(db)
	require.ErrorIs(t, err, ErrSchemaNotInitialised)
	assert.Contains(t, err.Error(), "files.notify is missing")
}

func TestUpdateSettingsChangesOnlyNotifyAndLocation(t *testing.T) {
	repo := newTestRepo(t)

	m := testMetadata()
	require.NoError(t, repo.CreateOrReplace(m))
	require.NoError(t, repo.RecordSyncFailure(m.ID, SyncFailure{Text: "Blocked: 403", At: time.Now()}))

	require.NoError(t, repo.UpdateSettings(m.ID, true, "/downloads/movies"))

	stored, err := repo.GetById(m.ID)
	require.NoError(t, err)
	assert.True(t, stored.Notify)
	assert.Equal(t, "/downloads/movies", stored.Location)
	assert.Equal(t, m.Magnet, stored.Magnet)
	assert.Equal(t, m.Name, stored.Name)
	assert.Equal(t, 1, stored.ConsecutiveFailures, "a settings update must not touch the sync state")
}

func TestUpdateSettingsOnMissingOrDeletedTaskIsNotFound(t *testing.T) {
	repo := newTestRepo(t)

	require.ErrorIs(t, repo.UpdateSettings("missing", true, "/downloads/movies"), sql.ErrNoRows)

	m := testMetadata()
	require.NoError(t, repo.CreateOrReplace(m))
	require.NoError(t, repo.Remove(m.ID))

	require.ErrorIs(t, repo.UpdateSettings(m.ID, true, "/downloads/movies"), sql.ErrNoRows)

	stored, err := repo.GetById(m.ID)
	require.NoError(t, err)
	assert.False(t, stored.Notify)
	assert.Equal(t, m.Location, stored.Location)
	assert.True(t, stored.DeleteAt.Valid)
}
