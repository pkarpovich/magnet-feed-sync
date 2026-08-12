package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
)

const (
	maxStatusLength = 300
	maxMessageItems = 10
)

// unavailableSourceError names a source a watch references but this process does not run —
// no jackett api key, no solver. It is deliberately its own type: a search that *failed* is
// transient and must hold the seed back, while a source that is not configured at all is a
// standing fact, and treating it as a failure would leave the watch unseeded forever and
// therefore permanently silent.
type unavailableSourceError struct {
	name string
}

func (e unavailableSourceError) Error() string {
	return e.name + ": source is not configured"
}

// RunOutcome is what one evaluation of a watch found: everything the sources returned after
// dedup (Raw), what survived the regex filters (Matched), what was never announced before
// (New) and whatever went wrong on the way (Errs).
type RunOutcome struct {
	Matched []SearchResult
	New     []SearchResult
	Raw     []SearchResult
	Errs    []error
}

// publisher is the consumer-side view of the NATS notification. The real JetStream
// implementation is injected from main.go.
type publisher interface {
	Publish(ctx context.Context, w Watch, o RunOutcome) error
}

// watchStore is the consumer-side view of the watch repository.
type watchStore interface {
	WatchesForCycle() ([]*Watch, error)
	SeenKeys(watchID string) (map[string]struct{}, error)
	MarkSeen(watchID string, results []SearchResult) error
	MarkSeeded(id string) error
	Disable(id string) error
	RecordRun(watchID, status string) error
}

// EngineDeps carries what the engine needs; all of it is injected from main.go.
type EngineDeps struct {
	Sources   []SearchSource
	Store     watchStore
	Publisher publisher
	Messages  chan<- string
}

// Engine is the single place a watch is evaluated, so the cron cycle and the reproduction
// endpoint cannot drift apart.
type Engine struct {
	sources   []SearchSource
	store     watchStore
	publisher publisher
	messages  chan<- string

	mu      sync.Mutex
	regexes map[string]*regexp.Regexp
}

func NewEngine(d EngineDeps) *Engine {
	pub := d.Publisher
	if pub == nil {
		pub = nopPublisher{}
	}

	return &Engine{
		sources:   d.Sources,
		store:     d.Store,
		publisher: pub,
		messages:  d.Messages,
		regexes:   make(map[string]*regexp.Regexp),
	}
}

// nopPublisher is what an engine built without a publisher gets. It errors rather than
// succeeding quietly, because a silent success would let RunCycle mark releases seen that
// nobody was ever told about.
type nopPublisher struct{}

func (nopPublisher) Publish(context.Context, Watch, RunOutcome) error {
	return errors.New("publish: no publisher configured")
}

// Evaluate searches, merges, filters and diffs. It is side-effect free: RunCycle owns every
// write, so the reproduction endpoint can share this path.
func (e *Engine) Evaluate(ctx context.Context, w Watch) RunOutcome {
	var o RunOutcome

	o.Raw, o.Errs = e.collect(ctx, w)

	matched, err := e.filter(w, o.Raw)
	if err != nil {
		o.Errs = append(o.Errs, err)

		return o
	}
	o.Matched = matched

	o.New, err = e.delta(w, o.Matched)
	if err != nil {
		o.Errs = append(o.Errs, err)
	}

	return o
}

// collect runs every query of every source sequentially: FlareSolverr must not be
// parallelised and Jackett must not be hammered. A failing source stops at its failing query
// but keeps whatever its earlier queries returned, and never aborts the sources after it.
func (e *Engine) collect(ctx context.Context, w Watch) ([]SearchResult, []error) {
	var (
		raw  []SearchResult
		errs []error
	)

	seen := make(map[string]struct{})
	for _, name := range w.Sources {
		source := e.sourceByName(name)
		if source == nil {
			errs = append(errs, unavailableSourceError{name: name})

			continue
		}

		results, err := e.searchAll(ctx, source, w.Queries)
		if err != nil {
			errs = append(errs, err)
		}

		for _, result := range results {
			key := result.SeenKey()
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}

			raw = append(raw, result)
		}
	}

	return raw, errs
}

