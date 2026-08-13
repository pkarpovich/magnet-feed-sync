package download_store

import (
	"testing"
	"time"

	"magnet-feed-sync/app/database"
	"magnet-feed-sync/app/downloads"
	"magnet-feed-sync/app/migrations"

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

func testDownload(id string) *downloads.Download {
	return &downloads.Download{
		ID:       id,
		Source:   "magnet:?xt=urn:btih:474d1403945c0768506233481557516e7af8d136",
		Location: "/downloads/cinema-prep",
		Hash:     "474d1403945c0768506233481557516e7af8d136",
	}
}

func TestNewRepositoryOnEmptyDatabase(t *testing.T) {
	db := newTestDB(t)

	_, err := NewRepository(db)

	require.ErrorIs(t, err, ErrSchemaNotInitialised)
}

func TestNewRepositoryWithoutDownloadsTable(t *testing.T) {
	db := newTestDB(t)
	_, err := migrations.Apply(db.DB())
	require.NoError(t, err)

	_, err = db.Exec(`DROP TABLE downloads`)
	require.NoError(t, err)

	_, err = NewRepository(db)

	require.ErrorIs(t, err, ErrSchemaNotInitialised)
}

// the check is on columns, not on table existence: a half-applied migration leaves the table
// in place and the column absent, which a table check passes
func TestNewRepositoryWithMissingColumn(t *testing.T) {
	db := newTestDB(t)
	_, err := migrations.Apply(db.DB())
	require.NoError(t, err)

	_, err = db.Exec(`ALTER TABLE downloads DROP COLUMN content_path`)
	require.NoError(t, err)

	_, err = NewRepository(db)

	require.ErrorIs(t, err, ErrSchemaNotInitialised)
}

func TestCreateAndGetByID(t *testing.T) {
	repo := newTestRepo(t)

	d := testDownload("0123456789abcdef")
	require.NoError(t, repo.Create(d))
	assert.False(t, d.CreatedAt.IsZero())

	stored, err := repo.GetByID("0123456789abcdef")
	require.NoError(t, err)
	require.NotNil(t, stored)

	assert.Equal(t, d.ID, stored.ID)
	assert.Equal(t, d.Source, stored.Source)
	assert.Equal(t, d.Location, stored.Location)
	assert.Equal(t, d.Hash, stored.Hash)
	assert.Empty(t, stored.Name)
	assert.Empty(t, stored.ContentPath)
	assert.Zero(t, stored.Size)
	assert.Empty(t, stored.Status)
	assert.Empty(t, stored.Reason)
	assert.WithinDuration(t, d.CreatedAt, stored.CreatedAt, time.Second)
	assert.Nil(t, stored.CompletedAt)
	assert.Nil(t, stored.PublishedAt)
}

func TestGetByIDUnknownID(t *testing.T) {
	repo := newTestRepo(t)

	stored, err := repo.GetByID("nope")

	require.NoError(t, err)
	assert.Nil(t, stored)
}

func TestPendingExcludesPublishedAndOrdersOldestFirst(t *testing.T) {
	repo := newTestRepo(t)

	base := time.Now().Truncate(time.Second)
	// inserted out of order, so the assertion below is about created_at and not about rowid
	for id, offset := range map[string]time.Duration{
		"third":     3 * time.Minute,
		"first":     time.Minute,
		"published": 4 * time.Minute,
		"second":    2 * time.Minute,
	} {
		d := testDownload(id)
		d.CreatedAt = base.Add(offset)
		require.NoError(t, repo.Create(d))
	}
	require.NoError(t, repo.MarkPublished("published", downloads.Outcome{Status: "completed"}))

	pending, err := repo.Pending()
	require.NoError(t, err)

	ids := make([]string, 0, len(pending))
	for _, d := range pending {
		ids = append(ids, d.ID)
	}

	assert.Equal(t, []string{"first", "second", "third"}, ids)
}

func TestPendingTiesOnTheIdenticalTimestamp(t *testing.T) {
	repo := newTestRepo(t)

	// the rowid tiebreak is what keeps the order total when two rows share a timestamp
	created := time.Now().Truncate(time.Second)
	for _, id := range []string{"first", "second", "third"} {
		d := testDownload(id)
		d.CreatedAt = created
		require.NoError(t, repo.Create(d))
	}

	pending, err := repo.Pending()
	require.NoError(t, err)

	ids := make([]string, 0, len(pending))
	for _, d := range pending {
		ids = append(ids, d.ID)
	}

	assert.Equal(t, []string{"first", "second", "third"}, ids)
}

func TestMarkPublishedWritesTheWholeOutcome(t *testing.T) {
	repo := newTestRepo(t)
	require.NoError(t, repo.Create(testDownload("abcdef0123456789")))

	completedAt := time.Date(2026, 8, 13, 10, 21, 39, 0, time.UTC)
	outcome := downloads.Outcome{
		Status:      "completed",
		Name:        "sample.bin",
		ContentPath: "/downloads/probe/sample.bin",
		Size:        4194304,
		CompletedAt: completedAt,
	}
	require.NoError(t, repo.MarkPublished("abcdef0123456789", outcome))

	stored, err := repo.GetByID("abcdef0123456789")
	require.NoError(t, err)
	require.NotNil(t, stored)

	assert.Equal(t, "completed", stored.Status)
	assert.Empty(t, stored.Reason)
	assert.Equal(t, "sample.bin", stored.Name)
	assert.Equal(t, "/downloads/probe/sample.bin", stored.ContentPath)
	assert.Equal(t, int64(4194304), stored.Size)
	require.NotNil(t, stored.CompletedAt)
	assert.True(t, completedAt.Equal(*stored.CompletedAt))
	require.NotNil(t, stored.PublishedAt)

	pending, err := repo.Pending()
	require.NoError(t, err)
	assert.Empty(t, pending)
}

// publish-then-mark can run the same row twice after a crash; the second pass must not
// overwrite the recorded outcome and must not report an error
func TestMarkPublishedTwiceChangesNothing(t *testing.T) {
	repo := newTestRepo(t)
	require.NoError(t, repo.Create(testDownload("cafebabecafebabe")))

	first := downloads.Outcome{Status: "completed", Name: "sample.bin", Size: 4194304}
	require.NoError(t, repo.MarkPublished("cafebabecafebabe", first))

	before, err := repo.GetByID("cafebabecafebabe")
	require.NoError(t, err)
	require.NotNil(t, before)

	second := downloads.Outcome{Status: "failed", Reason: "torrent no longer present in qbittorrent"}
	require.NoError(t, repo.MarkPublished("cafebabecafebabe", second))

	after, err := repo.GetByID("cafebabecafebabe")
	require.NoError(t, err)
	require.NotNil(t, after)

	assert.Equal(t, before.Status, after.Status)
	assert.Equal(t, before.Reason, after.Reason)
	assert.Equal(t, before.Name, after.Name)
	assert.Equal(t, before.Size, after.Size)
	assert.Equal(t, before.PublishedAt.UTC(), after.PublishedAt.UTC())
}

func TestMarkPublishedUnknownIDIsANoOp(t *testing.T) {
	repo := newTestRepo(t)

	require.NoError(t, repo.MarkPublished("missing", downloads.Outcome{Status: "failed"}))

	count, err := repo.CountPending()
	require.NoError(t, err)
	assert.Zero(t, count)
}

// three rows in the same second: the explicit created_at and the rowid tiebreak are what
// keep the order total, since CURRENT_TIMESTAMP would give all three the same value
func TestNewestBySourcePicksTheNewestOfTheSameSecond(t *testing.T) {
	repo := newTestRepo(t)

	base := time.Now().Truncate(time.Second)
	for i, id := range []string{"first", "second", "third"} {
		d := testDownload(id)
		d.Source = "https://tracker.test/download/1.torrent"
		d.CreatedAt = base.Add(time.Duration(i) * 10 * time.Millisecond)
		require.NoError(t, repo.Create(d))
	}

	other := testDownload("other-source")
	other.Source = "https://tracker.test/download/2.torrent"
	other.CreatedAt = base.Add(time.Hour)
	require.NoError(t, repo.Create(other))

	newest, err := repo.NewestBySource("https://tracker.test/download/1.torrent")
	require.NoError(t, err)
	require.NotNil(t, newest)
	assert.Equal(t, "third", newest.ID)
}

func TestNewestBySourceTiesOnTheIdenticalTimestamp(t *testing.T) {
	repo := newTestRepo(t)

	at := time.Now().Truncate(time.Second)
	for _, id := range []string{"first", "second", "third"} {
		d := testDownload(id)
		d.Source = "https://tracker.test/download/1.torrent"
		d.CreatedAt = at
		require.NoError(t, repo.Create(d))
	}

	newest, err := repo.NewestBySource("https://tracker.test/download/1.torrent")
	require.NoError(t, err)
	require.NotNil(t, newest)
	assert.Equal(t, "third", newest.ID)
}

// the driver writes a time.Time as RFC3339 text carrying its offset, so ORDER BY compares wall
// clocks: across the autumn DST rollback the newer row reads as the older one unless it is
// stored in UTC, and the rowid tiebreak cannot help because the two values are not equal
func TestNewestBySourceSurvivesTheDSTRollback(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no tzdata: ", err)
	}

	repo := newTestRepo(t)
	source := "https://tracker.test/download/1.torrent"

	older := testDownload("older")
	older.Source = source
	older.CreatedAt = time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC).In(berlin) // 02:30 CEST

	newer := testDownload("newer")
	newer.Source = source
	newer.CreatedAt = time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC).In(berlin) // 02:30 CET

	require.NoError(t, repo.Create(older))
	require.NoError(t, repo.Create(newer))

	newest, err := repo.NewestBySource(source)
	require.NoError(t, err)
	require.NotNil(t, newest)
	assert.Equal(t, "newer", newest.ID)

	pending, err := repo.Pending()
	require.NoError(t, err)
	require.Len(t, pending, 2)
	assert.Equal(t, "older", pending[0].ID)
	assert.Equal(t, "newer", pending[1].ID)
}

func TestNewestBySourceUnknownSource(t *testing.T) {
	repo := newTestRepo(t)

	newest, err := repo.NewestBySource("magnet:?xt=urn:btih:nothing")

	require.NoError(t, err)
	assert.Nil(t, newest)
}

// a published row also carries a terminal status, which is exactly what the duplicate path
// writes; the count must follow published_at and nothing else
func TestCountPendingCountsOnlyUnpublished(t *testing.T) {
	repo := newTestRepo(t)

	for _, id := range []string{"one", "two", "three"} {
		require.NoError(t, repo.Create(testDownload(id)))
	}

	count, err := repo.CountPending()
	require.NoError(t, err)
	assert.Equal(t, 3, count)

	require.NoError(t, repo.MarkPublished("two", downloads.Outcome{Status: "completed"}))

	count, err = repo.CountPending()
	require.NoError(t, err)
	assert.Equal(t, 2, count)
}
