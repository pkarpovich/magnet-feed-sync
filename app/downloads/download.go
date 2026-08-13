package downloads

import (
	"time"

	"magnet-feed-sync/app/types"
)

const (
	statusCompleted = "completed"
	statusFailed    = "failed"
)

const reasonMissing = "torrent no longer present in qbittorrent"

var knownTorrentStates = []string{
	"error", "missingFiles", "uploading", "pausedUP", "stoppedUP", "queuedUP", "stalledUP",
	"checkingUP", "forcedUP", "allocating", "downloading", "metaDL", "pausedDL", "stoppedDL",
	"queuedDL", "stalledDL", "checkingDL", "forcedDL", "checkingResumeData", "moving", "unknown",
}

var unusablePaths = map[string]struct{}{
	"checkingUP":         {},
	"checkingResumeData": {},
	"moving":             {},
	"allocating":         {},
}

type Download struct {
	ID          string
	Source      string
	Location    string
	Hash        string
	Name        string
	ContentPath string
	Size        int64
	Status      string
	Reason      string
	CreatedAt   time.Time
	CompletedAt *time.Time
	PublishedAt *time.Time
}

type Classification struct {
	Status string
	Reason string
}

func (c Classification) Terminal() bool {
	return c.Status != ""
}

func (c Classification) Completed() bool {
	return c.Status == statusCompleted
}

type Outcome struct {
	Status      string
	Reason      string
	Name        string
	ContentPath string
	Size        int64
	CompletedAt time.Time
}

// a deny list, never an allow list of finished states: a state qBittorrent adds later must
// still complete, because an event that never arrives is the worst failure this feature has
func Classify(s types.TorrentState, found bool) Classification {
	if !found {
		return Classification{Status: statusFailed, Reason: reasonMissing}
	}

	if s.State == "error" || s.State == "missingFiles" {
		return Classification{Status: statusFailed, Reason: "qbittorrent state: " + s.State}
	}

	if _, unusable := unusablePaths[s.State]; unusable {
		return Classification{}
	}

	if s.Progress >= 1 && s.CompletionOn > 0 {
		return Classification{Status: statusCompleted}
	}

	return Classification{}
}
