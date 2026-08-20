package downloads

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"magnet-feed-sync/app/notify"
	"magnet-feed-sync/app/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testHash        = "474d1403945c0768506233481557516e7af8d136"
	testContentPath = "/downloads/probe/sample.bin"
	testCompletion  = int64(1786626099)
)

var sweepClock = time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)

type fakeStore struct {
	rows         []*Download
	pendingErr   error
	markErr      error
	marked       map[string]Outcome
	pendingCalls int
}

func newFakeStore(rows ...*Download) *fakeStore {
	return &fakeStore{rows: rows, marked: make(map[string]Outcome)}
}

func (s *fakeStore) Pending() ([]*Download, error) {
	s.pendingCalls++
	if s.pendingErr != nil {
		return nil, s.pendingErr
	}

	var pending []*Download
	for _, row := range s.rows {
		if row.PublishedAt == nil {
			pending = append(pending, row)
		}
	}

	return pending, nil
}

func (s *fakeStore) MarkPublished(id string, o Outcome) error {
	if s.markErr != nil {
		return s.markErr
	}

	for _, row := range s.rows {
		if row.ID != id || row.PublishedAt != nil {
			continue
		}

		published := time.Now()
		row.PublishedAt = &published
		row.Status = o.Status
		row.Reason = o.Reason
		row.Name = o.Name
		row.ContentPath = o.ContentPath
		row.Size = o.Size
		completed := o.CompletedAt
		row.CompletedAt = &completed
		s.marked[id] = o
	}

	return nil
}

type fakeLookup struct {
	states map[string]types.TorrentState
	err    error
	calls  [][]string
}

func (l *fakeLookup) TorrentStates(_ context.Context, hashes []string) (map[string]types.TorrentState, error) {
	l.calls = append(l.calls, hashes)
	if l.err != nil {
		return nil, l.err
	}

	return l.states, nil
}

type recorder struct {
	messages []notify.Message
	err      error
	failFor  map[string]error
}

func (r *recorder) Publish(_ context.Context, m notify.Message) error {
	if err, fails := r.failFor[m.MsgID]; fails {
		return err
	}

	if r.err != nil {
		return r.err
	}

	r.messages = append(r.messages, m)

	return nil
}

func newTestSweeper(s *fakeStore, l *fakeLookup, r *recorder) *Sweeper {
	sweeper := NewSweeper(SweeperDeps{Store: s, Torrents: l, Notifier: r})
	sweeper.now = func() time.Time { return sweepClock }

	return sweeper
}

func pendingRow(id string) *Download {
	return &Download{
		ID:        id,
		Source:    "magnet:?xt=urn:btih:" + testHash,
		Location:  "/downloads/cinema-prep",
		Hash:      testHash,
		CreatedAt: time.Now(),
	}
}

func completedState(state string) types.TorrentState {
	return types.TorrentState{
		Hash:         testHash,
		Name:         "sample.bin",
		State:        state,
		ContentPath:  testContentPath,
		Progress:     1,
		CompletionOn: testCompletion,
		Size:         4194304,
	}
}

func unfinishedState(state string) types.TorrentState {
	return types.TorrentState{
		Hash:         testHash,
		Name:         "sample.bin",
		State:        state,
		ContentPath:  testContentPath,
		Progress:     0,
		CompletionOn: -1,
	}
}

func TestSweeperPublishesEveryTerminalState(t *testing.T) {
	for _, state := range knownTorrentStates {
		for _, tc := range []struct {
			name  string
			state types.TorrentState
		}{
			{name: "finished", state: completedState(state)},
			{name: "unfinished", state: unfinishedState(state)},
		} {
			t.Run(state+"/"+tc.name, func(t *testing.T) {
				row := pendingRow("a1b2c3d4e5f60718")
				store := newFakeStore(row)
				lookup := &fakeLookup{states: map[string]types.TorrentState{testHash: tc.state}}
				rec := &recorder{}

				require.NoError(t, newTestSweeper(store, lookup, rec).RunCycle(context.Background()))

				class := Classify(tc.state, true)
				if !class.Terminal() {
					assert.Empty(t, rec.messages)
					assert.Nil(t, row.PublishedAt)

					return
				}

				require.Len(t, rec.messages, 1)
				assert.Equal(t, "tuclaw.downloads.completed."+row.ID, rec.messages[0].Subject)
				assert.Equal(t, row.ID+":"+class.Status, rec.messages[0].MsgID)
				assert.Equal(t, class.Status, store.marked[row.ID].Status)
				assert.NotNil(t, row.PublishedAt)
			})
		}
	}
}

func TestSweeperSkipsTorrentLookupWithoutPendingRows(t *testing.T) {
	store := newFakeStore()
	lookup := &fakeLookup{}
	rec := &recorder{}

	require.NoError(t, newTestSweeper(store, lookup, rec).RunCycle(context.Background()))

	assert.Empty(t, lookup.calls)
	assert.Empty(t, rec.messages)
}

