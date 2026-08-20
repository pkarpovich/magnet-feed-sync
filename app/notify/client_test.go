package notify

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeStream struct {
	messages []Message
	err      error
}

func (s *fakeStream) publish(_ context.Context, m Message) error {
	s.messages = append(s.messages, m)

	return s.err
}

func TestClientPublishForwardsMessage(t *testing.T) {
	stream := &fakeStream{}
	c := &Client{stream: stream}

	msg := Message{
		Subject: "tuclaw.downloads.completed.0123456789abcdef",
		MsgID:   "0123456789abcdef:completed",
		Payload: []byte(`{"status":"completed"}`),
	}

	require.NoError(t, c.Publish(context.Background(), msg))
	require.Len(t, stream.messages, 1)
	assert.Equal(t, msg, stream.messages[0])
}

func TestClientPublishReturnsStreamError(t *testing.T) {
	stream := &fakeStream{err: errors.New("no stream matches subject")}
	c := &Client{stream: stream}

	err := c.Publish(context.Background(), Message{Subject: "tuclaw.releases.found.w"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no stream matches subject")
}

func TestClientEnabledReflectsBothStates(t *testing.T) {
	assert.False(t, NewClient(Options{}).Enabled())
	assert.True(t, (&Client{stream: &fakeStream{}}).Enabled())
}

func TestClientDisabledWithoutURL(t *testing.T) {
	c := NewClient(Options{})
	defer c.Close()

	require.ErrorIs(t, c.Publish(context.Background(), Message{Subject: "tuclaw.releases.found.w"}), ErrDisabled)
}

func TestNewClientSurvivesUnreachableNats(t *testing.T) {
	// an unreachable broker must not be fatal: the container would crash-loop on every
	// NATS restart, and every consumer already treats a publish error as "retry next cycle"
	c := NewClient(Options{URL: "nats://127.0.0.1:1"})
	defer c.Close()

	require.NotNil(t, c)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	require.Error(t, c.Publish(ctx, Message{Subject: "tuclaw.releases.found.w"}))
}

func TestNewClientSurvivesInvalidURL(t *testing.T) {
	c := NewClient(Options{URL: "://not-a-url"})
	defer c.Close()

	require.NotNil(t, c)
	require.Error(t, c.Publish(context.Background(), Message{Subject: "tuclaw.releases.found.w"}))
}
