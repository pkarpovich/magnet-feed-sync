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

// its own type on purpose: a failed search must hold the seed back, an unconfigured source
// must not — that one never recovers and would leave the watch unseeded, and so silent, forever
type unavailableSourceError struct {
	name string
}

func (e unavailableSourceError) Error() string {
	return e.name + ": source is not configured"
}

type RunOutcome struct {
	Matched []SearchResult
	New     []SearchResult
	Raw     []SearchResult
	Errs    []error
}

type publisher interface {
	Publish(ctx context.Context, w Watch, o RunOutcome) error
}

type watchStore interface {
	WatchesForCycle() ([]*Watch, error)
	SeenKeys(watchID string) (map[string]struct{}, error)
	MarkSeen(watchID string, results []SearchResult) error
	MarkSeeded(id string) error
	Disable(id string) error
	RecordRun(watchID, status string) error
}

type EngineDeps struct {
	Sources   []SearchSource
	Store     watchStore
	Publisher publisher
	Messages  chan<- string
}

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

// errors rather than succeeding quietly: a silent success would mark releases seen unsent
type nopPublisher struct{}

func (nopPublisher) Publish(context.Context, Watch, RunOutcome) error {
	return errors.New("publish: no publisher configured")
}

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

	// each source bounds its own call; a second table keyed on source names would drift from it
	for _, query := range queries {
		found, err := source.Search(ctx, query)
		if err != nil {
			// later queries are skipped (ext.to holds the single solver slot up to 180s), but what
			// earlier ones returned is kept: dropping it would withhold a genuinely found release
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
	// ad-hoc regexes from POST /api/search are not cached: a caller could grow the map unbounded
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

// keyed on the regex source, not the watch: a PATCH must never be served a stale compiled value
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

func escapeMarkdown(text string) string {
	return tbapi.EscapeText(tbapi.ModeMarkdownV2, strings.ReplaceAll(text, "\\", "\\\\"))
}

func (e *Engine) seed(w Watch, o RunOutcome) string {
	if err := e.store.MarkSeen(w.ID, o.Matched); err != nil {
		return err.Error()
	}

	// a partially failed run would bury releases the failed source never reported
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

// runes, not bytes: statuses carry Cyrillic queries
func truncateStatus(status string) string {
	runes := []rune(status)
	if len(runes) <= maxStatusLength {
		return status
	}

	return string(runes[:maxStatusLength])
}
