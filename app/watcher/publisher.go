package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// subjectPrefix keeps the literal tuclaw first token the TUCLAW stream subscribes to.
	subjectPrefix      = "tuclaw.releases.found."
	maxPayloadItems    = 10
	maxPayloadBytes    = 8192
	natsPublishTimeout = 10 * time.Second
	natsDialTimeout    = 5 * time.Second
)

var errPublisherDisabled = errors.New("publish: nats is not configured")

// natsMessage is what the JetStream seam carries. The two strings are on a struct rather
// than in the signature so a subject can never be passed as a message id — and so the
// message id is assertable at all: jetstream.WithMsgID returns an opaque PublishOpt over an
// unexported struct, which a fake standing directly in for jetstream.JetStream could not
// read back.
type natsMessage struct {
	Subject string
	MsgID   string
	Payload []byte
}

// jetStream is the consumer-side view of JetStream. The concrete client is wrapped by
// jetStreamAdapter, so payload construction can be tested without a broker.
type jetStream interface {
	publish(ctx context.Context, m natsMessage) error
}

// PublisherOptions configures the publisher. An empty URL disables publishing entirely.
type PublisherOptions struct {
	URL string
}

// Publisher announces a watch's new releases on the TUCLAW JetStream stream. The stream is
// owned by the tuclaw daemon: this is a publisher only and never creates or reconfigures it.
type Publisher struct {
	stream jetStream
	conn   *nats.Conn
	now    func() time.Time
}

var _ publisher = (*Publisher)(nil)

// NewPublisher connects to NATS. A failed connect is never fatal: the container would
// otherwise crash-loop every time NATS restarts. A publisher without a stream returns an
// error from Publish, which keeps the release unmarked and retried next cycle.
func NewPublisher(o PublisherOptions) *Publisher {
	p := &Publisher{now: time.Now}

	if o.URL == "" {
		slog.Warn("NATS_URL is empty, watch notifications are disabled")

		return p
	}

	conn, err := nats.Connect(o.URL,
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.Timeout(natsDialTimeout),
	)
	if err != nil {
		slog.Error("failed to connect to nats, watch notifications are disabled", "error", err)

		return p
	}
	p.conn = conn

	js, err := jetstream.New(conn)
	if err != nil {
		slog.Error("failed to create jetstream context, watch notifications are disabled", "error", err)

		return p
	}
	p.stream = jetStreamAdapter{js: js}

	return p
}

// Close releases the NATS connection.
func (p *Publisher) Close() {
	if p.conn != nil {
		p.conn.Close()
	}
}

// Publish sends the delta of one watch and returns only once JetStream has acked it, so
// nothing is marked seen before the wake-up is durable.
func (p *Publisher) Publish(ctx context.Context, w Watch, o RunOutcome) error {
	if p.stream == nil {
		return errPublisherDisabled
	}

	body, err := p.payload(w, o)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, natsPublishTimeout)
	defer cancel()

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

// payload carries identity and delta only — never the search parameters, which would drift
// from what the engine actually evaluated. total, matched and new_total survive every
// truncation, so a trimmed new list is never mistaken for the whole story: the cycle marks
// all of New seen, and without new_total a consumer could not tell that the items beyond
// the cap existed at all.
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

// messageID is the stream-side dedup key for the deliberate publish-then-mark ordering. It
// digests the *whole* sorted key set, so it is order-independent and always present — unlike
// "the newest item", since PublishedAt is the zero time for any ext.to row whose Age cell
// carries no title attribute.
//
// The set matters, not just its maximum: when a publish is acked but MarkSeen fails, the
// retry re-publishes the same releases plus whatever the cycle found since. A key derived
// from the maximum alone is unchanged by an added item that sorts lower, so JetStream would
// ack the retry as a duplicate while the cycle marks the whole set seen — losing that item
// permanently and silently, which is exactly what publish-then-mark exists to prevent.
func messageID(w Watch, results []SearchResult) string {
	keys := make([]string, 0, len(results))
	for _, result := range results {
		keys = append(keys, result.SeenKey())
	}
	sort.Strings(keys)

	digest := sha256.Sum256([]byte(strings.Join(keys, "\n")))

	return w.ID + ":" + hex.EncodeToString(digest[:])
}

type jetStreamAdapter struct {
	js jetstream.JetStream
}

func (a jetStreamAdapter) publish(ctx context.Context, m natsMessage) error {
	if _, err := a.js.Publish(ctx, m.Subject, m.Payload, jetstream.WithMsgID(m.MsgID)); err != nil {
		return err
	}

	return nil
}
