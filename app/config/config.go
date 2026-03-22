package config

import (
	"log/slog"

	"github.com/ilyakaznacheev/cleanenv"
	"github.com/joho/godotenv"
)

type SynologyConfig struct {
	URL         string `env:"SYNOLOGY_URL" env-default:"http://localhost:5000"`
	Username    string `env:"SYNOLOGY_USERNAME"`
	Password    string `env:"SYNOLOGY_PASSWORD"`
	Destination string `env:"SYNOLOGY_DESTINATION"`
}

type QBittorrentConfig struct {
	URL         string `env:"QBITTORRENT_URL"`
	Username    string `env:"QBITTORRENT_USERNAME"`
	Password    string `env:"QBITTORRENT_PASSWORD"`
	Destination string `env:"QBITTORRENT_DESTINATION"`
}

type TelegramConfig struct {
	Token      string  `env:"TELEGRAM_TOKEN"`
	SuperUsers []int64 `env:"TELEGRAM_SUPER_USERS" env-separator:","`
}

type HttpConfig struct {
	Port           int    `env:"HTTP_PORT" env-default:"8080"`
	BaseStaticPath string `env:"BASE_STATIC_PATH" env-default:"frontend/dist"`
}

type JackettConfig struct {
	URL string `env:"JACKETT_URL"`
}

type Config struct {
	Synology        SynologyConfig
	QBittorrent     QBittorrentConfig
	Telegram        TelegramConfig
	Http            HttpConfig
	Jackett         JackettConfig
	DownloadClient  string `env:"DOWNLOAD_CLIENT" env-default:"download_station"`
	DryMode         bool   `env:"DRY_MODE" env-default:"false"`
	Cron            string `env:"CRON" env-default:"0 * * * *"`
	OtelServiceName string `env:"OTEL_SERVICE_NAME" env-default:"magnet-feed-sync"`
	OtelEndpoint    string `env:"OTEL_EXPORTER_OTLP_ENDPOINT"`
	LokiURL         string `env:"LOKI_URL"`
}

func Init() (*Config, error) {
	err := godotenv.Load()
	if err != nil {
		slog.Warn("error while loading .env file", "error", err)
	}

	var cfg Config
	err = cleanenv.ReadEnv(&cfg)
	if err != nil {
		return nil, err
	}

	return &cfg, nil
}
