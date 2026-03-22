package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
)

func newTestLokiServer(t *testing.T) (*httptest.Server, *[]lokiPushPayload, *sync.Mutex) {
	t.Helper()
	var payloads []lokiPushPayload
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p lokiPushPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		payloads = append(payloads, p)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	return server, &payloads, &mu
}

func countValues(p lokiPushPayload) int {
	n := 0
	for _, s := range p.Streams {
		n += len(s.Values)
	}
	return n
}

func TestLokiHandler_Format(t *testing.T) {
	server, payloads, mu := newTestLokiServer(t)

	handler := NewLokiHandler("test-service", server.URL)
	logger := slog.New(handler)

	logger.Info("test message", "key", "value")
	handler.Shutdown()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, *payloads, 1)

	p := (*payloads)[0]
	require.Len(t, p.Streams, 1)
	assert.Equal(t, "test-service", p.Streams[0].Stream["service"])
	assert.Equal(t, "info", p.Streams[0].Stream["level"])

	require.Len(t, p.Streams[0].Values, 1)
	assert.NotEmpty(t, p.Streams[0].Values[0][0])

	var logLine map[string]any
	require.NoError(t, json.Unmarshal([]byte(p.Streams[0].Values[0][1]), &logLine))
	assert.Equal(t, "test message", logLine["msg"])
	assert.Equal(t, "value", logLine["key"])
}

func TestLokiHandler_TraceID(t *testing.T) {
	server, payloads, mu := newTestLokiServer(t)

	handler := NewLokiHandler("test-service", server.URL)
	logger := slog.New(handler)

	traceID, err := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	require.NoError(t, err)
	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), spanCtx)

	logger.InfoContext(ctx, "traced message")
	handler.Shutdown()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, *payloads, 1)

	var logLine map[string]any
	require.NoError(t, json.Unmarshal([]byte((*payloads)[0].Streams[0].Values[0][1]), &logLine))
	assert.Equal(t, "0123456789abcdef0123456789abcdef", logLine["trace_id"])
}

func TestLokiHandler_NoTraceID(t *testing.T) {
	server, payloads, mu := newTestLokiServer(t)

	handler := NewLokiHandler("test-service", server.URL)
	logger := slog.New(handler)

	logger.Info("no trace")
	handler.Shutdown()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, *payloads, 1)

	var logLine map[string]any
	require.NoError(t, json.Unmarshal([]byte((*payloads)[0].Streams[0].Values[0][1]), &logLine))
	assert.Nil(t, logLine["trace_id"])
}

func TestLokiHandler_LevelLabels(t *testing.T) {
	server, payloads, mu := newTestLokiServer(t)

	handler := NewLokiHandler("test-service", server.URL)
	logger := slog.New(handler)

	logger.Info("info msg")
	logger.Warn("warn msg")
	logger.Error("error msg")
	handler.Shutdown()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, *payloads, 1)

	levels := make(map[string]bool)
	for _, s := range (*payloads)[0].Streams {
		levels[s.Stream["level"]] = true
	}
	assert.True(t, levels["info"])
	assert.True(t, levels["warn"])
	assert.True(t, levels["error"])
}

func TestLokiHandler_BatchFlushOnFull(t *testing.T) {
	server, payloads, mu := newTestLokiServer(t)

	handler := NewLokiHandler("test-service", server.URL)
	logger := slog.New(handler)

	for i := range 150 {
		logger.Info("message", "i", i)
	}
	handler.Shutdown()

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(*payloads), 2)

	assert.Equal(t, maxBatchSize, countValues((*payloads)[0]))

	total := 0
	for _, p := range *payloads {
		total += countValues(p)
	}
	assert.Equal(t, 150, total)
}

func TestLokiHandler_FlushOnShutdown(t *testing.T) {
	server, payloads, mu := newTestLokiServer(t)

	handler := NewLokiHandler("test-service", server.URL)
	logger := slog.New(handler)

	for i := range 5 {
		logger.Info("message", "i", i)
	}
	handler.Shutdown()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, *payloads, 1)
	assert.Equal(t, 5, countValues((*payloads)[0]))
}

func TestLokiHandler_WithAttrs(t *testing.T) {
	server, payloads, mu := newTestLokiServer(t)

	handler := NewLokiHandler("test-service", server.URL)
	withAttrs := handler.WithAttrs([]slog.Attr{
		slog.String("component", "auth"),
		slog.Int("version", 2),
	})
	logger := slog.New(withAttrs)

	logger.Info("attrs message", "key", "value")
	handler.Shutdown()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, *payloads, 1)

	var logLine map[string]any
	require.NoError(t, json.Unmarshal([]byte((*payloads)[0].Streams[0].Values[0][1]), &logLine))
	assert.Equal(t, "attrs message", logLine["msg"])
	assert.Equal(t, "auth", logLine["component"])
	assert.Equal(t, float64(2), logLine["version"])
	assert.Equal(t, "value", logLine["key"])
}

