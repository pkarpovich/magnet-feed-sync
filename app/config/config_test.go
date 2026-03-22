package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInit_DefaultValues(t *testing.T) {
	t.Setenv("TELEGRAM_TOKEN", "test-token")

	cfg, err := Init()
	require.NoError(t, err)

	assert.Equal(t, "magnet-feed-sync", cfg.OtelServiceName)
	assert.Empty(t, cfg.OtelEndpoint)
	assert.Empty(t, cfg.LokiURL)
}

func TestInit_CustomObservabilityValues(t *testing.T) {
	t.Setenv("TELEGRAM_TOKEN", "test-token")
	t.Setenv("OTEL_SERVICE_NAME", "custom-service")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel:4318")
	t.Setenv("LOKI_URL", "http://loki:3100")

	cfg, err := Init()
	require.NoError(t, err)

	assert.Equal(t, "custom-service", cfg.OtelServiceName)
	assert.Equal(t, "http://otel:4318", cfg.OtelEndpoint)
	assert.Equal(t, "http://loki:3100", cfg.LokiURL)
}
