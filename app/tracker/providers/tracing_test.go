package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func setupTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	orig := otel.GetTracerProvider()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(orig)
	})
	return exporter
}

func TestJackettProvider_Parse_CreatesTracingSpan(t *testing.T) {
	exporter := setupTestTracer(t)

	xmlResponse := `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <item>
      <title>Test Torrent</title>
      <link>magnet:?xt=urn:btih:abc123&amp;dn=test</link>
      <guid>https://tracker.example.com/topic/123</guid>
      <pubDate>Mon, 20 Mar 2026 10:00:00 +0000</pubDate>
    </item>
  </channel>
</rss>`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		fmt.Fprint(w, xmlResponse)
	}))
	defer server.Close()

	provider := NewJackettProvider(server.URL)
	result, err := provider.Parse(context.Background(), server.URL+"/api/v2.0/indexers/test/results?q=test")
	require.NoError(t, err)
	assert.Equal(t, "Test Torrent", result.Title)

	spans := exporter.GetSpans()
	require.GreaterOrEqual(t, len(spans), 1)

	spanNames := make([]string, len(spans))
	for i, s := range spans {
		spanNames[i] = s.Name
	}
	assert.Contains(t, spanNames, "JackettProvider.Parse")
}

func TestRutrackerProvider_Parse_CreatesTracingSpan(t *testing.T) {
	exporter := setupTestTracer(t)

	htmlResponse := `<html><body>
		<a class="magnet-link" href="magnet:?xt=urn:btih:abc123&dn=test">magnet</a>
		<a id="topic-title">Test Rutracker Torrent</a>
	</body></html>`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, htmlResponse)
	}))
	defer server.Close()

	provider := &RutrackerProvider{}
	result, err := provider.Parse(context.Background(), server.URL+"?t=123")
	require.NoError(t, err)
	assert.Equal(t, "Test Rutracker Torrent", result.Title)

	spans := exporter.GetSpans()
	require.GreaterOrEqual(t, len(spans), 1)

	spanNames := make([]string, len(spans))
	for i, s := range spans {
		spanNames[i] = s.Name
	}
	assert.Contains(t, spanNames, "RutrackerProvider.Parse")
}

func TestNnmProvider_Parse_CreatesTracingSpan(t *testing.T) {
	exporter := setupTestTracer(t)

	htmlResponse := `<html><body>
		<a href="magnet:?xt=urn:btih:abc123&dn=test">magnet</a>
		<a class="maintitle">Test NNM Torrent</a>
	</body></html>`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, htmlResponse)
	}))
	defer server.Close()

	provider := &NnmProvider{}
	result, err := provider.Parse(context.Background(), server.URL+"?t=456")
	require.NoError(t, err)
	assert.Equal(t, "Test NNM Torrent", result.Title)

	spans := exporter.GetSpans()
	require.GreaterOrEqual(t, len(spans), 1)

	spanNames := make([]string, len(spans))
	for i, s := range spans {
		spanNames[i] = s.Name
	}
	assert.Contains(t, spanNames, "NnmProvider.Parse")
}

func TestProviderParse_NoopTracingNoCrash(t *testing.T) {
	otel.SetTracerProvider(otel.GetTracerProvider())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><body><a class="magnet-link" href="magnet:?xt=urn:btih:abc">m</a></body></html>`)
	}))
	defer server.Close()

	provider := &RutrackerProvider{}
	_, err := provider.Parse(context.Background(), server.URL+"?t=1")
	require.NoError(t, err)
}
