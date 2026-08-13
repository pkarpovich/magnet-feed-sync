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
	"github.com/robfig/cron/v3"
	downloadTasks "magnet-feed-sync/app/bot/download-tasks"
	"magnet-feed-sync/app/config"
	"magnet-feed-sync/app/database"
	"magnet-feed-sync/app/download-client/qbittorrent"
	downloadStore "magnet-feed-sync/app/download-store"
	"magnet-feed-sync/app/events"
	"magnet-feed-sync/app/http"
	"magnet-feed-sync/app/notify"
	"magnet-feed-sync/app/observability"
	"magnet-feed-sync/app/schedular"
	taskStore "magnet-feed-sync/app/task-store"
	"magnet-feed-sync/app/tracker"
	"magnet-feed-sync/app/tracker/providers"
	watchStore "magnet-feed-sync/app/watch-store"
	"magnet-feed-sync/app/watcher"
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
		cleanupLog()
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
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = shutdownTracing(shutdownCtx)
	}()

	done := make(chan struct{})

	dClient := qbittorrent.NewClient(cfg.QBittorrent)

	directFetcher := providers.NewDirectFetcher()

	var pageSolver watchSolver

	rutrackerFetcher := providers.NewBlockedFetcher()
	if cfg.FlaresolverrURL != "" {
		solver := providers.NewSolverFetcher(cfg.FlaresolverrURL)
		rutrackerFetcher = solver
		pageSolver = solver
		slog.Info("rutracker provider uses flaresolverr", "url", redactURL(cfg.FlaresolverrURL))

		// run() cancels ctx before deferred functions run, so the session teardown
		// needs a context that survives it
		defer func() {
			closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer closeCancel()
			if err := solver.Close(closeCtx); err != nil {
				slog.Error("error closing flaresolverr session", "error", err)
			}
		}()
	} else {
		slog.Warn("flaresolverr url is not configured, rutracker pages will be reported as blocked")
	}

	providerList := []providers.Provider{
		providers.NewRutrackerProvider(rutrackerFetcher),
		providers.NewNnmProvider(directFetcher),
	}
	if cfg.Jackett.URL != "" {
		redacted := redactURL(cfg.Jackett.URL)
		slog.Info("jackett provider enabled", "url", redacted)
		providerList = append(providerList, providers.NewJackettProvider(cfg.Jackett.URL, directFetcher))
	}
	t := tracker.NewParser(dClient, providerList...)

	breaker := newProviderBreaker(providerList)

	db, err := database.NewClient("tasks.db")
	if err != nil {
		return fmt.Errorf("failed to create database client: %w", err)
	}
	store, err := taskStore.NewRepository(db)
	if err != nil {
		return fmt.Errorf("failed to create task store: %w", err)
	}
	watchRepo, err := watchStore.NewRepository(db)
	if err != nil {
		return fmt.Errorf("failed to create watch store: %w", err)
	}
	downloadRepo, err := downloadStore.NewRepository(db)
	if err != nil {
		return fmt.Errorf("failed to create download store: %w", err)
	}

	messagesForSend := make(chan string)

	sources := watcherSources(cfg, pageSolver)

	notifier := notify.NewClient(notify.Options{URL: cfg.NatsURL})
	defer notifier.Close()

	publisher := watcher.NewPublisher(watcher.PublisherOptions{Transport: notifier})

	engine := watcher.NewEngine(watcher.EngineDeps{
		Sources:   sources.list,
		Store:     watchRepo,
		Publisher: publisher,
		Messages:  messagesForSend,
	})

	downloadTasksClient := downloadTasks.NewClient(&downloadTasks.ClientCtx{
		Tracker:         t,
		DClient:         dClient,
		Store:           store,
		Breaker:         breaker,
		Notifier:        notifier,
		DryMode:         cfg.DryMode,
		MessagesForSend: messagesForSend,
	})

	s, err := schedular.NewService()
	if err != nil {
		return fmt.Errorf("failed to create scheduler: %w", err)
	}

	// AddJob returns its error and Start is non-blocking, so registration stays on the
	// startup path: a bad cron expression is a boot failure, not a background surprise
	if err := s.AddJob("files", cfg.Cron, func() { downloadTasksClient.CheckForUpdates(ctx) }); err != nil {
		return fmt.Errorf("scheduler failed: %w", err)
	}
	if err := s.AddJob("watcher", cfg.WatchCron, func() { runWatchCycle(ctx, engine) }); err != nil {
		return fmt.Errorf("scheduler failed: %w", err)
	}
	s.Start()

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

	httpCtx := &http.ClientCtx{
		Config:           cfg.Http,
		Store:            store,
		TaskCreator:      downloadTasksClient,
		DownloadClient:   dClient,
		DownloadStore:    downloadRepo,
		TorrentLookup:    dClient,
		Notifier:         notifier,
		DryMode:          cfg.DryMode,
		Breaker:          breaker,
		RunState:         store,
		WatchStore:       watchRepo,
		Engine:           engine,
		StaleRunAfter:    staleRunAfter(cfg.Cron),
		StaleWatchAfter:  staleRunAfter(cfg.WatchCron),
		StartedAt:        time.Now(),
		FailureThreshold: downloadTasks.FailureThreshold,
	}
	// a typed nil in the interface field would pass the handler's nil check and panic on the
	// first magnet call, so a disabled ext.to source leaves the field unset
	if sources.extto != nil {
		httpCtx.Magnets = sources.extto
	}

	httpClient := http.NewClient(httpCtx)

	go tgListener.SendMessagesForAdmins(ctx)
	go httpClient.Start(ctx, done)

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