func TestSweeperAsksForEveryPendingHashOnce(t *testing.T) {
	first, second := pendingRow("1111111111111111"), pendingRow("2222222222222222")
	second.Hash = "9ecd4676fd0f0474151a4b74a5958f42639cebdf"
	store := newFakeStore(first, second)
	lookup := &fakeLookup{states: map[string]types.TorrentState{}}

	require.NoError(t, newTestSweeper(store, lookup, &recorder{}).RunCycle(context.Background()))

	require.Len(t, lookup.calls, 1)
	assert.Equal(t, []string{first.Hash, second.Hash}, lookup.calls[0])
}

func TestSweeperReportsEveryTerminalRowInOneCycle(t *testing.T) {
	first, second, third := pendingRow("1111111111111111"), pendingRow("2222222222222222"), pendingRow("3333333333333333")
	second.Hash = "9ecd4676fd0f0474151a4b74a5958f42639cebdf"
	third.Hash = "5e7c3b1a9d2f4068a1c3e5079b2d4f6081a3c5e7"
	store := newFakeStore(first, second, third)
	lookup := &fakeLookup{states: map[string]types.TorrentState{
		first.Hash:  completedState("stalledUP"),
		second.Hash: unfinishedState("downloading"),
		// third is absent: deleted by hand, which is terminal too
	}}
	rec := &recorder{}

	require.NoError(t, newTestSweeper(store, lookup, rec).RunCycle(context.Background()))

	require.Len(t, rec.messages, 2)
	assert.Equal(t, "tuclaw.downloads.completed."+first.ID, rec.messages[0].Subject)
	assert.Equal(t, "tuclaw.downloads.completed."+third.ID, rec.messages[1].Subject)
	assert.Equal(t, "completed", store.marked[first.ID].Status)
	assert.Equal(t, "failed", store.marked[third.ID].Status)
	assert.NotContains(t, store.marked, second.ID)
	assert.Nil(t, second.PublishedAt)
}

func TestSweeperPublishFailureDoesNotAbortTheCycle(t *testing.T) {
	first, second := pendingRow("1111111111111111"), pendingRow("2222222222222222")
	second.Hash = "9ecd4676fd0f0474151a4b74a5958f42639cebdf"
	store := newFakeStore(first, second)
	lookup := &fakeLookup{states: map[string]types.TorrentState{
		first.Hash:  completedState("stalledUP"),
		second.Hash: completedState("stalledUP"),
	}}
	rec := &recorder{failFor: map[string]error{first.ID + ":completed": errors.New("nats is down")}}

	require.NoError(t, newTestSweeper(store, lookup, rec).RunCycle(context.Background()))

	require.Len(t, rec.messages, 1)
	assert.Equal(t, "tuclaw.downloads.completed."+second.ID, rec.messages[0].Subject)
	assert.Nil(t, first.PublishedAt, "the row whose publish failed must be retried next tick")
	assert.NotNil(t, second.PublishedAt)
}

func TestSweeperSkipsTorrentLookupOnCancelledContext(t *testing.T) {
	row := pendingRow("a1b2c3d4e5f60718")
	store := newFakeStore(row)
	lookup := &fakeLookup{err: errors.New("context canceled")}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// a shutdown is not a failed cycle, so the lookup is not even attempted
	require.NoError(t, newTestSweeper(store, lookup, &recorder{}).RunCycle(ctx))
	assert.Empty(t, lookup.calls)
}

func TestSweeperCompletedPayload(t *testing.T) {
	row := pendingRow("a1b2c3d4e5f60718")
	store := newFakeStore(row)
	lookup := &fakeLookup{states: map[string]types.TorrentState{testHash: completedState("stalledUP")}}
	rec := &recorder{}

	require.NoError(t, newTestSweeper(store, lookup, rec).RunCycle(context.Background()))

	require.Len(t, rec.messages, 1)
	assert.Equal(t, "tuclaw.downloads.completed.a1b2c3d4e5f60718", rec.messages[0].Subject)
	assert.Equal(t, "a1b2c3d4e5f60718:completed", rec.messages[0].MsgID)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.messages[0].Payload, &body))
	assert.Equal(t, map[string]any{
		"download_id":  "a1b2c3d4e5f60718",
		"status":       "completed",
		"hash":         testHash,
		"name":         "sample.bin",
		"content_path": testContentPath,
		"size":         float64(4194304),
		"location":     "/downloads/cinema-prep",
		"completed_at": time.Unix(testCompletion, 0).UTC().Format(time.RFC3339),
	}, body)

	outcome := store.marked[row.ID]
	assert.Equal(t, testContentPath, outcome.ContentPath)
	assert.Equal(t, int64(4194304), outcome.Size)
	assert.Equal(t, time.Unix(testCompletion, 0).UTC(), outcome.CompletedAt)
}

