package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"
)

const (
	flushInterval   = time.Second
	maxBatchSize    = 100
	lokiPushPath    = "/loki/api/v1/push"
	lokiSendTimeout = 5 * time.Second
)

type lokiEntry struct {
	timestamp string
	line      string
	level     string
}

type lokiSender struct {
	serviceName string
	lokiURL     string
	client      *http.Client

	mu      sync.Mutex
	entries []lokiEntry

	done      chan struct{}
	stopCh    chan struct{}
	closeOnce sync.Once
}

func newLokiSender(serviceName, lokiURL string) *lokiSender {
	s := &lokiSender{
		serviceName: serviceName,
		lokiURL:     lokiURL,
		client:      &http.Client{Timeout: lokiSendTimeout},
		entries:     make([]lokiEntry, 0, maxBatchSize),
		done:        make(chan struct{}),
		stopCh:      make(chan struct{}),
	}
	go s.flushLoop()
	return s
}

func (s *lokiSender) flushLoop() {
	defer close(s.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.flush()
		case <-s.stopCh:
			s.flush()
			return
		}
	}
}

func (s *lokiSender) add(e lokiEntry) bool {
	s.mu.Lock()
	s.entries = append(s.entries, e)
	shouldFlush := len(s.entries) >= maxBatchSize
	s.mu.Unlock()
	return shouldFlush
}

func (s *lokiSender) flush() {
	s.mu.Lock()
	if len(s.entries) == 0 {
		s.mu.Unlock()
		return
	}
	batch := s.entries
	s.entries = make([]lokiEntry, 0, maxBatchSize)
	s.mu.Unlock()

	s.send(batch)
}

type lokiPushPayload struct {
	Streams []lokiStream `json:"streams"`
}

type lokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

func (s *lokiSender) send(entries []lokiEntry) {
	byLevel := make(map[string][][2]string)
	for _, e := range entries {
		byLevel[e.level] = append(byLevel[e.level], [2]string{e.timestamp, e.line})
	}

	streams := make([]lokiStream, 0, len(byLevel))
	for level, values := range byLevel {
		streams = append(streams, lokiStream{
			Stream: map[string]string{
				"service": s.serviceName,
				"level":   level,
			},
			Values: values,
		})
	}

	payload := lokiPushPayload{Streams: streams}
	body, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loki: marshal payload: %v (dropped %d entries)\n", err, len(entries))
		return
	}

	pushURL, err := url.JoinPath(s.lokiURL, lokiPushPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loki: join URL: %v (dropped %d entries)\n", err, len(entries))
		return
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, pushURL, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "loki: create request: %v (dropped %d entries)\n", err, len(entries))
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loki: send request: %v (dropped %d entries)\n", err, len(entries))
		return
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		fmt.Fprintf(os.Stderr, "loki: unexpected status %d: %s (dropped %d entries)\n", resp.StatusCode, respBody, len(entries))
	}
}

func (s *lokiSender) shutdown() {
	s.closeOnce.Do(func() { close(s.stopCh) })
	<-s.done
}

type LokiHandler struct {
	sender *lokiSender
	attrs  []slog.Attr
	groups []string
}

func NewLokiHandler(serviceName, lokiURL string) *LokiHandler {
	return &LokiHandler{
		sender: newLokiSender(serviceName, lokiURL),
	}
}

func (h *LokiHandler) Shutdown() {
	h.sender.shutdown()
}

func (h *LokiHandler) Enabled(_ context.Context, _ slog.Level) bool {
	return true
}

func (h *LokiHandler) Handle(ctx context.Context, r slog.Record) error {
	fields := make(map[string]any)

	for _, a := range h.attrs {
		fields[a.Key] = resolveValue(a.Value)
	}

	target := nestedMap(fields, h.groups)
	r.Attrs(func(a slog.Attr) bool {
		target[a.Key] = resolveValue(a.Value)
		return true
	})

	fields["msg"] = r.Message

	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		fields["trace_id"] = sc.TraceID().String()
	}

	ts := r.Time
	if ts.IsZero() {
		ts = time.Now()
	}

	line, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("marshal log entry: %w", err)
	}

	entry := lokiEntry{
		timestamp: strconv.FormatInt(ts.UnixNano(), 10),
		line:      string(line),
		level:     levelToString(r.Level),
	}

	if h.sender.add(entry) {
		h.sender.flush()
	}

	return nil
}

func (h *LokiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	copy(newAttrs[len(h.attrs):], attrs)
	return &LokiHandler{
		sender: h.sender,
		attrs:  newAttrs,
		groups: h.groups,
	}
}

func (h *LokiHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	newGroups := make([]string, len(h.groups)+1)
	copy(newGroups, h.groups)
	newGroups[len(h.groups)] = name
	return &LokiHandler{
		sender: h.sender,
		attrs:  h.attrs,
		groups: newGroups,
	}
}

type multiHandler struct {
	handlers []slog.Handler
}

func (h *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, handler := range h.handlers {
		if !handler.Enabled(ctx, r.Level) {
			continue
		}
		if err := handler.Handle(ctx, r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (h *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		handlers[i] = handler.WithAttrs(attrs)
	}
	return &multiHandler{handlers: handlers}
}

func (h *multiHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		handlers[i] = handler.WithGroup(name)
	}
	return &multiHandler{handlers: handlers}
}

func SetupLogging(serviceName, lokiURL string) (*slog.Logger, func()) {
	if lokiURL == "" {
		return slog.New(slog.NewTextHandler(os.Stdout, nil)), func() {}
	}

	lokiHandler := NewLokiHandler(serviceName, lokiURL)
	stdoutHandler := slog.NewJSONHandler(os.Stdout, nil)
	multi := &multiHandler{handlers: []slog.Handler{stdoutHandler, lokiHandler}}

	return slog.New(multi), lokiHandler.Shutdown
}

func resolveValue(v slog.Value) any {
	v = v.Resolve()
	if v.Kind() == slog.KindGroup {
		m := make(map[string]any)
		for _, a := range v.Group() {
			m[a.Key] = resolveValue(a.Value)
		}
		return m
	}
	return v.Any()
}

func nestedMap(m map[string]any, groups []string) map[string]any {
	for _, g := range groups {
		sub, ok := m[g].(map[string]any)
		if !ok {
			sub = make(map[string]any)
			m[g] = sub
		}
		m = sub
	}
	return m
}

func levelToString(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}