// watchSolver is the consumer-side view of the flaresolverr client the ext.to source needs.
// The concrete solver type is unexported, so the composition root names it through this.
type watchSolver interface {
	Solve(ctx context.Context, url string) (*providers.SolvedPage, error)
}

// watchSourceSet keeps the concrete ext.to source next to the source list: the search
// endpoints resolve magnets through it, and that is a method the SearchSource interface
// deliberately does not carry.
type watchSourceSet struct {
	list  []watcher.SearchSource
	extto *watcher.ExttoSource
}

// watcherSources builds the sources every watch is run against. A missing Jackett api key or
// an unconfigured solver disables that one source with a startup warning — the service must
// degrade, not refuse to start.
func watcherSources(cfg *config.Config, solver watchSolver) watchSourceSet {
	var set watchSourceSet

	if cfg.Jackett.URL != "" && cfg.Jackett.APIKey != "" {
		set.list = append(set.list, watcher.NewJackettSource(watcher.JackettOptions{
			BaseURL:   cfg.Jackett.URL,
			PublicURL: cfg.Jackett.PublicURL,
			APIKey:    cfg.Jackett.APIKey,
		}))
		slog.Info("jackett watch source enabled", "url", redactURL(cfg.Jackett.URL))
	} else {
		slog.Warn("jackett watch source is disabled",
			"url_configured", cfg.Jackett.URL != "", "api_key_configured", cfg.Jackett.APIKey != "")
	}

	if solver == nil {
		slog.Warn("extto watch source is disabled, flaresolverr url is not configured")

		return set
	}

	set.extto = watcher.NewExttoSource(watcher.ExttoOptions{Solver: solver})
	set.list = append(set.list, set.extto)

	return set
}

// runWatchCycle keeps a failed cycle off the fatal path: only a registration error may stop
// the service, while a cycle error is logged and retried on the next tick.
func runWatchCycle(ctx context.Context, engine *watcher.Engine) {
	if err := engine.RunCycle(ctx); err != nil {
		slog.ErrorContext(ctx, "watcher cycle failed", "error", err)
	}
}

const (
	staleRunFallback = 2 * time.Hour
	// enough firings to see the longest gap of a clustered schedule such as `0 9,10 * * *`,
	// where the first gap is 1h but the real one is 23h
	staleRunSamples = 24
)

func staleRunAfter(cronExpr string) time.Duration {
	sched, err := cron.ParseStandard(cronExpr)
	if err != nil {
		slog.Warn("invalid cron expression, using fallback stale run interval", "cron", cronExpr, "error", err)
		return staleRunFallback
	}

	longest := time.Duration(0)
	at := sched.Next(time.Now())
	for range staleRunSamples {
		next := sched.Next(at)
		if gap := next.Sub(at); gap > longest {
			longest = gap
		}
		at = next
	}

	if longest <= 0 {
		return staleRunFallback
	}

	return 2 * longest
}

func newProviderBreaker(providerList []providers.Provider) *tracker.Breaker {
	names := make([]string, 0, len(providerList))
	for _, provider := range providerList {
		names = append(names, provider.Name())
	}

	return tracker.NewBreaker(nil, names...)
}

// redactedPlaceholder is url-safe on purpose: '*' would be percent-escaped into the log line
const redactedPlaceholder = "redacted"

func redactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<invalid url>"
	}
	// basic-auth credentials in the url would otherwise reach stdout and loki verbatim
	if u.User != nil {
		u.User = url.User(redactedPlaceholder)
	}
	q := u.Query()
	for key := range q {
		if strings.Contains(strings.ToLower(key), "apikey") || strings.Contains(strings.ToLower(key), "api_key") {
			q.Set(key, redactedPlaceholder)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}
