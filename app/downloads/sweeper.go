package downloads

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"magnet-feed-sync/app/notify"
	"magnet-feed-sync/app/types"
)

type store interface {
	Pending() ([]*Download, error)
	MarkPublished(id string, o Outcome) error
}

type torrentLookup interface {
	TorrentStates(ctx context.Context, hashes []string) (map[string]types.TorrentState, error)
}

type publisher interface {
	Publish(ctx context.Context, m notify.Message) error
}

type SweeperDeps struct {
	Store    store
	Torrents torrentLookup
	Notifier publisher
}

type Sweeper struct {
	store    store
	torrents torrentLookup
	notifier publisher
	now      func() time.Time
}

func NewSweeper(d SweeperDeps) *Sweeper {
	return &Sweeper{
		store:    d.Store,
		torrents: d.Torrents,
		notifier: d.Notifier,
		now:      time.Now,
	}
}

func (s *Sweeper) RunCycle(ctx context.Context) error {
	pending, err := s.store.Pending()
	if err != nil {
		return fmt.Errorf("load pending downloads: %w", err)
	}

	// a shutdown is not a cycle failure: the lookup would report the cancelled context as one
	if len(pending) == 0 || ctx.Err() != nil {
		return nil
	}

	// a failed lookup aborts the cycle: an empty map reads as "every torrent was deleted by
	// hand", which would publish a false failure for every row and lose every real completion
	states, err := s.torrents.TorrentStates(ctx, s.hashes(pending))
	if err != nil {
		slog.ErrorContext(ctx, "failed to look up torrent states", "error", err)

		return fmt.Errorf("look up torrent states: %w", err)
	}

	for _, d := range pending {
		if ctx.Err() != nil {
			return nil
		}

		state, found := states[d.Hash]

		class := Classify(state, found)
		if !class.Terminal() {
			continue
		}

		s.report(ctx, d, state, class)
	}

	return nil
}

func (s *Sweeper) hashes(pending []*Download) []string {
	list := make([]string, 0, len(pending))
	for _, d := range pending {
		list = append(list, d.Hash)
	}

	return list
}

// publish first, mark second: a crash between the two costs one duplicate wake, the reverse
// loses the event permanently and silently
func (s *Sweeper) report(ctx context.Context, d *Download, state types.TorrentState, class Classification) {
	outcome := s.outcome(d, state, class)

	body, err := s.payload(d, outcome)
	if err != nil {
		slog.ErrorContext(ctx, "failed to build download payload", "download_id", d.ID, "error", err)

		return
	}

	msg := notify.Message{
		Subject: Subject(d.ID),
		MsgID:   d.ID + ":" + outcome.Status,
		Payload: body,
	}

	if err := s.notifier.Publish(ctx, msg); err != nil {
		slog.ErrorContext(ctx, "failed to publish download event", "download_id", d.ID, "error", err)

		return
	}

	if err := s.store.MarkPublished(d.ID, outcome); err != nil {
		slog.ErrorContext(ctx, "failed to mark download published", "download_id", d.ID, "error", err)
	}
}

func (s *Sweeper) outcome(d *Download, state types.TorrentState, class Classification) Outcome {
	if !class.Completed() {
		return Outcome{
			Status:      class.Status,
			Reason:      class.Reason,
			Name:        d.Name,
			ContentPath: d.ContentPath,
			Size:        d.Size,
			CompletedAt: s.now().UTC(),
		}
	}

	return Outcome{
		Status:      class.Status,
		Name:        state.Name,
		ContentPath: state.ContentPath,
		Size:        state.Size,
		CompletedAt: time.Unix(state.CompletionOn, 0).UTC(),
	}
}

type completedPayload struct {
	DownloadID  string `json:"download_id"`
	Status      string `json:"status"`
	Hash        string `json:"hash"`
	Name        string `json:"name"`
	ContentPath string `json:"content_path"`
	Size        int64  `json:"size"`
	Location    string `json:"location"`
	CompletedAt string `json:"completed_at"`
}

type failedPayload struct {
	DownloadID  string `json:"download_id"`
	Status      string `json:"status"`
	Reason      string `json:"reason"`
	Hash        string `json:"hash"`
	Name        string `json:"name"`
	CompletedAt string `json:"completed_at"`
}

func (s *Sweeper) payload(d *Download, o Outcome) ([]byte, error) {
	completedAt := o.CompletedAt.UTC().Format(time.RFC3339)

	var body any = failedPayload{
		DownloadID:  d.ID,
		Status:      o.Status,
		Reason:      o.Reason,
		Hash:        d.Hash,
		Name:        o.Name,
		CompletedAt: completedAt,
	}

	if o.Status == statusCompleted {
		body = completedPayload{
			DownloadID:  d.ID,
			Status:      o.Status,
			Hash:        d.Hash,
			Name:        o.Name,
			ContentPath: o.ContentPath,
			Size:        o.Size,
			Location:    d.Location,
			CompletedAt: completedAt,
		}
	}

	marshalled, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal payload of download %s: %w", d.ID, err)
	}

	return marshalled, nil
}
