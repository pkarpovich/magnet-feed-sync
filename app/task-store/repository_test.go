package task_store

import (
	"database/sql"
	"testing"
	"time"

	"magnet-feed-sync/app/database"
	"magnet-feed-sync/app/tracker"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRepo(t *testing.T) *Repository {
	t.Helper()

	t.Chdir(t.TempDir())

	db, err := database.NewClient("test.db")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

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

func TestCreateOrReplaceRoundTripsFailureFields(t *testing.T) {
	repo := newTestRepo(t)

	errorAt := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	m := testMetadata()
	m.ConsecutiveFailures = 2
	m.LastError = "Blocked: flaresolverr not configured"
	m.LastErrorAt = sql.NullTime{Time: errorAt, Valid: true}

	require.NoError(t, repo.CreateOrReplace(m))

	byID, err := repo.GetById(m.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, byID.ConsecutiveFailures)
	assert.Equal(t, "Blocked: flaresolverr not configured", byID.LastError)
	assert.True(t, byID.LastErrorAt.Valid)
	assert.True(t, errorAt.Equal(byID.LastErrorAt.Time))

	all, err := repo.GetAll()
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, 2, all[0].ConsecutiveFailures)
	assert.Equal(t, "Blocked: flaresolverr not configured", all[0].LastError)
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
	require.NoError(t, repo.RecordSyncFailure(m.ID, SyncFailure{Text: "Blocked: 403", At: second}))

	stored, err = repo.GetById(m.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, stored.ConsecutiveFailures)
	assert.Equal(t, "Blocked: 403", stored.LastError)
	assert.True(t, second.Equal(stored.LastErrorAt.Time))
}

func TestRecordSyncSuccessResetsFailureState(t *testing.T) {
	repo := newTestRepo(t)

	m := testMetadata()
	require.NoError(t, repo.CreateOrReplace(m))

	failedAt := time.Date(2026, 8, 6, 8, 0, 0, 0, time.UTC)
	require.NoError(t, repo.RecordSyncFailure(m.ID, SyncFailure{Text: "Blocked: 403", At: failedAt}))

	syncedAt := failedAt.Add(2 * time.Hour)
	require.NoError(t, repo.RecordSyncSuccess(m.ID, syncedAt))

	stored, err := repo.GetById(m.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, stored.ConsecutiveFailures)
	assert.Empty(t, stored.LastError)
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
