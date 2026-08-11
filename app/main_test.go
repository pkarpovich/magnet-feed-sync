package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"magnet-feed-sync/app/config"
	"magnet-feed-sync/app/tracker/providers"
	"magnet-feed-sync/app/watcher"
)

func TestStaleRunAfter(t *testing.T) {
	tests := []struct {
		name string
		cron string
		want time.Duration
	}{
		{name: "hourly", cron: "0 * * * *", want: 2 * time.Hour},
		{name: "every 15 minutes", cron: "*/15 * * * *", want: 30 * time.Minute},
		// the first gap is 1h but the schedule is silent for 23h, so a window built from the
		// first gap alone would report 503 for most of the day
		{name: "clustered", cron: "0 9,10 * * *", want: 46 * time.Hour},
		{name: "invalid", cron: "not a cron", want: staleRunFallback},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, staleRunAfter(tt.cron))
		})
	}
}

type stubSolver struct{}

func (stubSolver) Solve(context.Context, string) (*providers.SolvedPage, error) {
	return &providers.SolvedPage{}, nil
}

func sourceNames(set watchSourceSet) []string {
	names := make([]string, 0, len(set.list))
	for _, source := range set.list {
		names = append(names, source.Name())
	}

	return names
}

func TestWatcherSourcesFullyConfigured(t *testing.T) {
	cfg := &config.Config{Jackett: config.JackettConfig{
		URL:       "http://jackett:9117",
		PublicURL: "https://jackett.example.com",
		APIKey:    "secret-key",
	}}

	set := watcherSources(cfg, stubSolver{})

	assert.Equal(t, []string{"jackett", "extto"}, sourceNames(set))
	assert.NotNil(t, set.extto)
}

// TestDegradedNoJackettKey: the live JACKETT_URL carries no api key, so the source cannot
// search — it is dropped with a warning instead of taking the service down.
func TestDegradedNoJackettKey(t *testing.T) {
	cfg := &config.Config{Jackett: config.JackettConfig{URL: "http://jackett:9117"}}

	set := watcherSources(cfg, stubSolver{})

	assert.Equal(t, []string{"extto"}, sourceNames(set))
	assert.NotNil(t, set.extto)
}

func TestDegradedNoSolver(t *testing.T) {
	cfg := &config.Config{Jackett: config.JackettConfig{
		URL:    "http://jackett:9117",
		APIKey: "secret-key",
	}}

	set := watcherSources(cfg, nil)

	assert.Equal(t, []string{"jackett"}, sourceNames(set))
	assert.Nil(t, set.extto)
}

func TestDegradedNoNATS(t *testing.T) {
	publisher := watcher.NewPublisher(watcher.PublisherOptions{URL: ""})
	require.NotNil(t, publisher)
	t.Cleanup(publisher.Close)

	err := publisher.Publish(context.Background(), watcher.Watch{ID: "one-night-only-en"}, watcher.RunOutcome{})

	// a disabled publisher must error, never succeed quietly: a quiet success would let the
	// engine mark releases seen that nobody was ever told about
	require.Error(t, err)
}

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "userinfo", in: "http://user:s3cret@solver:8191/v1", want: "http://redacted@solver:8191/v1"},
		{name: "api key", in: "http://jackett:9117/api?apikey=s3cret", want: "http://jackett:9117/api?apikey=redacted"},
		{name: "nothing to hide", in: "http://solver:8191/v1", want: "http://solver:8191/v1"},
		{name: "unparsable", in: "://nope", want: "<invalid url>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, redactURL(tt.in))
		})
	}
}
