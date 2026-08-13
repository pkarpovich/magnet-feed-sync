package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"magnet-feed-sync/app/notify"
)

// fakeStream captures what would have gone to JetStream, so payload, subject and message id
// are assertable without a broker.
type fakeStream struct {
	messages []natsMessage
	err      error
}

func (s *fakeStream) publish(_ context.Context, m natsMessage) error {
	s.messages = append(s.messages, m)

	return s.err
}

func testPublisher(stream jetStream) *Publisher {
	return &Publisher{
		stream: stream,
		now: func() time.Time {
			return time.Date(2026, time.August, 10, 12, 30, 0, 0, time.UTC)
		},
	}
}

func newResults(source string, titles ...string) []SearchResult {
	results := make([]SearchResult, 0, len(titles))
	for i, title := range titles {
		results = append(results, SearchResult{
			Source:     source,
			ExternalID: externalID(i),
			Title:      title,
		})
	}

	return results
}

func decodePayload(t *testing.T, body []byte) payload {
	t.Helper()

	var decoded payload
	require.NoError(t, json.Unmarshal(body, &decoded))

	return decoded
}

func TestPublisherPayloadFields(t *testing.T) {
	stream := &fakeStream{}
	p := testPublisher(stream)

	w := Watch{ID: "one-night-only-en", Rev: 3}
	o := RunOutcome{
		Raw:     newResults(sourceJackett, make([]string, 226)...),
		Matched: newResults(sourceJackett, enRelease),
		New:     []SearchResult{{Source: sourceJackett, ExternalID: "1883913", Title: enRelease}},
	}

	require.NoError(t, p.Publish(context.Background(), w, o))
	require.Len(t, stream.messages, 1)

	decoded := decodePayload(t, stream.messages[0].Payload)
	assert.Equal(t, "one-night-only-en", decoded.WatchID)
	assert.Equal(t, 3, decoded.WatchRev)
	assert.Equal(t, "2026-08-10T12:30:00Z", decoded.FoundAt)
	assert.Equal(t, 226, decoded.Total)
	assert.Equal(t, 1, decoded.Matched)
	assert.Equal(t, []payloadItem{{Source: sourceJackett, ID: "1883913", Title: enRelease}}, decoded.New)
}

func TestPublisherFoundAtIsRFC3339UTC(t *testing.T) {
	stream := &fakeStream{}
	p := testPublisher(stream)
	p.now = func() time.Time {
		return time.Date(2026, time.August, 10, 12, 30, 0, 0, time.FixedZone("MSK", 3*60*60))
	}

	o := RunOutcome{New: newResults(sourceJackett, enRelease)}
	require.NoError(t, p.Publish(context.Background(), Watch{ID: "w"}, o))

	decoded := decodePayload(t, stream.messages[0].Payload)
	assert.Equal(t, "2026-08-10T09:30:00Z", decoded.FoundAt)

	parsed, err := time.Parse(time.RFC3339, decoded.FoundAt)
	require.NoError(t, err)
	assert.Equal(t, time.UTC, parsed.Location())
}

func TestPublisherCapsItems(t *testing.T) {
	stream := &fakeStream{}
	p := testPublisher(stream)

	titles := make([]string, maxPayloadItems+5)
	for i := range titles {
		titles[i] = enRelease
	}
	o := RunOutcome{
		Raw:     newResults(sourceJackett, titles...),
		Matched: newResults(sourceJackett, titles...),
		New:     newResults(sourceJackett, titles...),
	}

	require.NoError(t, p.Publish(context.Background(), Watch{ID: "w"}, o))

	decoded := decodePayload(t, stream.messages[0].Payload)
	assert.Len(t, decoded.New, maxPayloadItems)
	assert.Equal(t, maxPayloadItems+5, decoded.Total)
	assert.Equal(t, maxPayloadItems+5, decoded.Matched)
	// the cycle marks every one of them seen, so without this count the 5 dropped releases
	// would be invisible to the consumer forever
	assert.Equal(t, maxPayloadItems+5, decoded.NewTotal)
}

func TestPublisherTrimsToByteCap(t *testing.T) {
	stream := &fakeStream{}
	p := testPublisher(stream)

	long := strings.Repeat("x", 2000)
	titles := make([]string, maxPayloadItems)
	for i := range titles {
		titles[i] = long
	}
	o := RunOutcome{
		Raw:     newResults(sourceJackett, titles...),
		Matched: newResults(sourceJackett, titles...),
		New:     newResults(sourceJackett, titles...),
	}

	require.NoError(t, p.Publish(context.Background(), Watch{ID: "w"}, o))

	body := stream.messages[0].Payload
	assert.LessOrEqual(t, len(body), maxPayloadBytes)

	decoded := decodePayload(t, body)
	assert.NotEmpty(t, decoded.New)
	assert.Less(t, len(decoded.New), maxPayloadItems)
	// the counts describe the whole run, so a trimmed list never reads as the whole story
	assert.Equal(t, maxPayloadItems, decoded.Total)
	assert.Equal(t, maxPayloadItems, decoded.Matched)
	assert.Equal(t, maxPayloadItems, decoded.NewTotal)
}

