package watch_store

import (
	"testing"
	"time"

	"magnet-feed-sync/app/database"
	"magnet-feed-sync/app/migrations"
	"magnet-feed-sync/app/watcher"

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

// tests reach their schema the same way production does, so a migration that forgets a
// column fails the suite instead of only failing on deploy
func newTestRepo(t *testing.T) *Repository {
	t.Helper()

	db := newTestDB(t)

	_, err := migrations.Apply(db.DB())
	require.NoError(t, err)

	repo, err := NewRepository(db)
	require.NoError(t, err)

	return repo
}

func testWatch() *watcher.Watch {
	expires := time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC)

	return &watcher.Watch{
		ID:           "one-night-only-en",
		Queries:      []string{"One Night Only 2026", "Только на одну ночь 2026"},
		Sources:      []string{"jackett", "extto"},
		IncludeRegex: `(?i)one[ ._-]night[ ._-]only.*2026`,
		ExcludeRegex: `(?i)bee gees|def leppard`,
		ExpiresAt:    &expires,
	}
}

func TestNewRepositoryOnEmptyDatabase(t *testing.T) {
	db := newTestDB(t)

	_, err := NewRepository(db)

	require.ErrorIs(t, err, ErrSchemaNotInitialised)
}

func TestNewRepositoryWithoutSeenTable(t *testing.T) {
	db := newTestDB(t)
	_, err := migrations.Apply(db.DB())
	require.NoError(t, err)

	_, err = db.Exec(`DROP TABLE watch_seen`)
	require.NoError(t, err)

	_, err = NewRepository(db)

	require.ErrorIs(t, err, ErrSchemaNotInitialised)
}

// the check is on columns, not on table existence: the incident behind task-store's version
// of this had the table present and the columns missing, which a table check passes
func TestNewRepositoryWithoutAColumn(t *testing.T) {
	db := newTestDB(t)
	_, err := migrations.Apply(db.DB())
	require.NoError(t, err)

	_, err = db.Exec(`ALTER TABLE watches DROP COLUMN last_status`)
	require.NoError(t, err)

	_, err = NewRepository(db)

	require.ErrorIs(t, err, ErrSchemaNotInitialised)
	assert.Contains(t, err.Error(), "watches.last_status")
}

// a soft delete that nothing can clear retires the id for good: the row still exists, so a
// re-create is a conflict forever
func TestReviveClearsTheSoftDelete(t *testing.T) {
	repo := newTestRepo(t)
	w := testWatch()
	require.NoError(t, repo.Create(w))
	require.NoError(t, repo.Disable(w.ID))

	disabled, err := repo.GetByID(w.ID)
	require.NoError(t, err)
	require.NotNil(t, disabled.DisabledAt)

	require.NoError(t, repo.Revive(w.ID))

	enabled, err := repo.GetByID(w.ID)
	require.NoError(t, err)
	assert.Nil(t, enabled.DisabledAt)

	forCycle, err := repo.WatchesForCycle()
	require.NoError(t, err)
	require.Len(t, forCycle, 1)
}

// a re-created id is a create: it must seed silently rather than publish what already existed,
// and it must not carry the dead watch's run state into /api/health. The announcements it
// already made survive, which is what keeps the silent seed from being a replay.
func TestReviveResetsTheRunLifecycleAndKeepsSeenRows(t *testing.T) {
	repo := newTestRepo(t)
	w := testWatch()
	require.NoError(t, repo.Create(w))
	require.NoError(t, repo.MarkSeen(w.ID, []watcher.SearchResult{
		{Source: "jackett", ExternalID: "1883913", Title: "One.Night.Only.2026.1080p"},
	}))
	require.NoError(t, repo.MarkSeeded(w.ID))
	require.NoError(t, repo.RecordRun(w.ID, `jackett "One Night Only 2026": 502`))
	require.NoError(t, repo.Disable(w.ID))

	require.NoError(t, repo.Revive(w.ID))

	revived, err := repo.GetByID(w.ID)
	require.NoError(t, err)
	assert.Nil(t, revived.SeededAt)
	assert.Nil(t, revived.LastRunAt)
	assert.Empty(t, revived.LastStatus)

	seen, err := repo.SeenKeys(w.ID)
	require.NoError(t, err)
	assert.Len(t, seen, 1)
}