func TestLokiHandler_WithGroup(t *testing.T) {
	server, payloads, mu := newTestLokiServer(t)

	handler := NewLokiHandler("test-service", server.URL)
	withGroup := handler.WithGroup("request")
	logger := slog.New(withGroup)

	logger.Info("grouped message", "method", "GET", "path", "/api")
	handler.Shutdown()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, *payloads, 1)

	var logLine map[string]any
	require.NoError(t, json.Unmarshal([]byte((*payloads)[0].Streams[0].Values[0][1]), &logLine))
	assert.Equal(t, "grouped message", logLine["msg"])
	reqGroup, ok := logLine["request"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "GET", reqGroup["method"])
	assert.Equal(t, "/api", reqGroup["path"])
}

func TestLokiHandler_WithGroupEmpty(t *testing.T) {
	handler := NewLokiHandler("test-service", "http://localhost:3100")
	same := handler.WithGroup("")
	assert.Equal(t, handler, same)
	handler.Shutdown()
}

func TestLokiHandler_DebugLevel(t *testing.T) {
	server, payloads, mu := newTestLokiServer(t)

	handler := NewLokiHandler("test-service", server.URL)
	logger := slog.New(handler)

	logger.Debug("debug msg")
	handler.Shutdown()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, *payloads, 1)
	assert.Equal(t, "debug", (*payloads)[0].Streams[0].Stream["level"])
}

func TestMultiHandler_Enabled(t *testing.T) {
	handler := &multiHandler{handlers: []slog.Handler{
		slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn}),
	}}

	assert.False(t, handler.Enabled(context.Background(), slog.LevelInfo))
	assert.True(t, handler.Enabled(context.Background(), slog.LevelWarn))
	assert.True(t, handler.Enabled(context.Background(), slog.LevelError))
}

func TestMultiHandler_WithAttrs(t *testing.T) {
	h1 := slog.NewTextHandler(io.Discard, nil)
	h2 := slog.NewTextHandler(io.Discard, nil)
	multi := &multiHandler{handlers: []slog.Handler{h1, h2}}

	result := multi.WithAttrs([]slog.Attr{slog.String("key", "value")})
	resultMulti, ok := result.(*multiHandler)
	require.True(t, ok)
	assert.Len(t, resultMulti.handlers, 2)
}

func TestMultiHandler_WithGroup(t *testing.T) {
	h1 := slog.NewTextHandler(io.Discard, nil)
	h2 := slog.NewTextHandler(io.Discard, nil)
	multi := &multiHandler{handlers: []slog.Handler{h1, h2}}

	result := multi.WithGroup("test")
	resultMulti, ok := result.(*multiHandler)
	require.True(t, ok)
	assert.Len(t, resultMulti.handlers, 2)
}

func TestMultiHandler_Handle(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	h1 := slog.NewTextHandler(&buf1, nil)
	h2 := slog.NewTextHandler(&buf2, nil)
	multi := &multiHandler{handlers: []slog.Handler{h1, h2}}

	logger := slog.New(multi)
	logger.Info("test message")

	assert.Contains(t, buf1.String(), "test message")
	assert.Contains(t, buf2.String(), "test message")
}

func TestResolveValue_Group(t *testing.T) {
	val := slog.GroupValue(
		slog.String("nested_key", "nested_val"),
		slog.Int("nested_int", 42),
	)

	result := resolveValue(val)
	m, ok := result.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "nested_val", m["nested_key"])
	assert.Equal(t, int64(42), m["nested_int"])
}

func TestNestedMap_MultipleGroups(t *testing.T) {
	root := make(map[string]any)
	leaf := nestedMap(root, []string{"a", "b"})
	leaf["key"] = "value"

	aMap, ok := root["a"].(map[string]any)
	require.True(t, ok)
	bMap, ok := aMap["b"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "value", bMap["key"])
}

func TestNestedMap_Empty(t *testing.T) {
	root := make(map[string]any)
	result := nestedMap(root, nil)
	assert.Equal(t, root, result)
}

func TestSetupLogging_WithoutLokiURL(t *testing.T) {
	logger, cleanup := SetupLogging("test-service", "")
	defer cleanup()

	_, ok := logger.Handler().(*slog.TextHandler)
	assert.True(t, ok)
}

func TestSetupLogging_WithLokiURL(t *testing.T) {
	server, _, _ := newTestLokiServer(t)

	logger, cleanup := SetupLogging("test-service", server.URL)
	defer cleanup()

	_, ok := logger.Handler().(*multiHandler)
	assert.True(t, ok)
}
