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
	assert.Empty(t, cfg.FlaresolverrURL)
	assert.Empty(t, cfg.NatsURL)
	assert.Equal(t, "0 * * * *", cfg.Cron)
	assert.Equal(t, defaultWatchCron, cfg.WatchCron)
	assert.Equal(t, "*/10 * * * *", cfg.DownloadCron)
}

func TestInit_DownloadCronFromEnv(t *testing.T) {
	t.Setenv("TELEGRAM_TOKEN", "test-token")
	t.Setenv("DOWNLOAD_CRON", "*/5 * * * *")

	cfg, err := Init()
	require.NoError(t, err)

	assert.Equal(t, "*/5 * * * *", cfg.DownloadCron)
}

func TestInit_WatchCronFromEnv(t *testing.T) {
	t.Setenv("TELEGRAM_TOKEN", "test-token")
	t.Setenv("WATCH_CRON", "*/30 * * * *")

	cfg, err := Init()
	require.NoError(t, err)

	assert.Equal(t, "*/30 * * * *", cfg.WatchCron)
}

func TestInit_NatsFromEnv(t *testing.T) {
	t.Setenv("TELEGRAM_TOKEN", "test-token")
	t.Setenv("NATS_URL", "nats://nats:4222")

	cfg, err := Init()
	require.NoError(t, err)

	assert.Equal(t, "nats://nats:4222", cfg.NatsURL)
}

func TestInit_FlaresolverrFromEnv(t *testing.T) {
	t.Setenv("TELEGRAM_TOKEN", "test-token")
	t.Setenv("FLARESOLVERR_URL", "https://flaresolverr.example.com/v1")

	cfg, err := Init()
	require.NoError(t, err)

	assert.Equal(t, "https://flaresolverr.example.com/v1", cfg.FlaresolverrURL)
}

func TestInit_QBittorrentFromEnv(t *testing.T) {
	t.Setenv("TELEGRAM_TOKEN", "test-token")
	t.Setenv("QBITTORRENT_URL", "http://qbittorrent:8080")
	t.Setenv("QBITTORRENT_USERNAME", "admin")
	t.Setenv("QBITTORRENT_PASSWORD", "secret")
	t.Setenv("QBITTORRENT_DESTINATION", "/downloads/movies")

	cfg, err := Init()
	require.NoError(t, err)

	assert.Equal(t, "http://qbittorrent:8080", cfg.QBittorrent.URL)
	assert.Equal(t, "admin", cfg.QBittorrent.Username)
	assert.Equal(t, "secret", cfg.QBittorrent.Password)
	assert.Equal(t, "/downloads/movies", cfg.QBittorrent.Destination)
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

func TestInit_JackettFromEnv(t *testing.T) {
	t.Setenv("TELEGRAM_TOKEN", "test-token")
	t.Setenv("JACKETT_URL", "http://jackett:9117")
	t.Setenv("JACKETT_API_KEY", "secret-key")
	t.Setenv("JACKETT_PUBLIC_URL", "https://jackett.example.com")

	cfg, err := Init()
	require.NoError(t, err)

	assert.Equal(t, "http://jackett:9117", cfg.Jackett.URL)
	assert.Equal(t, "secret-key", cfg.Jackett.APIKey)
	assert.Equal(t, "https://jackett.example.com", cfg.Jackett.PublicURL)
}

func TestInit_JackettPublicURLDefaultsToURL(t *testing.T) {
	t.Setenv("TELEGRAM_TOKEN", "test-token")
	t.Setenv("JACKETT_URL", "http://jackett:9117")

	cfg, err := Init()
	require.NoError(t, err)

	assert.Equal(t, "http://jackett:9117", cfg.Jackett.PublicURL)
	assert.Empty(t, cfg.Jackett.APIKey)
}