func TestReviveUnknownWatch(t *testing.T) {
	repo := newTestRepo(t)

	require.ErrorIs(t, repo.Revive("nowhere"), ErrNotFound)
}

// every production watch is created without an expiry, and testWatch always sets one
func TestCreateWithoutExpiry(t *testing.T) {
	repo := newTestRepo(t)
	w := testWatch()
	w.ExpiresAt = nil
	require.NoError(t, repo.Create(w))

	got, err := repo.GetByID(w.ID)
	require.NoError(t, err)
	assert.Nil(t, got.ExpiresAt)
	assert.Nil(t, got.DisabledAt)
}

func TestCreateAndGetByID(t *testing.T) {
	repo := newTestRepo(t)
	want := testWatch()

	require.NoError(t, repo.Create(want))

	got, err := repo.GetByID(want.ID)
	require.NoError(t, err)

	assert.Equal(t, want.Queries, got.Queries)
	assert.Equal(t, want.Sources, got.Sources)
	assert.Equal(t, want.IncludeRegex, got.IncludeRegex)
	assert.Equal(t, want.ExcludeRegex, got.ExcludeRegex)
	assert.Equal(t, 1, got.Rev)
	assert.Nil(t, got.SeededAt)
	assert.Nil(t, got.LastRunAt)
	assert.Empty(t, got.LastStatus)
	require.NotNil(t, got.ExpiresAt)
	assert.True(t, want.ExpiresAt.Equal(*got.ExpiresAt))
}

func TestGetAllReturnsEveryWatch(t *testing.T) {
	repo := newTestRepo(t)

	first := testWatch()
	require.NoError(t, repo.Create(first))

	second := testWatch()
	second.ID = "another-watch"
	require.NoError(t, repo.Create(second))
	require.NoError(t, repo.Disable(second.ID))

	all, err := repo.GetAll()
	require.NoError(t, err)

	assert.Len(t, all, 2)
}

func TestUpdateBumpsRev(t *testing.T) {
	repo := newTestRepo(t)
	w := testWatch()
	require.NoError(t, repo.Create(w))

	w.Queries = []string{"One Night Only 2027"}
	w.Sources = []string{"jackett"}
	w.IncludeRegex = `(?i)one night only`
	require.NoError(t, repo.Update(w))

	got, err := repo.GetByID(w.ID)
	require.NoError(t, err)

	assert.Equal(t, 2, got.Rev)
	assert.Equal(t, []string{"One Night Only 2027"}, got.Queries)
	assert.Equal(t, []string{"jackett"}, got.Sources)
	assert.Equal(t, `(?i)one night only`, got.IncludeRegex)
}

func TestWatchesForCycleExcludesDisabledAndKeepsExpired(t *testing.T) {
	repo := newTestRepo(t)

	expired := testWatch()
	expired.ID = "expired-watch"
	past := time.Now().Add(-time.Hour)
	expired.ExpiresAt = &past
	require.NoError(t, repo.Create(expired))

	disabled := testWatch()
	disabled.ID = "disabled-watch"
	require.NoError(t, repo.Create(disabled))
	require.NoError(t, repo.Disable(disabled.ID))

	forCycle, err := repo.WatchesForCycle()
	require.NoError(t, err)

	require.Len(t, forCycle, 1)
	assert.Equal(t, "expired-watch", forCycle[0].ID)
}

func TestMarkSeededAndRecordRun(t *testing.T) {
	repo := newTestRepo(t)
	w := testWatch()
	require.NoError(t, repo.Create(w))

	require.NoError(t, repo.MarkSeeded(w.ID))
	require.NoError(t, repo.RecordRun(w.ID, "jackett: request failed"))

	got, err := repo.GetByID(w.ID)
	require.NoError(t, err)

	assert.NotNil(t, got.SeededAt)
	assert.NotNil(t, got.LastRunAt)
	assert.Equal(t, "jackett: request failed", got.LastStatus)

	require.NoError(t, repo.RecordRun(w.ID, ""))

	got, err = repo.GetByID(w.ID)
	require.NoError(t, err)
	assert.Empty(t, got.LastStatus)
}