func TestSweeperFailedPayloads(t *testing.T) {
	for _, tc := range []struct {
		name   string
		states map[string]types.TorrentState
		reason string
	}{
		{
			name:   "error state",
			states: map[string]types.TorrentState{testHash: completedState("error")},
			reason: "qbittorrent state: error",
		},
		{
			name:   "missing files",
			states: map[string]types.TorrentState{testHash: completedState("missingFiles")},
			reason: "qbittorrent state: missingFiles",
		},
		{
			name:   "deleted by hand",
			states: map[string]types.TorrentState{},
			reason: "torrent no longer present in qbittorrent",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := pendingRow("a1b2c3d4e5f60718")
			store := newFakeStore(row)
			rec := &recorder{}

			sweeper := newTestSweeper(store, &fakeLookup{states: tc.states}, rec)
			require.NoError(t, sweeper.RunCycle(context.Background()))

			require.Len(t, rec.messages, 1)
			// the failure rides the very subject the caller was handed: a failure-specific one
			// would never fire the agent's one-shot event task
			assert.Equal(t, "tuclaw.downloads.completed.a1b2c3d4e5f60718", rec.messages[0].Subject)
			assert.Equal(t, "a1b2c3d4e5f60718:failed", rec.messages[0].MsgID)

			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.messages[0].Payload, &body))
			assert.Equal(t, map[string]any{
				"download_id":  "a1b2c3d4e5f60718",
				"status":       "failed",
				"reason":       tc.reason,
				"hash":         testHash,
				"name":         "",
				"completed_at": sweepClock.Format(time.RFC3339),
			}, body)

			assert.Equal(t, sweepClock, store.marked[row.ID].CompletedAt)
		})
	}
}

func TestSweeperLookupErrorPublishesAndMarksNothing(t *testing.T) {
	row := pendingRow("a1b2c3d4e5f60718")
	store := newFakeStore(row)
	rec := &recorder{}

	sweeper := newTestSweeper(store, &fakeLookup{err: errors.New("qbittorrent is down")}, rec)
	err := sweeper.RunCycle(context.Background())

	require.Error(t, err)
	assert.Empty(t, rec.messages)
	assert.Empty(t, store.marked)
	assert.Nil(t, row.PublishedAt)
}

func TestSweeperPendingErrorAbortsCycle(t *testing.T) {
	store := newFakeStore()
	store.pendingErr = errors.New("database is locked")
	lookup := &fakeLookup{}

	err := newTestSweeper(store, lookup, &recorder{}).RunCycle(context.Background())

	require.Error(t, err)
	assert.Empty(t, lookup.calls)
}

func TestSweeperRetriesAfterPublishFailure(t *testing.T) {
	row := pendingRow("a1b2c3d4e5f60718")
	store := newFakeStore(row)
	lookup := &fakeLookup{states: map[string]types.TorrentState{testHash: completedState("stalledUP")}}
	rec := &recorder{err: errors.New("nats is down")}
	sweeper := newTestSweeper(store, lookup, rec)

	require.NoError(t, sweeper.RunCycle(context.Background()))
	assert.Empty(t, store.marked)
	assert.Nil(t, row.PublishedAt)

	rec.err = nil
	require.NoError(t, sweeper.RunCycle(context.Background()))

	require.Len(t, rec.messages, 1)
	assert.NotNil(t, row.PublishedAt)
}

func TestSweeperPublishesOnceAcrossCycles(t *testing.T) {
	row := pendingRow("a1b2c3d4e5f60718")
	store := newFakeStore(row)
	lookup := &fakeLookup{states: map[string]types.TorrentState{testHash: completedState("stalledUP")}}
	rec := &recorder{}
	sweeper := newTestSweeper(store, lookup, rec)

	require.NoError(t, sweeper.RunCycle(context.Background()))
	require.NoError(t, sweeper.RunCycle(context.Background()))

	assert.Len(t, rec.messages, 1)
	assert.Len(t, lookup.calls, 1)
}

func TestSweeperIgnoresAlreadyPublishedRows(t *testing.T) {
	published := time.Now()
	row := pendingRow("a1b2c3d4e5f60718")
	row.Status = "completed"
	row.PublishedAt = &published
	store := newFakeStore(row)
	lookup := &fakeLookup{states: map[string]types.TorrentState{testHash: completedState("stalledUP")}}
	rec := &recorder{}

	require.NoError(t, newTestSweeper(store, lookup, rec).RunCycle(context.Background()))

	assert.Empty(t, rec.messages)
	assert.Empty(t, lookup.calls)
}

func TestSweeperStopsOnCancelledContext(t *testing.T) {
	row := pendingRow("a1b2c3d4e5f60718")
	store := newFakeStore(row)
	lookup := &fakeLookup{states: map[string]types.TorrentState{testHash: completedState("stalledUP")}}
	rec := &recorder{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, newTestSweeper(store, lookup, rec).RunCycle(ctx))

	assert.Empty(t, rec.messages)
	assert.Nil(t, row.PublishedAt)
}

func TestSweeperKeepsRowPendingWhenMarkFails(t *testing.T) {
	row := pendingRow("a1b2c3d4e5f60718")
	store := newFakeStore(row)
	store.markErr = errors.New("database is locked")
	lookup := &fakeLookup{states: map[string]types.TorrentState{testHash: completedState("stalledUP")}}
	rec := &recorder{}

	require.NoError(t, newTestSweeper(store, lookup, rec).RunCycle(context.Background()))

	assert.Len(t, rec.messages, 1)
	assert.Nil(t, row.PublishedAt)
}
