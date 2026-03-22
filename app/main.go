package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	downloadTasks "magnet-feed-sync/app/bot/download-tasks"
	"magnet-feed-sync/app/config"
	"magnet-feed-sync/app/database"
	downloadClient "magnet-feed-sync/app/download-client"
	"magnet-feed-sync/app/events"
	"magnet-feed-sync/app/http"
	"magnet-feed-sync/app/observability"
	"magnet-feed-sync/app/schedular"
	taskStore "magnet-feed-sync/app/task-store"
	"magnet-feed-sync/app/tracker"
	"magnet-feed-sync/app/tracker/providers"
)

func main() {
	cfg, err := config.Init()
	if err != nil {
		slog.Error("error reading config", "error", err)
		os.Exit(1)
	}

	logger, cleanupLog := observability.SetupLogging(cfg.OtelServiceName, cfg.LokiURL)
	defer cleanupLog()
	slog.SetDefault(logger)

	slog.Info("starting app")

	if cfg.DryMode {
		slog.Warn("dry mode is enabled")
	}

	if err := run(cfg); err != nil {
		slog.Error("error running app", "error", err)
		os.Exit(1)
	}
}

func run(cfg *config.Config) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	shutdownTracing, err := observability.SetupTracing(ctx, cfg.OtelServiceName, cfg.OtelEndpoint)
	if err != nil {
		return fmt.Errorf("failed to setup tracing: %w", err)
	}
	defer func() { _ = shutdownTracing(ctx) }()

	done := make(chan struct{})

	dClient, err := downloadClient.NewClient(*cfg)
	if err != nil {
		return fmt.Errorf("failed to create download client: %w", err)
	}

	providerList := []providers.Provider{
		&providers.RutrackerProvider{},
		&providers.NnmProvider{},
	}
	if cfg.Jackett.URL != "" {
		redacted := redactURL(cfg.Jackett.URL)
		slog.Info("jackett provider enabled", "url", redacted)
		providerList = append(providerList, providers.NewJackettProvider(cfg.Jackett.URL))
	}
	t := tracker.NewParser(dClient, providerList...)

	db, err := database.NewClient("tasks.db")
	if err != nil {
		return fmt.Errorf("failed to create database client: %w", err)
	}
	store, err := taskStore.NewRepository(db)
	if err != nil {
		return fmt.Errorf("failed to create task store: %w", err)
	}

	messagesForSend := make(chan string)

	downloadTasksClient := downloadTasks.NewClient(&downloadTasks.ClientCtx{
		Tracker:         t,
		DClient:         dClient,
		Store:           store,
		DryMode:         cfg.DryMode,
		MessagesForSend: messagesForSend,
	})

	s, err := schedular.NewService(cfg)
	if err != nil {
		return fmt.Errorf("failed to create scheduler: %w", err)
	}

	go func() {
		err = s.Start(downloadTasksClient.CheckForUpdates)
		if err != nil {
			slog.Error("error starting scheduler", "error", err)
			os.Exit(1)
		}
	}()

	tbAPI, err := tbapi.NewBotAPI(cfg.Telegram.Token)
	if err != nil {
		return fmt.Errorf("failed to create Telegram events: %w", err)
	}

	tgListener := &events.TelegramListener{
		SuperUsers:      cfg.Telegram.SuperUsers,
		TbAPI:           tbAPI,
		Bot:             downloadTasksClient,
		Store:           store,
		MessagesForSend: messagesForSend,
	}

	go tgListener.SendMessagesForAdmins(ctx)
	go http.NewClient(cfg.Http, store, downloadTasksClient, dClient).Start(ctx, done)

	go func() {
		if err := tgListener.Do(); err != nil {
			slog.Error("error in telegram listener", "error", err)
			panic(err)
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	cancel()

	select {
	case <-done:
		slog.Info("application shutdown completed")
	case <-time.After(15 * time.Second):
		slog.Info("application shutdown timed out")
	}

	return nil
}

func redactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<invalid url>"
	}
	q := u.Query()
	for key := range q {
		if strings.Contains(strings.ToLower(key), "apikey") || strings.Contains(strings.ToLower(key), "api_key") {
			q.Set(key, "***")
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}