func (e *Engine) searchAll(ctx context.Context, source SearchSource, queries []string) ([]SearchResult, error) {
	var results []SearchResult

	// no timeout is applied here: every source bounds its own call with the same constant,
	// and a second copy of that table keyed on source names would silently disagree with it
	for _, query := range queries {
		found, err := source.Search(ctx, query)
		if err != nil {
			// the queries after the failure are not attempted — a source that just refused
			// us will refuse them too, and on ext.to each attempt holds the single solver
			// slot for up to 180s. What it already returned is still handed back: dropping
			// it would withhold a release that was genuinely found, which is the silent
			// miss the publish-then-mark ordering exists to prevent
			return results, fmt.Errorf("%s %q: %w", source.Name(), query, err)
		}

		results = append(results, found...)
	}

	return results, nil
}

func (e *Engine) sourceByName(name string) SearchSource {
	for _, source := range e.sources {
		if source.Name() == name {
			return source
		}
	}

	return nil
}

func (e *Engine) filter(w Watch, raw []SearchResult) ([]SearchResult, error) {
	// only a stored watch's patterns are cached: POST /api/search takes its regexes from the
	// request body, and caching those would let a caller grow the map without bound
	cache := w.ID != ""

	include, err := e.compile(w.IncludeRegex, cache)
	if err != nil {
		return nil, err
	}

	exclude, err := e.compile(w.ExcludeRegex, cache)
	if err != nil {
		return nil, err
	}

	var matched []SearchResult
	for _, result := range raw {
		if include != nil && !include.MatchString(result.Title) {
			continue
		}
		if exclude != nil && exclude.MatchString(result.Title) {
			continue
		}

		matched = append(matched, result)
	}

	return matched, nil
}

// compile caches on the regex source string rather than on the watch, so a PATCH that
// changes a pattern can never be served a stale compiled value.
func (e *Engine) compile(pattern string, cache bool) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if compiled, ok := e.regexes[pattern]; ok {
		return compiled, nil
	}

	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("compile regex %q: %w", pattern, err)
	}
	if cache {
		e.regexes[pattern] = compiled
	}

	return compiled, nil
}

// delta reports what was never announced. An ad-hoc watch has no id and therefore no seen
// set: everything it matched is new.
func (e *Engine) delta(w Watch, matched []SearchResult) ([]SearchResult, error) {
	if w.ID == "" {
		return matched, nil
	}

	seen, err := e.store.SeenKeys(w.ID)
	if err != nil {
		return nil, fmt.Errorf("load seen keys of watch %s: %w", w.ID, err)
	}

	var fresh []SearchResult
	for _, result := range matched {
		if _, ok := seen[result.SeenKey()]; ok {
			continue
		}

		fresh = append(fresh, result)
	}

	return fresh, nil
}

// RunCycle evaluates every watch the cron owns and applies the effects.
func (e *Engine) RunCycle(ctx context.Context) error {
	watches, err := e.store.WatchesForCycle()
	if err != nil {
		return fmt.Errorf("load watches: %w", err)
	}

	for _, w := range watches {
		if ctx.Err() != nil {
			return nil
		}

		e.runWatch(ctx, *w)
	}

	return nil
}

func (e *Engine) runWatch(ctx context.Context, w Watch) {
	if expired(w) {
		e.disable(w)

		return
	}

	o := e.Evaluate(ctx, w)
	// a sweep cancelled by shutdown must not record the aborted parse as a watch failure
	if ctx.Err() != nil {
		return
	}

	e.recordRun(w, e.applyEffects(ctx, w, o))
}

// applyEffects performs the writes a cycle owes and returns the status to record. Ordering
// is load-bearing: publish first, mark seen only after the ack. A crash between the two
// costs one duplicate wake; the reverse loses the release permanently and silently.
func (e *Engine) applyEffects(ctx context.Context, w Watch, o RunOutcome) string {
	status := statusOf(o.Errs)

	if w.SeededAt == nil {
		return joinStatus(status, e.seed(w, o))
	}

	if len(o.New) == 0 {
		return status
	}

	if err := e.publisher.Publish(ctx, w, o); err != nil {
		slog.ErrorContext(ctx, "failed to publish watch hit", "watch_id", w.ID, "error", err)

		return joinStatus(status, err.Error())
	}

	e.notify(ctx, w, o.New)

	if err := e.store.MarkSeen(w.ID, o.New); err != nil {
		return joinStatus(status, err.Error())
	}

	return status
}

