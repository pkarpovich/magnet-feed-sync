package downloads

import (
	"testing"

	"magnet-feed-sync/app/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKnownTorrentStatesCoversTheVocabulary(t *testing.T) {
	require.Len(t, knownTorrentStates, 21)
}

func TestClassify(t *testing.T) {
	// the two failing states classify the same way whatever the progress, so they are listed
	// apart from the states whose outcome depends on it
	failing := map[string]struct{}{"error": {}, "missingFiles": {}}
	neverTerminal := map[string]struct{}{
		"checkingUP":         {},
		"checkingResumeData": {},
		"moving":             {},
		"allocating":         {},
	}

	finished := types.TorrentState{Hash: "abc", Progress: 1, CompletionOn: 1786626099}
	running := types.TorrentState{Hash: "abc", Progress: 0, CompletionOn: -1}

	states := append(append([]string{}, knownTorrentStates...), "someFutureState")

	for _, state := range states {
		t.Run(state, func(t *testing.T) {
			done, pending := finished, running
			done.State, pending.State = state, state

			switch {
			case isIn(failing, state):
				for _, s := range []types.TorrentState{done, pending} {
					c := Classify(s, true)
					assert.Equal(t, "failed", c.Status)
					assert.Equal(t, "qbittorrent state: "+state, c.Reason)
					assert.True(t, c.Terminal())
					assert.False(t, c.Completed())
				}
			case isIn(neverTerminal, state):
				assert.Equal(t, Classification{}, Classify(done, true))
				assert.Equal(t, Classification{}, Classify(pending, true))
			default:
				c := Classify(done, true)
				assert.Equal(t, "completed", c.Status)
				assert.Empty(t, c.Reason)
				assert.True(t, c.Completed())
				assert.Equal(t, Classification{}, Classify(pending, true))
			}

			assert.Equal(
				t,
				Classification{Status: "failed", Reason: "torrent no longer present in qbittorrent"},
				Classify(done, false),
			)
		})
	}
}

func TestClassifyRejectsUnusableCompletionOn(t *testing.T) {
	// -1 is what an unfinished torrent reports, so the criterion is `> 0` and never `!= 0`
	for _, completionOn := range []int64{0, -1} {
		c := Classify(types.TorrentState{State: "stalledUP", Progress: 1, CompletionOn: completionOn}, true)

		assert.Equal(t, Classification{}, c, "completion_on %d must not complete", completionOn)
	}
}

func TestClassifyRejectsPartialProgress(t *testing.T) {
	for _, progress := range []float64{0, 0.5, 0.999} {
		c := Classify(types.TorrentState{State: "stalledUP", Progress: progress, CompletionOn: 1786626099}, true)

		assert.Equal(t, Classification{}, c, "progress %v must not complete", progress)
	}
}

func TestSubjectIsTheOneTheResponseHandsBack(t *testing.T) {
	assert.Equal(t, "tuclaw.downloads.completed.a1b2c3d4e5f60718", Subject("a1b2c3d4e5f60718"))
}

func TestClassifyIgnoresStateWhenHashIsMissing(t *testing.T) {
	c := Classify(types.TorrentState{}, false)

	assert.Equal(t, "failed", c.Status)
	assert.Equal(t, "torrent no longer present in qbittorrent", c.Reason)
}

func isIn(set map[string]struct{}, state string) bool {
	_, ok := set[state]

	return ok
}
