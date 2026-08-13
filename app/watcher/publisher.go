package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"magnet-feed-sync/app/notify"
)

const (
	subjectPrefix   = "tuclaw.releases.found."
	maxPayloadItems = 10
	maxPayloadBytes = 8192
)

type natsMessage struct {
	Subject string
	MsgID   string
	Payload []byte
}

type jetStream interface {
	publish(ctx context.Context, m natsMessage) error
}

type publisherTransport interface {
	Publish(ctx context.Context, m notify.Message) error
}

type PublisherOptions struct {
	Transport publisherTransport
}

type Publisher struct {
	stream jetStream
	now    func() time.Time
}

var _ publisher = (*Publisher)(nil)

func NewPublisher(o PublisherOptions) *Publisher {
	p := &Publisher{now: time.Now}

	if o.Transport != nil {
		p.stream = transportAdapter{transport: o.Transport}
	}

	return p
}

func (p *Publisher) Publish(ctx context.Context, w Watch, o RunOutcome) error {
	if p.stream == nil {
		return notify.ErrDisabled
	}

	body, err := p.payload(w, o)
	if err != nil {
		return err
	}

	msg := natsMessage{
		Subject: subjectPrefix + w.ID,
		MsgID:   messageID(w, o.New),
		Payload: body,
	}

	if err := p.stream.publish(ctx, msg); err != nil {
		return fmt.Errorf("publish watch %s: %w", w.ID, err)
	}

	return nil
}

type payload struct {
	WatchID  string        `json:"watch_id"`
	WatchRev int           `json:"watch_rev"`
	FoundAt  string        `json:"found_at"`
	Total    int           `json:"total"`
	Matched  int           `json:"matched"`
	NewTotal int           `json:"new_total"`
	New      []payloadItem `json:"new"`
}

type payloadItem struct {
	Source string `json:"source"`
	ID     string `json:"id"`
	Title  string `json:"title"`
}

// identity and delta only: search parameters here would drift from what the engine evaluated
func (p *Publisher) payload(w Watch, o RunOutcome) ([]byte, error) {
	items := make([]payloadItem, 0, len(o.New))
	for _, result := range o.New {
		items = append(items, payloadItem{
			Source: result.Source,
			ID:     result.ExternalID,
			Title:  result.Title,
		})
	}
	if len(items) > maxPayloadItems {
		items = items[:maxPayloadItems]
	}

	body := payload{
		WatchID:  w.ID,
		WatchRev: w.Rev,
		FoundAt:  p.now().UTC().Format(time.RFC3339),
		Total:    len(o.Raw),
		Matched:  len(o.Matched),
		NewTotal: len(o.New),
		New:      items,
	}

	for {
		marshalled, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal payload of watch %s: %w", w.ID, err)
		}

		if len(marshalled) <= maxPayloadBytes {
			return marshalled, nil
		}

		if len(body.New) == 0 {
			return nil, fmt.Errorf("payload of watch %s exceeds %d bytes", w.ID, maxPayloadBytes)
		}

		body.New = body.New[:len(body.New)-1]
	}
}

// digests the whole sorted key set, not its maximum: a retry that added a lower-sorting item
// would otherwise be acked as a duplicate and that item marked seen without ever being sent
func messageID(w Watch, results []SearchResult) string {
	keys := make([]string, 0, len(results))
	for _, result := range results {
		keys = append(keys, result.SeenKey())
	}
	sort.Strings(keys)

	digest := sha256.Sum256([]byte(strings.Join(keys, "\n")))

	return w.ID + ":" + hex.EncodeToString(digest[:])
}

type transportAdapter struct {
	transport publisherTransport
}

func (a transportAdapter) publish(ctx context.Context, m natsMessage) error {
	return a.transport.Publish(ctx, notify.Message{
		Subject: m.Subject,
		MsgID:   m.MsgID,
		Payload: m.Payload,
	})
}
