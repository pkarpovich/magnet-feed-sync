package notify

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	publishTimeout = 10 * time.Second
	dialTimeout    = 5 * time.Second
)

var ErrDisabled = errors.New("publish: nats is not configured")

type Message struct {
	Subject string
	MsgID   string
	Payload []byte
}

type Options struct {
	URL string
}

type stream interface {
	publish(ctx context.Context, m Message) error
}

type Client struct {
	stream stream
	conn   *nats.Conn
}

// a failed connect is never fatal: the container would crash-loop on every NATS restart
func NewClient(o Options) *Client {
	c := &Client{}

	if o.URL == "" {
		slog.Warn("NATS_URL is empty, notifications are disabled")

		return c
	}

	conn, err := nats.Connect(o.URL,
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.Timeout(dialTimeout),
	)
	if err != nil {
		slog.Error("failed to connect to nats, notifications are disabled", "error", err)

		return c
	}
	c.conn = conn

	js, err := jetstream.New(conn)
	if err != nil {
		slog.Error("failed to create jetstream context, notifications are disabled", "error", err)

		return c
	}
	c.stream = jetStreamAdapter{js: js}

	return c
}

func (c *Client) Enabled() bool {
	return c.stream != nil
}

func (c *Client) Publish(ctx context.Context, m Message) error {
	if c.stream == nil {
		return ErrDisabled
	}

	ctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()

	return c.stream.publish(ctx, m)
}

func (c *Client) Close() {
	if c.conn != nil {
		c.conn.Close()
	}
}

type jetStreamAdapter struct {
	js jetstream.JetStream
}

func (a jetStreamAdapter) publish(ctx context.Context, m Message) error {
	if _, err := a.js.Publish(ctx, m.Subject, m.Payload, jetstream.WithMsgID(m.MsgID)); err != nil {
		return err
	}

	return nil
}
