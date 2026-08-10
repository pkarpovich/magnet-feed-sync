package tracker

import (
	"sync"
	"time"

	"magnet-feed-sync/app/tracker/providers"
)

const (
	initialCooldown = time.Hour
	maxCooldown     = 24 * time.Hour
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
	// set only by a half-open probe, so an untripped provider is never held back by it
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
	switch {
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
