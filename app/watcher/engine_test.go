package watcher

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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

	seenKeysCalls int

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
	s.mu.Lock()
	s.seenKeysCalls++
	s.mu.Unlock()

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

// mustExcludeTitles are the verbatim junk titles from the measured 226-result set that the
// acceptance watch has to reject.
var mustExcludeTitles = []string{
	"Bee Gees One Night Only 1998 WEBRip 1080p x264 AAC ENG Lulloz",
	"Def Leppard - One Night Only: Live At The Leadmill [2024, Classic Rock, Hard Rock, Blu-ray, 1080i]",
	"RuPauls Drag Race S15E02 One Night Only Part 2 1080p AMZN WEB DL DDP2 0 H 264 FLUX TGx",
	"One Night Only / Tian Liang Zhi Qian [2016, BDRemux 1080p] VO + DVO + Sub Rus, Eng + Original Chi",
}

func resultsFor(source string, titles ...string) []SearchResult {
	results := make([]SearchResult, 0, len(titles))
	for i, title := range titles {
		results = append(results, SearchResult{
			Source:     source,
			ExternalID: source + "-" + externalID(i),
			Title:      title,
			Query:      "One Night Only 2026",
		})
	}

	return results
}

// TestAcceptanceScenario runs the whole acceptance scenario over the Acceptance watch verbatim —
// both queries, both sources — rather than the trimmed watches the per-effect tests use.
func TestAcceptanceScenario(t *testing.T) {
	w := acceptanceWatch()
	store := newFakeStore(w)
	pub := &fakePublisher{}

	jackettTitles := append([]string{ruRelease}, mustExcludeTitles...)
	jackett := &fakeSource{name: "jackett", replies: []sourceReply{{results: resultsFor("jackett", jackettTitles...)}}}
	extto := &fakeSource{name: "extto", replies: []sourceReply{{results: resultsFor("extto", mustExcludeTitles...)}}}
	engine := NewEngine(EngineDeps{Sources: []SearchSource{jackett, extto}, Store: store, Publisher: pub})

	require.NoError(t, engine.RunCycle(context.Background()))

	assert.Empty(t, pub.published, "the first cycle must seed silently")
	assert.Equal(t, []string{w.ID}, store.seeded)
	assert.Equal(t, []string{ruRelease}, store.seenTitles(w.ID))
	assert.Equal(t, "", store.statuses[w.ID])
	assert.Equal(t, []string{"One Night Only 2026", "Только на одну ночь 2026"}, jackett.calls)

	jackett.replies = []sourceReply{{results: resultsFor("jackett", append([]string{ruRelease, enRelease}, mustExcludeTitles...)...)}}

	require.NoError(t, engine.RunCycle(context.Background()))

	require.Len(t, pub.published, 1)
	require.Len(t, pub.published[0].New, 1)
	assert.Equal(t, enRelease, pub.published[0].New[0].Title)
	assert.ElementsMatch(t, []string{ruRelease, enRelease}, store.seenTitles(w.ID))

	require.NoError(t, engine.RunCycle(context.Background()))

	assert.Len(t, pub.published, 1, "a repeat cycle must publish nothing")

	extto.replies = []sourceReply{{err: errors.New("challenge twice")}}

	require.NoError(t, engine.RunCycle(context.Background()))

	assert.Contains(t, store.statuses[w.ID], "challenge twice")
	assert.Len(t, pub.published, 1)
	assert.Equal(t, 4, store.runs[w.ID])
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
	// an ad-hoc watch has no id, so it must not consult the seen set at all — the two rows
	// are new because nothing was looked up, not because the lookup missed
	assert.Zero(t, store.seenKeysCalls)
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

// a query that fails stops the rest of that source's queries, but what the earlier ones
// already returned still counts: dropping it would withhold a release that was really found
// for as long as the failing query keeps failing
func TestEvaluateKeepsWhatEarlierQueriesReturned(t *testing.T) {
	source := &fakeSource{
		name: "jackett",
		replies: []sourceReply{
			{results: resultsFor("jackett", enRelease)},
			{err: errors.New("bad status: 502")},
			{results: resultsFor("jackett", ruRelease)},
		},
	}
	w := acceptanceWatch()
	w.Queries = []string{"One Night Only 2026", "Только на одну ночь 2026", "third"}
	w.Sources = []string{"jackett"}
	engine := NewEngine(EngineDeps{Sources: []SearchSource{source}, Store: newFakeStore()})

	o := engine.Evaluate(context.Background(), *w)

	require.Len(t, o.Errs, 1)
	assert.Contains(t, o.Errs[0].Error(), "bad status: 502")
	require.Len(t, o.New, 1)
	assert.Equal(t, enRelease, o.New[0].Title)
	// the third query is never issued: a source that just refused us will refuse it too
	assert.Len(t, source.calls, 2)
}

func TestEvaluateUnknownSourceIsAnError(t *testing.T) {
	w := acceptanceWatch()
	w.Sources = []string{"nowhere"}
	engine := NewEngine(EngineDeps{Store: newFakeStore()})

	o := engine.Evaluate(context.Background(), *w)

	require.Len(t, o.Errs, 1)
	assert.Contains(t, o.Errs[0].Error(), "source is not configured")
	assert.Empty(t, o.Raw)
}

// the compiled-regex cache is keyed on the pattern string, not on the watch, so a PATCH
// that changes a pattern can never be served the previous compiled value
func TestEvaluateUsesTheCurrentRegexAfterAChange(t *testing.T) {
	source := &fakeSource{
		name: "jackett",
		replies: []sourceReply{
			{results: resultsFor("jackett", enRelease, ruRelease)},
			{results: resultsFor("jackett", enRelease, ruRelease)},
		},
	}
	w := Watch{ID: "one-night-only-en", Queries: []string{"q"}, Sources: []string{"jackett"}, IncludeRegex: `(?i)WEB-DL`}
	engine := NewEngine(EngineDeps{Sources: []SearchSource{source}, Store: newFakeStore()})

	first := engine.Evaluate(context.Background(), w)
	require.Len(t, first.Matched, 1)
	assert.Equal(t, enRelease, first.Matched[0].Title)

	w.IncludeRegex = `(?i)TSRip`
	second := engine.Evaluate(context.Background(), w)

	require.Len(t, second.Matched, 1)
	assert.Equal(t, ruRelease, second.Matched[0].Title)
}

// POST /api/search takes its regexes from the request body; caching those would let a
// caller grow the map without bound for as long as the process runs
func TestEvaluateDoesNotCacheAdHocRegexes(t *testing.T) {
	stored := Watch{ID: "one-night-only-en", Queries: []string{"q"}, Sources: []string{"jackett"}, IncludeRegex: `(?i)WEB-DL`}
	adHoc := Watch{Queries: []string{"q"}, Sources: []string{"jackett"}, IncludeRegex: `(?i)TSRip`}
	engine := NewEngine(EngineDeps{
		Sources: []SearchSource{sourceWith("jackett", enRelease)},
		Store:   newFakeStore(),
	})

	engine.Evaluate(context.Background(), stored)
	engine.Evaluate(context.Background(), adHoc)
	engine.Evaluate(context.Background(), adHoc)

	engine.mu.Lock()
	defer engine.mu.Unlock()
	assert.Len(t, engine.regexes, 1)
	assert.Contains(t, engine.regexes, stored.IncludeRegex)
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

// A source the process does not run at all is a standing configuration fact, not a failed
// cycle. Holding the seed back for it would leave the watch unseeded — and therefore
// permanently silent — for as long as the deployment runs without a jackett key or a solver.
func TestRunCycleSeedsDespiteAnUnconfiguredSource(t *testing.T) {
	w := acceptanceWatch()
	w.Queries = []string{"One Night Only 2026"}
	w.Sources = []string{"extto", "jackett"}
	store := newFakeStore(w)
	engine := NewEngine(EngineDeps{
		Sources:   []SearchSource{sourceWith("jackett", ruRelease)},
		Store:     store,
		Publisher: &fakePublisher{},
	})

	require.NoError(t, engine.RunCycle(context.Background()))

	assert.Equal(t, []string{w.ID}, store.seeded)
	// the fact is still reported, it just does not withhold the seed
	assert.Contains(t, store.statuses[w.ID], "extto: source is not configured")
}

// the follow-on half of the same rule: once seeded, the watch keeps publishing from the
// sources that do run
func TestRunCyclePublishesWithAnUnconfiguredSource(t *testing.T) {
	seeded := time.Now().Add(-time.Hour)
	w := acceptanceWatch()
	w.Queries = []string{"One Night Only 2026"}
	w.Sources = []string{"extto", "jackett"}
	w.SeededAt = &seeded
	store := newFakeStore(w)
	pub := &fakePublisher{}
	engine := NewEngine(EngineDeps{
		Sources:   []SearchSource{sourceWith("jackett", enRelease)},
		Store:     store,
		Publisher: pub,
	})

	require.NoError(t, engine.RunCycle(context.Background()))

	require.Len(t, pub.published, 1)
	assert.Equal(t, []string{enRelease}, store.seenTitles(w.ID))
}

// a failed seed write must not be followed by MarkSeeded: the watch would be sealed as
// seeded while holding none of the rows it is supposed to suppress
func TestRunCycleFailedSeedWriteDoesNotMarkSeeded(t *testing.T) {
	w := acceptanceWatch()
	w.Queries = []string{"One Night Only 2026"}
	w.Sources = []string{"jackett"}
	store := newFakeStore(w)
	store.markSeenErr = errors.New("database is locked")
	engine := NewEngine(EngineDeps{
		Sources:   []SearchSource{sourceWith("jackett", enRelease)},
		Store:     store,
		Publisher: &fakePublisher{},
	})

	require.NoError(t, engine.RunCycle(context.Background()))

	assert.Empty(t, store.seeded)
	assert.Contains(t, store.statuses[w.ID], "database is locked")
}

// publish succeeded but the seen write did not: the release stays in the delta and is
// retried, which is the deliberate cost of publishing before marking
func TestRunCycleFailedMarkSeenKeepsTheDelta(t *testing.T) {
	seeded := time.Now().Add(-time.Hour)
	w := acceptanceWatch()
	w.Queries = []string{"One Night Only 2026"}
	w.Sources = []string{"jackett"}
	w.SeededAt = &seeded
	store := newFakeStore(w)
	store.markSeenErr = errors.New("database is locked")
	pub := &fakePublisher{}
	engine := NewEngine(EngineDeps{
		Sources:   []SearchSource{sourceWith("jackett", enRelease)},
		Store:     store,
		Publisher: pub,
	})

	require.NoError(t, engine.RunCycle(context.Background()))

	require.Len(t, pub.published, 1)
	assert.Empty(t, store.seenTitles(w.ID))
	assert.Contains(t, store.statuses[w.ID], "database is locked")
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

// last_status is stored as a string of runes, not bytes: the statuses carry Cyrillic
// queries, and cutting on a byte boundary would store half a rune
func TestRecordedStatusIsTruncatedOnRuneBoundaries(t *testing.T) {
	w := acceptanceWatch()
	w.Queries = []string{strings.Repeat("ночь", 200)}
	w.Sources = []string{"jackett"}
	store := newFakeStore(w)
	failing := &fakeSource{name: "jackett", replies: []sourceReply{{err: errors.New(strings.Repeat("сбой", 200))}}}
	engine := NewEngine(EngineDeps{Sources: []SearchSource{failing}, Store: store, Publisher: &fakePublisher{}})

	require.NoError(t, engine.RunCycle(context.Background()))

	status := store.statuses[w.ID]
	assert.Equal(t, maxStatusLength, len([]rune(status)))
	assert.True(t, utf8.ValidString(status), "a byte-sliced status would end in a broken rune")
}

// the telegram mirror is capped, and the tail has to say so — a silently trimmed list reads
// like the whole delta
func TestWatchHitMessageReportsWhatItTrimmed(t *testing.T) {
	results := make([]SearchResult, 0, maxMessageItems+2)
	for i := range maxMessageItems + 2 {
		results = append(results, SearchResult{Source: "jackett", ExternalID: strconv.Itoa(i), Title: enRelease})
	}

	message := watchHitMessage(Watch{ID: "one-night-only-en"}, results)

	assert.Equal(t, maxMessageItems, strings.Count(message, "jackett: "))
	assert.Contains(t, message, "and 2 more")
}

// the cron cycle and both search endpoints drive one Engine at once, and the regex cache is
// the state they share — this is what gives -race something to observe
func TestEvaluateIsSafeUnderConcurrentUse(t *testing.T) {
	stored := Watch{ID: "one-night-only-en", Queries: []string{"q"}, Sources: []string{"jackett"}, IncludeRegex: acceptanceInclude}
	engine := NewEngine(EngineDeps{
		Sources: []SearchSource{&concurrentSource{name: "jackett", results: resultsFor("jackett", enRelease, ruRelease)}},
		Store:   newFakeStore(),
	})

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()

			w := stored
			// half of the goroutines run the stored watch, half an ad-hoc one with a pattern
			// of its own — the two take different branches of the cache
			if i%2 == 1 {
				w.ID = ""
				w.IncludeRegex = `(?i)` + strconv.Itoa(i) + `|one night only`
			}

			o := engine.Evaluate(context.Background(), w)
			assert.Empty(t, o.Errs)
		}()
	}
	wg.Wait()
}

// concurrentSource is the goroutine-safe counterpart of fakeSource: it always answers the
// same thing, so it needs no reply script and no call log.
type concurrentSource struct {
	name    string
	results []SearchResult
}

func (s *concurrentSource) Name() string { return s.name }

func (s *concurrentSource) Search(context.Context, string) ([]SearchResult, error) {
	return s.results, nil
}
