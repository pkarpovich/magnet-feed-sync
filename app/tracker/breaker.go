package tracker

import (
	"sync"
	"time"

	"magnet-feed-sync/app/tracker/providers"
)

const (
	initialCooldown = time.Hour
	maxCooldown     = 24 * time.Hour
	// A lone Blocked fetch is far more often a solver timeout than a refusal (5 of alpha's 18
	// timeout runs between July and September were singles, each costing an hour of skipped
	// tasks), while a real block fails every fetch in a row. The streak spans runs and only a
	// successful fetch clears it.
	tripAfter = 2
)

// State is the observable circuit state of a single provider.
type State struct {
	Tripped     bool
	NextProbeAt time.Time
	Cooldown    time.Duration
}

type breakerEntry struct {
	state         State
	probedThisRun bool
	blockedStreak int
}

// Breaker keeps one circuit per provider so a blocked tracker stops burning requests every sweep.
type Breaker struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]*breakerEntry
}

// NewBreaker returns a breaker seeded with one untripped entry per provider name.
func NewBreaker(now func() time.Time, names ...string) *Breaker {
	if now == nil {
		now = time.Now
	}

	b := &Breaker{now: now, entries: make(map[string]*breakerEntry, len(names))}
	for _, name := range names {
		b.entries[name] = &breakerEntry{state: State{Cooldown: initialCooldown}}
	}

	return b
}

func (b *Breaker) BeginRun() {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, entry := range b.entries {
		entry.probedThisRun = false
	}
}

func (b *Breaker) Allow(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	entry := b.entry(name)
	// set only by a half-open probe, and deliberately not cleared by RecordSuccess: at most one
	// task per provider per run runs against a tracker that was blocked when the run started
	if entry.probedThisRun {
		return false
	}

	if !entry.state.Tripped {
		return true
	}

	if b.now().Before(entry.state.NextProbeAt) {
		return false
	}

	entry.probedThisRun = true
	return true
}

func (b *Breaker) RecordFailure(name string, kind providers.ErrorKind) {
	if kind != providers.KindBlocked {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	entry := b.entry(name)
	entry.blockedStreak++
	switch {
	case !entry.state.Tripped && entry.blockedStreak < tripAfter:
		return
	case !entry.state.Tripped:
		entry.state.Tripped = true
		entry.state.Cooldown = initialCooldown
	case entry.state.Cooldown < maxCooldown:
		entry.state.Cooldown *= 2
		if entry.state.Cooldown > maxCooldown {
			entry.state.Cooldown = maxCooldown
		}
	}

	entry.state.NextProbeAt = b.now().Add(entry.state.Cooldown)
}

func (b *Breaker) RecordSuccess(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	entry := b.entry(name)
	entry.blockedStreak = 0
	entry.state.Tripped = false
	entry.state.Cooldown = initialCooldown
	entry.state.NextProbeAt = time.Time{}
}

func (b *Breaker) Snapshot() map[string]State {
	b.mu.Lock()
	defer b.mu.Unlock()

	snapshot := make(map[string]State, len(b.entries))
	for name, entry := range b.entries {
		snapshot[name] = entry.state
	}

	return snapshot
}

func (b *Breaker) entry(name string) *breakerEntry {
	entry, ok := b.entries[name]
	if !ok {
		entry = &breakerEntry{state: State{Cooldown: initialCooldown}}
		b.entries[name] = entry
	}

	return entry
}
