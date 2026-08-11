package watcher

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	acceptanceInclude = `(?i)one[ ._-]night[ ._-]only.*2026|только[ ._-]на[ ._-]одну[ ._-]ночь.*2026`
	acceptanceExclude = `(?i)bee gees|def leppard|one desire|streisand|rupaul|top chef|concert|chinese|\b2016\b`

	enRelease = "One.Night.Only.2026.1080p.WEB-DL.DDP5.1.H264-GROUP"
	ruRelease = "Только на одну ночь / One Night Only (2026) TSRip [H.264] [AD]"
)

// fakeSource scripts one answer per Search call, so a test can make the second query of a
// source fail while the first succeeds.
type fakeSource struct {
	name    string
	replies []sourceReply
	calls   []string
}

type sourceReply struct {
	results []SearchResult
	err     error
}

func (s *fakeSource) Name() string {
	return s.name
}

func (s *fakeSource) Search(_ context.Context, query string) ([]SearchResult, error) {
	s.calls = append(s.calls, query)

	if len(s.replies) == 0 {
		return nil, nil
	}

	reply := s.replies[0]
	if len(s.replies) > 1 {
		s.replies = s.replies[1:]
	}

	return reply.results, reply.err
}

func sourceWith(name string, titles ...string) *fakeSource {
	results := make([]SearchResult, 0, len(titles))
	for i, title := range titles {
		results = append(results, SearchResult{
			Source:     name,
			ExternalID: externalID(i),
			Title:      title,
			Query:      "One Night Only 2026",
		})
	}

	return &fakeSource{name: name, replies: []sourceReply{{results: results}}}
}

func externalID(i int) string {
	return string(rune('1' + i))
}

// fakeStore keeps the watch state in memory; the SQL behaviour it stands in for is covered
// by the watch-store tests.
type fakeStore struct {
	mu       sync.Mutex
	watches  []*Watch
	seen     map[string]map[string]SearchResult
	statuses map[string]string
	runs     map[string]int
	seeded   []string
	disabled []string

	seenKeysErr error
	markSeenErr error
	cycleErr    error
}

func newFakeStore(watches ...*Watch) *fakeStore {
	return &fakeStore{
		watches:  watches,
		seen:     make(map[string]map[string]SearchResult),
		statuses: make(map[string]string),
		runs:     make(map[string]int),
	}
}

func (s *fakeStore) WatchesForCycle() ([]*Watch, error) {
	return s.watches, s.cycleErr
}