func TestPublisherSubjectAndMessageID(t *testing.T) {
	stream := &fakeStream{}
	p := testPublisher(stream)

	o := RunOutcome{New: []SearchResult{
		{Source: sourceJackett, ExternalID: "1883913", Title: ruRelease},
		{Source: SourceExtto, ExternalID: "20151803", Title: enRelease},
		{Source: sourceJackett, ExternalID: "42", Title: enRelease},
	}}

	require.NoError(t, p.Publish(context.Background(), Watch{ID: "one-night-only-en"}, o))

	assert.Equal(t, "tuclaw.releases.found.one-night-only-en", stream.messages[0].Subject)
	assert.Equal(t, "one-night-only-en:"+expectedMessageDigest(o.New), stream.messages[0].MsgID)
}

// expectedMessageDigest recomputes the id the way the payload contract states it — over the
// sorted set of seen keys — rather than by calling messageID, which would assert the
// implementation against itself.
func expectedMessageDigest(results []SearchResult) string {
	keys := make([]string, 0, len(results))
	for _, result := range results {
		keys = append(keys, result.SeenKey())
	}
	sort.Strings(keys)

	digest := sha256.Sum256([]byte(strings.Join(keys, "\n")))

	return hex.EncodeToString(digest[:])
}

// a retry of the publish-then-mark ordering that picked up an extra release must not collapse
// into the previous message id, however that release sorts: JetStream would ack it as a
// duplicate and the cycle would mark it seen without anyone ever being told.
func TestPublisherMessageIDCoversEveryNewItem(t *testing.T) {
	first := &fakeStream{}
	second := &fakeStream{}

	published := []SearchResult{{Source: sourceJackett, ExternalID: "42"}}
	// "1883913" sorts below "42", which is what a maximum-only key could not see
	retried := []SearchResult{published[0], {Source: sourceJackett, ExternalID: "1883913"}}

	w := Watch{ID: "w"}
	require.NoError(t, testPublisher(first).Publish(context.Background(), w, RunOutcome{New: published}))
	require.NoError(t, testPublisher(second).Publish(context.Background(), w, RunOutcome{New: retried}))

	assert.NotEqual(t, first.messages[0].MsgID, second.messages[0].MsgID)
}

func TestPublisherMessageIDIsOrderIndependent(t *testing.T) {
	first := &fakeStream{}
	second := &fakeStream{}

	results := []SearchResult{
		{Source: sourceJackett, ExternalID: "1883913"},
		{Source: SourceExtto, ExternalID: "20151803"},
	}
	reversed := []SearchResult{results[1], results[0]}

	w := Watch{ID: "w"}
	require.NoError(t, testPublisher(first).Publish(context.Background(), w, RunOutcome{New: results}))
	require.NoError(t, testPublisher(second).Publish(context.Background(), w, RunOutcome{New: reversed}))

	assert.Equal(t, first.messages[0].MsgID, second.messages[0].MsgID)
}

func TestPublisherReturnsStreamError(t *testing.T) {
	stream := &fakeStream{err: errors.New("no stream matches subject")}
	p := testPublisher(stream)

	err := p.Publish(context.Background(), Watch{ID: "w"}, RunOutcome{New: newResults(sourceJackett, enRelease)})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no stream matches subject")
	assert.Contains(t, err.Error(), "publish watch w")
}

type disabledTransport struct{}

func (disabledTransport) Publish(context.Context, notify.Message) error {
	return notify.ErrDisabled
}

// a disabled transport must surface as an error the engine can recognise, never as a quiet
// success: the cycle would mark releases seen that nobody was ever told about
func TestPublisherForwardsDisabledTransport(t *testing.T) {
	p := NewPublisher(PublisherOptions{Transport: disabledTransport{}})

	err := p.Publish(context.Background(), Watch{ID: "w"}, RunOutcome{New: newResults(sourceJackett, enRelease)})

	require.ErrorIs(t, err, notify.ErrDisabled)
}

func TestPublisherTransportReceivesMessage(t *testing.T) {
	transport := &recordingTransport{}
	p := NewPublisher(PublisherOptions{Transport: transport})
	p.now = func() time.Time { return time.Date(2026, time.August, 10, 12, 30, 0, 0, time.UTC) }

	o := RunOutcome{New: []SearchResult{{Source: sourceJackett, ExternalID: "1883913", Title: enRelease}}}
	require.NoError(t, p.Publish(context.Background(), Watch{ID: "one-night-only-en"}, o))

	require.Len(t, transport.messages, 1)
	assert.Equal(t, "tuclaw.releases.found.one-night-only-en", transport.messages[0].Subject)
	assert.Equal(t, "one-night-only-en:"+expectedMessageDigest(o.New), transport.messages[0].MsgID)
	assert.Equal(t, "one-night-only-en", decodePayload(t, transport.messages[0].Payload).WatchID)
}

type recordingTransport struct {
	messages []notify.Message
}

func (r *recordingTransport) Publish(_ context.Context, m notify.Message) error {
	r.messages = append(r.messages, m)

	return nil
}
