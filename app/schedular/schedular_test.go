package schedular

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// neverCron fires on 1 January at midnight, so nothing runs on its own during a test.
const neverCron = "0 0 1 1 *"

func TestAddJobRegistersEveryJob(t *testing.T) {
	s, err := NewService()
	require.NoError(t, err)

	require.NoError(t, s.AddJob("files", "0 * * * *", func() {}))
	require.NoError(t, s.AddJob("watcher", "20 * * * *", func() {}))

	jobs := s.scheduler.Jobs()
	require.Len(t, jobs, 2)

	names := []string{jobs[0].Name(), jobs[1].Name()}
	assert.ElementsMatch(t, []string{"files", "watcher"}, names)
}

func TestAddJobInvalidCron(t *testing.T) {
	s, err := NewService()
	require.NoError(t, err)

	err = s.AddJob("watcher", "not a cron", func() {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "watcher")
}

func TestAddJobRunsInSingletonMode(t *testing.T) {
	s, err := NewService()
	require.NoError(t, err)

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	require.NoError(t, s.AddJob("watcher", neverCron, func() {
		started <- struct{}{}
		<-release
	}))

	s.Start()
	t.Cleanup(func() {
		close(release)
		_ = s.scheduler.Shutdown()
	})

	jobs := s.scheduler.Jobs()
	require.Len(t, jobs, 1)

	// the second trigger waits for the first run to be under way: gocron creates the
	// per-job singleton runner on the first dispatch, and two simultaneous triggers can both
	// race past that creation, which would fail the assertion on timing rather than on
	// behaviour
	require.NoError(t, jobs[0].RunNow())
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the job never ran")
	}

	// the first run blocks on release; without singleton mode the second would join it
	require.NoError(t, jobs[0].RunNow())

	time.Sleep(300 * time.Millisecond)
	assert.Empty(t, started, "a second run started while the first was still going")
}