func (s *fakeStore) SeenKeys(watchID string) (map[string]struct{}, error) {
	if s.seenKeysErr != nil {
		return nil, s.seenKeysErr
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	keys := make(map[string]struct{}, len(s.seen[watchID]))
	for key := range s.seen[watchID] {
		keys[key] = struct{}{}
	}

	return keys, nil
}

func (s *fakeStore) MarkSeen(watchID string, results []SearchResult) error {
	if s.markSeenErr != nil {
		return s.markSeenErr
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.seen[watchID] == nil {
		s.seen[watchID] = make(map[string]SearchResult)
	}
	for _, result := range results {
		s.seen[watchID][result.SeenKey()] = result
	}

	return nil
}

func (s *fakeStore) MarkSeeded(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.seeded = append(s.seeded, id)
	for _, w := range s.watches {
		if w.ID == id {
			now := time.Now()
			w.SeededAt = &now
		}
	}

	return nil
}

func (s *fakeStore) Disable(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.disabled = append(s.disabled, id)

	return nil
}

func (s *fakeStore) RecordRun(watchID, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.statuses[watchID] = status
	s.runs[watchID]++

	return nil
}

func (s *fakeStore) seenTitles(watchID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var titles []string
	for _, result := range s.seen[watchID] {
		titles = append(titles, result.Title)
	}

	return titles
}

type fakePublisher struct {
	published []RunOutcome
	err       error
}

func (p *fakePublisher) Publish(_ context.Context, _ Watch, o RunOutcome) error {
	if p.err != nil {
		return p.err
	}

	p.published = append(p.published, o)

	return nil
}

func acceptanceWatch() *Watch {
	return &Watch{
		ID:           "one-night-only-en",
		Queries:      []string{"One Night Only 2026", "Только на одну ночь 2026"},
		Sources:      []string{"jackett", "extto"},
		IncludeRegex: acceptanceInclude,
		ExcludeRegex: acceptanceExclude,
	}
}

func TestEvaluateFiltersWithAcceptanceRegexes(t *testing.T) {
	source := sourceWith("jackett",
		enRelease,
		ruRelease,
		"Bee Gees One Night Only 1998 WEBRip 1080p x264 AAC ENG Lulloz",
		"Def Leppard - One Night Only: Live At The Leadmill [2024, Classic Rock, Hard Rock, Blu-ray, 1080i]",
		"RuPauls Drag Race S15E02 One Night Only Part 2 1080p AMZN WEB DL DDP2 0 H 264 FLUX TGx",
		"One Night Only / Tian Liang Zhi Qian [2016, BDRemux 1080p] VO + DVO + Sub Rus, Eng + Original Chi",
	)

	w := acceptanceWatch()
	w.Sources = []string{"jackett"}
	engine := NewEngine(EngineDeps{Sources: []SearchSource{source}, Store: newFakeStore()})

	o := engine.Evaluate(context.Background(), *w)

	require.Empty(t, o.Errs)
	assert.Len(t, o.Raw, 6)
	require.Len(t, o.Matched, 2)
	assert.Equal(t, enRelease, o.Matched[0].Title)
	assert.Equal(t, ruRelease, o.Matched[1].Title)
}

func TestEvaluateWithoutRegexesPassesEverything(t *testing.T) {
	source := sourceWith("jackett", enRelease, "Bee Gees One Night Only 1998 WEBRip")
	w := &Watch{ID: "plain", Queries: []string{"q"}, Sources: []string{"jackett"}}
	engine := NewEngine(EngineDeps{Sources: []SearchSource{source}, Store: newFakeStore()})

	o := engine.Evaluate(context.Background(), *w)

	require.Empty(t, o.Errs)
	assert.Len(t, o.Matched, 2)
	assert.Len(t, o.New, 2)
}

func TestEvaluateDeduplicatesBySourceAndExternalID(t *testing.T) {
	duplicate := []SearchResult{{Source: "jackett", ExternalID: "1883913", Title: enRelease}}
	source := &fakeSource{
		name:    "jackett",
		replies: []sourceReply{{results: duplicate}, {results: duplicate}},
	}
	w := acceptanceWatch()
	w.Sources = []string{"jackett"}
	engine := NewEngine(EngineDeps{Sources: []SearchSource{source}, Store: newFakeStore()})

	o := engine.Evaluate(context.Background(), *w)

	assert.Equal(t, []string{"One Night Only 2026", "Только на одну ночь 2026"}, source.calls)
	assert.Len(t, o.Raw, 1)
	assert.Len(t, o.Matched, 1)
}

func TestEvaluateKeepsSameExternalIDFromDifferentSources(t *testing.T) {
	jackett := &fakeSource{name: "jackett", replies: []sourceReply{{
		results: []SearchResult{{Source: "jackett", ExternalID: "42", Title: enRelease}},
	}}}
	extto := &fakeSource{name: "extto", replies: []sourceReply{{
		results: []SearchResult{{Source: "extto", ExternalID: "42", Title: enRelease}},
	}}}
	w := acceptanceWatch()
	w.Queries = []string{"One Night Only 2026"}
	engine := NewEngine(EngineDeps{
		Sources: []SearchSource{jackett, extto},
		Store:   newFakeStore(),
	})

	o := engine.Evaluate(context.Background(), *w)

	assert.Len(t, o.Matched, 2)
}

func TestEvaluateAdHocWatchReportsEverythingAsNew(t *testing.T) {
	store := newFakeStore()
	require.NoError(t, store.MarkSeen("", []SearchResult{{Source: "jackett", ExternalID: "1", Title: enRelease}}))

	source := sourceWith("jackett", enRelease, ruRelease)
	w := Watch{Queries: []string{"One Night Only 2026"}, Sources: []string{"jackett"}, IncludeRegex: acceptanceInclude}
	engine := NewEngine(EngineDeps{Sources: []SearchSource{source}, Store: store})

	o := engine.Evaluate(context.Background(), w)

	require.Empty(t, o.Errs)
	assert.Len(t, o.New, 2)
	assert.Nil(t, store.seenKeysErr)
}

func TestEvaluateFailingSourceDoesNotAbortTheOthers(t *testing.T) {
	failing := &fakeSource{name: "extto", replies: []sourceReply{{err: errors.New("challenge twice")}}}
	working := sourceWith("jackett", enRelease)
	w := acceptanceWatch()
	w.Queries = []string{"One Night Only 2026"}
	w.Sources = []string{"extto", "jackett"}
	engine := NewEngine(EngineDeps{
		Sources: []SearchSource{failing, working},
		Store:   newFakeStore(),
	})

	o := engine.Evaluate(context.Background(), *w)

	require.Len(t, o.Errs, 1)
	assert.Contains(t, o.Errs[0].Error(), "challenge twice")
	require.Len(t, o.New, 1)
	assert.Equal(t, enRelease, o.New[0].Title)
}

func TestEvaluateUnknownSourceIsAnError(t *testing.T) {
	w := acceptanceWatch()
	w.Sources = []string{"nowhere"}
	engine := NewEngine(EngineDeps{Store: newFakeStore()})

	o := engine.Evaluate(context.Background(), *w)

	require.Len(t, o.Errs, 1)
	assert.Contains(t, o.Errs[0].Error(), "unknown source")
	assert.Empty(t, o.Raw)
}

func TestEvaluateInvalidRegexIsAnError(t *testing.T) {
	source := sourceWith("jackett", enRelease)
	w := &Watch{ID: "broken", Queries: []string{"q"}, Sources: []string{"jackett"}, IncludeRegex: "("}
	engine := NewEngine(EngineDeps{Sources: []SearchSource{source}, Store: newFakeStore()})

	o := engine.Evaluate(context.Background(), *w)

	require.Len(t, o.Errs, 1)
	assert.Contains(t, o.Errs[0].Error(), "compile regex")
	assert.Empty(t, o.Matched)
}

func TestEvaluateSeenKeysFailureIsAnError(t *testing.T) {
	store := newFakeStore()
	store.seenKeysErr = errors.New("database is locked")
	source := sourceWith("jackett", enRelease)
	w := acceptanceWatch()
	w.Queries = []string{"One Night Only 2026"}
	w.Sources = []string{"jackett"}
	engine := NewEngine(EngineDeps{Sources: []SearchSource{source}, Store: store})

	o := engine.Evaluate(context.Background(), *w)

	require.Len(t, o.Errs, 1)
	assert.Contains(t, o.Errs[0].Error(), "database is locked")
	assert.Len(t, o.Matched, 1)
	assert.Empty(t, o.New)
}

func TestRunCycleSeedsSilentlyThenPublishesOnce(t *testing.T) {
	w := acceptanceWatch()
	w.Queries = []string{"One Night Only 2026"}
	w.Sources = []string{"jackett"}
	store := newFakeStore(w)
	pub := &fakePublisher{}
	source := sourceWith("jackett", ruRelease)
	engine := NewEngine(EngineDeps{Sources: []SearchSource{source}, Store: store, Publisher: pub})

	require.NoError(t, engine.RunCycle(context.Background()))

	assert.Empty(t, pub.published, "the first cycle must seed silently")
	assert.Equal(t, []string{w.ID}, store.seeded)
	assert.Equal(t, []string{ruRelease}, store.seenTitles(w.ID))
	assert.Equal(t, "", store.statuses[w.ID])

	source.replies = []sourceReply{{results: []SearchResult{
		{Source: "jackett", ExternalID: "1", Title: ruRelease},
		{Source: "jackett", ExternalID: "2", Title: enRelease},
	}}}

	require.NoError(t, engine.RunCycle(context.Background()))

	require.Len(t, pub.published, 1)
	require.Len(t, pub.published[0].New, 1)
	assert.Equal(t, enRelease, pub.published[0].New[0].Title)
	assert.Len(t, pub.published[0].Matched, 2)

	require.NoError(t, engine.RunCycle(context.Background()))

	assert.Len(t, pub.published, 1, "a repeat cycle must publish nothing")
	assert.Equal(t, 3, store.runs[w.ID])
}

func TestRunCycleSeedOnCleanCycleWithoutMatches(t *testing.T) {
	w := acceptanceWatch()
	w.Queries = []string{"One Night Only 2026"}
	w.Sources = []string{"jackett"}
	store := newFakeStore(w)
	source := sourceWith("jackett", "Bee Gees One Night Only 1998 WEBRip")
	engine := NewEngine(EngineDeps{Sources: []SearchSource{source}, Store: store, Publisher: &fakePublisher{}})

	require.NoError(t, engine.RunCycle(context.Background()))

	assert.Equal(t, []string{w.ID}, store.seeded)
	assert.Empty(t, store.seenTitles(w.ID))
}

func TestRunCycleDoesNotSeedOnAFailedFirstCycle(t *testing.T) {
	w := acceptanceWatch()
	w.Queries = []string{"One Night Only 2026"}
	w.Sources = []string{"extto", "jackett"}
	store := newFakeStore(w)
	failing := &fakeSource{name: "extto", replies: []sourceReply{{err: errors.New("challenge twice")}}}
	engine := NewEngine(EngineDeps{
		Sources:   []SearchSource{failing, sourceWith("jackett", ruRelease)},
		Store:     store,
		Publisher: &fakePublisher{},
	})

	require.NoError(t, engine.RunCycle(context.Background()))

	assert.Empty(t, store.seeded)
	assert.Equal(t, []string{ruRelease}, store.seenTitles(w.ID))
	assert.Contains(t, store.statuses[w.ID], "challenge twice")
}

func TestRunCycleNeverMarksFilteredItemsSeen(t *testing.T) {
	w := acceptanceWatch()
	w.Queries = []string{"One Night Only 2026"}
	w.Sources = []string{"jackett"}
	store := newFakeStore(w)
	source := sourceWith("jackett", enRelease, "Def Leppard - One Night Only: Live At The Leadmill [2024]")
	engine := NewEngine(EngineDeps{Sources: []SearchSource{source}, Store: store, Publisher: &fakePublisher{}})

	require.NoError(t, engine.RunCycle(context.Background()))

	assert.Equal(t, []string{enRelease}, store.seenTitles(w.ID))
}

func TestRunCycleFailedPublishKeepsTheDelta(t *testing.T) {
	seeded := time.Now().Add(-time.Hour)
	w := acceptanceWatch()
	w.Queries = []string{"One Night Only 2026"}
	w.Sources = []string{"jackett"}
	w.SeededAt = &seeded
	store := newFakeStore(w)
	pub := &fakePublisher{err: errors.New("no responders")}
	engine := NewEngine(EngineDeps{
		Sources:   []SearchSource{sourceWith("jackett", enRelease)},
		Store:     store,
		Publisher: pub,
	})

	require.NoError(t, engine.RunCycle(context.Background()))

	assert.Empty(t, store.seenTitles(w.ID))
	assert.Contains(t, store.statuses[w.ID], "no responders")

	pub.err = nil
	require.NoError(t, engine.RunCycle(context.Background()))

	require.Len(t, pub.published, 1)
	assert.Equal(t, []string{enRelease}, store.seenTitles(w.ID))
	assert.Equal(t, "", store.statuses[w.ID])
}

func TestRunCyclePublishesThenMarksExactlyThePublishedItems(t *testing.T) {
	seeded := time.Now().Add(-time.Hour)
	w := acceptanceWatch()
	w.Queries = []string{"One Night Only 2026"}
	w.Sources = []string{"jackett"}
	w.SeededAt = &seeded
	store := newFakeStore(w)
	require.NoError(t, store.MarkSeen(w.ID, []SearchResult{{Source: "jackett", ExternalID: "1", Title: ruRelease}}))

	pub := &fakePublisher{}
	messages := make(chan string, 1)
	source := sourceWith("jackett", ruRelease, enRelease, "Bee Gees One Night Only 1998 WEBRip")
	engine := NewEngine(EngineDeps{
		Sources:   []SearchSource{source},
		Store:     store,
		Publisher: pub,
		Messages:  messages,
	})

	require.NoError(t, engine.RunCycle(context.Background()))

	require.Len(t, pub.published, 1)
	require.Len(t, pub.published[0].New, 1)
	assert.Equal(t, enRelease, pub.published[0].New[0].Title)
	assert.ElementsMatch(t, []string{ruRelease, enRelease}, store.seenTitles(w.ID))
	assert.Equal(t, "", store.statuses[w.ID])
}

func TestRunCycleMirrorsPublishedHitsToTelegram(t *testing.T) {
	seeded := time.Now().Add(-time.Hour)
	w := acceptanceWatch()
	w.Queries = []string{"One Night Only 2026"}
	w.Sources = []string{"jackett"}
	w.SeededAt = &seeded
	store := newFakeStore(w)
	messages := make(chan string, 1)
	engine := NewEngine(EngineDeps{
		Sources:   []SearchSource{sourceWith("jackett", enRelease)},
		Store:     store,
		Publisher: &fakePublisher{},
		Messages:  messages,
	})

	require.NoError(t, engine.RunCycle(context.Background()))

	select {
	case msg := <-messages:
		assert.Contains(t, msg, `one\-night\-only\-en`, "reserved chars must be escaped for MarkdownV2")
		assert.Contains(t, msg, "jackett")
		assert.Contains(t, msg, `One\.Night\.Only\.2026`)
	default:
		t.Fatal("no admin message was sent for a published hit")
	}
}

func TestRunCycleSendsNoTelegramMessageWhenPublishFails(t *testing.T) {
	seeded := time.Now().Add(-time.Hour)
	w := acceptanceWatch()
	w.Queries = []string{"One Night Only 2026"}
	w.Sources = []string{"jackett"}
	w.SeededAt = &seeded
	messages := make(chan string, 1)
	engine := NewEngine(EngineDeps{
		Sources:   []SearchSource{sourceWith("jackett", enRelease)},
		Store:     newFakeStore(w),
		Publisher: &fakePublisher{err: errors.New("no responders")},
		Messages:  messages,
	})

	require.NoError(t, engine.RunCycle(context.Background()))

	assert.Empty(t, messages)
}

func TestRunCycleDoesNotBlockOnAnUnreadMessageChannel(t *testing.T) {
	seeded := time.Now().Add(-time.Hour)
	w := acceptanceWatch()
	w.Queries = []string{"One Night Only 2026"}
	w.Sources = []string{"jackett"}
	w.SeededAt = &seeded
	store := newFakeStore(w)
	pub := &fakePublisher{}
	engine := NewEngine(EngineDeps{
		Sources:   []SearchSource{sourceWith("jackett", enRelease)},
		Store:     store,
		Publisher: pub,
		// unbuffered and nobody is reading, exactly like main.go's channel with a stalled consumer
		Messages: make(chan string),
	})

	done := make(chan error, 1)
	go func() {
		done <- engine.RunCycle(context.Background())
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the cycle blocked on the admin message channel")
	}

	require.Len(t, pub.published, 1)
	assert.Equal(t, []string{enRelease}, store.seenTitles(w.ID))
}

func TestRunCycleSkipsAndDisablesAnExpiredWatch(t *testing.T) {
	expires := time.Now().Add(-time.Minute)
	w := acceptanceWatch()
	w.Sources = []string{"jackett"}
	w.ExpiresAt = &expires
	store := newFakeStore(w)
	source := sourceWith("jackett", enRelease)
	engine := NewEngine(EngineDeps{Sources: []SearchSource{source}, Store: store, Publisher: &fakePublisher{}})

	require.NoError(t, engine.RunCycle(context.Background()))

	assert.Empty(t, source.calls, "an expired watch must not be searched")
	assert.Equal(t, []string{w.ID}, store.disabled)
	assert.Equal(t, "expired", store.statuses[w.ID])
	assert.Equal(t, 1, store.runs[w.ID])
}

func TestRunCycleReportsAStoreFailure(t *testing.T) {
	store := newFakeStore()
	store.cycleErr = errors.New("database is locked")
	engine := NewEngine(EngineDeps{Store: store})

	err := engine.RunCycle(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "load watches")
}

func TestRunCycleStopsOnCancelledContextWithoutRecording(t *testing.T) {
	w := acceptanceWatch()
	w.Sources = []string{"jackett"}
	store := newFakeStore(w)
	engine := NewEngine(EngineDeps{
		Sources:   []SearchSource{sourceWith("jackett", enRelease)},
		Store:     store,
		Publisher: &fakePublisher{},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, engine.RunCycle(ctx))

	assert.Equal(t, 0, store.runs[w.ID])
}

func TestRunCycleWithoutPublisherDoesNotMarkSeen(t *testing.T) {
	seeded := time.Now().Add(-time.Hour)
	w := acceptanceWatch()
	w.Queries = []string{"One Night Only 2026"}
	w.Sources = []string{"jackett"}
	w.SeededAt = &seeded
	store := newFakeStore(w)
	engine := NewEngine(EngineDeps{Sources: []SearchSource{sourceWith("jackett", enRelease)}, Store: store})

	require.NoError(t, engine.RunCycle(context.Background()))

	assert.Empty(t, store.seenTitles(w.ID))
	assert.Contains(t, store.statuses[w.ID], "no publisher configured")
}
