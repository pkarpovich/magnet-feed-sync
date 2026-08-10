package tracker

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"magnet-feed-sync/app/tracker/providers"
)

type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.now = c.now.Add(d)
}

func TestBreakerSeededProvidersAreOK(t *testing.T) {
	b := NewBreaker(nil, "rutracker", "nnm")

	snapshot := b.Snapshot()
	require.Len(t, snapshot, 2)
	for _, name := range []string{"rutracker", "nnm"} {
		assert.False(t, snapshot[name].Tripped, "%s should start untripped", name)
		assert.True(t, b.Allow(name), "%s should be allowed", name)
	}
}

func TestBreakerTripSkipsWithoutFetch(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)}
	b := NewBreaker(clock.Now, "rutracker", "nnm")

	b.RecordFailure("rutracker", providers.KindBlocked)

	assert.False(t, b.Allow("rutracker"), "blocked provider should be skipped before the cooldown expires")
	assert.True(t, b.Allow("nnm"), "an unrelated provider must stay allowed")

	state := b.Snapshot()["rutracker"]
	assert.True(t, state.Tripped)
	assert.Equal(t, time.Hour, state.Cooldown)
	assert.Equal(t, clock.now.Add(time.Hour), state.NextProbeAt)
}

func TestBreakerTripsOnlyOnBlocked(t *testing.T) {
	b := NewBreaker(nil, "rutracker")

	b.RecordFailure("rutracker", providers.KindTransient)
	b.RecordFailure("rutracker", providers.KindPermanent)

	assert.False(t, b.Snapshot()["rutracker"].Tripped)
	assert.True(t, b.Allow("rutracker"))
}

func TestBreakerProbeAfterCooldown(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)}
	b := NewBreaker(clock.Now, "rutracker")

	b.BeginRun()
	b.RecordFailure("rutracker", providers.KindBlocked)
	assert.False(t, b.Allow("rutracker"))

	clock.advance(time.Hour)
	b.BeginRun()
	require.True(t, b.Allow("rutracker"), "half-open probe should be allowed once the cooldown expired")
	assert.False(t, b.Allow("rutracker"), "only one probe per run is allowed")

	b.RecordSuccess("rutracker")
	assert.False(t, b.Allow("rutracker"), "a successful probe still ends the run for that provider")
	assert.False(t, b.Snapshot()["rutracker"].Tripped)

	b.BeginRun()
	assert.True(t, b.Allow("rutracker"), "the next run runs the provider normally")
}

func TestBreakerCooldownSequence(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)}
	b := NewBreaker(clock.Now, "rutracker")

	expected := []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour, 16 * time.Hour, 24 * time.Hour, 24 * time.Hour}

	got := make([]time.Duration, 0, len(expected))
	for range expected {
		b.BeginRun()
		b.RecordFailure("rutracker", providers.KindBlocked)

		state := b.Snapshot()["rutracker"]
		got = append(got, state.Cooldown)
		assert.Equal(t, clock.now.Add(state.Cooldown), state.NextProbeAt)

		clock.advance(state.Cooldown)
	}

	assert.Equal(t, expected, got)

	b.BeginRun()
	require.True(t, b.Allow("rutracker"))
	b.RecordSuccess("rutracker")
	assert.Equal(t, time.Hour, b.Snapshot()["rutracker"].Cooldown, "a successful probe resets the cooldown")
}

func TestBreakerUnknownProviderIsTracked(t *testing.T) {
	b := NewBreaker(nil)

	assert.True(t, b.Allow("jackett"))
	b.RecordFailure("jackett", providers.KindBlocked)

	assert.False(t, b.Allow("jackett"))
	assert.True(t, b.Snapshot()["jackett"].Tripped)
}