func TestMarkSeenIsIdempotent(t *testing.T) {
	repo := newTestRepo(t)
	w := testWatch()
	require.NoError(t, repo.Create(w))

	results := []watcher.SearchResult{
		{Source: "jackett", ExternalID: "1883913", Title: "One Night Only (2026) TSRip"},
	}

	require.NoError(t, repo.MarkSeen(w.ID, results))
	require.NoError(t, repo.MarkSeen(w.ID, results))

	rows, err := repo.SeenRows(w.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "jackett", rows[0].Source)
	assert.Equal(t, "1883913", rows[0].ExternalID)
	assert.Equal(t, "One Night Only (2026) TSRip", rows[0].Title)
	assert.False(t, rows[0].FirstSeenAt.IsZero())

	keys, err := repo.SeenKeys(w.ID)
	require.NoError(t, err)
	assert.Len(t, keys, 1)
}

func TestSeenKeysAreNamespacedBySource(t *testing.T) {
	repo := newTestRepo(t)
	w := testWatch()
	require.NoError(t, repo.Create(w))

	require.NoError(t, repo.MarkSeen(w.ID, []watcher.SearchResult{
		{Source: "jackett", ExternalID: "20151803", Title: "from jackett"},
		{Source: "extto", ExternalID: "20151803", Title: "from extto"},
	}))

	keys, err := repo.SeenKeys(w.ID)
	require.NoError(t, err)

	require.Len(t, keys, 2)
	assert.Contains(t, keys, watcher.SearchResult{Source: "jackett", ExternalID: "20151803"}.SeenKey())
	assert.Contains(t, keys, watcher.SearchResult{Source: "extto", ExternalID: "20151803"}.SeenKey())
}

func TestSeenStateIsPerWatch(t *testing.T) {
	repo := newTestRepo(t)

	first := testWatch()
	require.NoError(t, repo.Create(first))

	second := testWatch()
	second.ID = "another-watch"
	require.NoError(t, repo.Create(second))

	require.NoError(t, repo.MarkSeen(first.ID, []watcher.SearchResult{
		{Source: "jackett", ExternalID: "1", Title: "a release"},
	}))

	keys, err := repo.SeenKeys(second.ID)
	require.NoError(t, err)
	assert.Empty(t, keys)
}

func TestUnknownWatchID(t *testing.T) {
	repo := newTestRepo(t)

	_, err := repo.GetByID("missing")
	require.ErrorIs(t, err, ErrNotFound)

	require.ErrorIs(t, repo.Update(testWatch()), ErrNotFound)
	require.ErrorIs(t, repo.Disable("missing"), ErrNotFound)
	require.ErrorIs(t, repo.MarkSeeded("missing"), ErrNotFound)
	require.ErrorIs(t, repo.RecordRun("missing", ""), ErrNotFound)
}

func TestMalformedStoredQueries(t *testing.T) {
	repo := newTestRepo(t)

	_, err := repo.db.Exec(`INSERT INTO watches (id, queries) VALUES (?, ?)`, "broken", "not json")
	require.NoError(t, err)

	_, err = repo.GetByID("broken")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse queries of watch broken")

	_, err = repo.GetAll()
	require.Error(t, err)
}

func TestCreateStampsCreatedAt(t *testing.T) {
	repo := newTestRepo(t)
	w := testWatch()
	require.NoError(t, repo.Create(w))

	stored, err := repo.GetByID(w.ID)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), stored.CreatedAt, time.Minute)
}

func TestReviveRestampsCreatedAt(t *testing.T) {
	repo := newTestRepo(t)
	w := testWatch()
	require.NoError(t, repo.Create(w))
	_, err := repo.db.Exec(`UPDATE watches SET created_at = '2026-01-01 00:00:00' WHERE id = ?`, w.ID)
	require.NoError(t, err)
	require.NoError(t, repo.Disable(w.ID))

	backdated, err := repo.GetByID(w.ID)
	require.NoError(t, err)
	require.Equal(t, time.January, backdated.CreatedAt.Month())

	require.NoError(t, repo.Revive(w.ID))

	revived, err := repo.GetByID(w.ID)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), revived.CreatedAt, time.Minute, "a revive is a create, so the pending grace restarts")
}