// notify mirrors every published hit to the admin channel. An event task fires once and
// completes, so a missed re-arm on the agent side makes the watch publish to nobody — and
// that silence reads exactly like "no releases". The telegram copy makes it visible.
//
// The send is non-blocking on purpose: messagesForSend is unbuffered, so waiting on a
// stalled reader would wedge the whole cron behind it.
func (e *Engine) notify(ctx context.Context, w Watch, results []SearchResult) {
	if len(results) == 0 {
		return
	}

	select {
	case e.messages <- watchHitMessage(w, results):
	default:
		slog.WarnContext(ctx, "dropped watch notification, admin channel is not ready",
			"watch_id", w.ID, "new", len(results))
	}
}

func watchHitMessage(w Watch, results []SearchResult) string {
	lines := make([]string, 0, maxMessageItems+2)
	lines = append(lines, fmt.Sprintf("🔔 Watch %s found %d new release(s):", w.ID, len(results)))

	shown := results
	if len(shown) > maxMessageItems {
		shown = shown[:maxMessageItems]
	}
	for _, result := range shown {
		lines = append(lines, fmt.Sprintf("%s: %s", result.Source, result.Title))
	}
	if len(results) > len(shown) {
		lines = append(lines, fmt.Sprintf("and %d more", len(results)-len(shown)))
	}

	return escapeMarkdown(strings.Join(lines, "\n\n"))
}

// escapeMarkdown makes plain text safe for the MarkdownV2 parse mode every admin message is
// sent with (see events.NewMarkdownMessage) — telegram rejects the whole message when a
// reserved char such as '(' or '-' is left unescaped. '\' is doubled by hand first, because
// tbapi.EscapeText leaves it alone.
func escapeMarkdown(text string) string {
	return tbapi.EscapeText(tbapi.ModeMarkdownV2, strings.ReplaceAll(text, "\\", "\\\\"))
}

// seed records the current world silently: a fresh watch must not wake the agent with
// releases that already existed when it was created.
func (e *Engine) seed(w Watch, o RunOutcome) string {
	if err := e.store.MarkSeen(w.ID, o.Matched); err != nil {
		return err.Error()
	}

	// seeding from a partially failed run would bury every release the failed source
	// never reported, so the seed waits for a clean cycle. A source that is not configured
	// at all does not count: it never comes back on its own, and holding the seed for it
	// leaves the watch unseeded — and therefore silent — forever
	if hasSearchFailure(o.Errs) {
		return ""
	}

	if err := e.store.MarkSeeded(w.ID); err != nil {
		return err.Error()
	}

	return ""
}

func (e *Engine) disable(w Watch) {
	if err := e.store.Disable(w.ID); err != nil {
		slog.Error("failed to disable expired watch", "watch_id", w.ID, "error", err)
	}

	e.recordRun(w, "expired")
}

func (e *Engine) recordRun(w Watch, status string) {
	if err := e.store.RecordRun(w.ID, status); err != nil {
		slog.Error("failed to record watch run", "watch_id", w.ID, "error", err)
	}
}

func expired(w Watch) bool {
	return w.ExpiresAt != nil && !w.ExpiresAt.After(time.Now())
}

// hasSearchFailure reports whether anything that could succeed on a later cycle went wrong.
// An unconfigured source is excluded: it is a standing configuration fact, still reported in
// last_status and on /api/health, but never a reason to withhold the seed.
func hasSearchFailure(errs []error) bool {
	for _, err := range errs {
		var unavailable unavailableSourceError
		if errors.As(err, &unavailable) {
			continue
		}

		return true
	}

	return false
}

func statusOf(errs []error) string {
	messages := make([]string, 0, len(errs))
	for _, err := range errs {
		messages = append(messages, err.Error())
	}

	return truncateStatus(strings.Join(messages, "; "))
}

func joinStatus(status, addition string) string {
	if addition == "" {
		return status
	}
	if status == "" {
		return truncateStatus(addition)
	}

	return truncateStatus(status + "; " + addition)
}

// truncateStatus counts runes, not bytes: the statuses carry Cyrillic queries and a byte
// slice would store a broken rune.
func truncateStatus(status string) string {
	runes := []rune(status)
	if len(runes) <= maxStatusLength {
		return status
	}

	return string(runes[:maxStatusLength])
}
