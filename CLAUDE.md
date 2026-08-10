# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Magnet Feed Sync is a Telegram bot and web interface for automating torrent download management from RSS feed trackers (RuTracker, NNMClub, Jackett/Torznab). It creates download tasks on qBittorrent.

## Build and Development Commands

### Backend (Go)
```bash
# Install migration tool
go install github.com/rubenv/sql-migrate/...@latest

# Apply database migrations
sql-migrate up

# Create new migration
sql-migrate new <migration_name>

# Build binary
go build -o server ./app
```

### Frontend
```bash
cd frontend
pnpm install
pnpm dev          # Development server
pnpm build        # Production build (runs tsc -b && vite build)
pnpm lint         # ESLint with zero warnings tolerance
```

### Docker
```bash
docker compose up --build
```

## Architecture

### Backend (`/app`)
- **main.go**: Entry point, wires dependencies, starts Telegram listener and HTTP server
- **bot/**: Telegram command handlers and download task management
- **config/**: Environment-based configuration via cleanenv
- **database/**: SQLite client with retry mechanism for reliability
- **download-client/**: qBittorrent client (`qbittorrent/`) built on `github.com/autobrr/go-qbittorrent`.
  Consumers depend on small consumer-side `DownloadClient` interfaces; `main.go` injects the concrete client
- **events/**: Telegram event handlers for bot interactions
- **http/**: HTTP server serving web UI, REST API, and health checks. Two download entry points with a
  deliberate split: `POST /api/files` is tracked (provider parses the tracker page, a row is persisted,
  the cron feed re-checks it); `POST /api/downloads` is one-shot fire-and-forget (magnet or `.torrent`
  URL forwarded verbatim to the download client, no row, no monitoring, nothing logged/persisted).
  `GET /api/health` reports real state (`ok` / `degraded` / `unhealthy` + 503), derived from per-task
  failure counters, the breaker snapshot, and the last cron run — it is not a hardcoded string
- **schedular/**: Cron job scheduling via gocron
- **task-store/**: SQLite repository pattern for task persistence
- **tracker/**: RSS feed parsing with provider abstraction
  - `providers/`: RuTracker, NNMClub, and Jackett implementations
  - `breaker.go`: per-provider circuit breaker consumed by the cron sweep and the health endpoint
- **types/**: Shared type definitions (Location)
- **observability/**: Structured logging (slog) with Loki backend and OpenTelemetry tracing setup
- **utils/**: Shared utility functions (magnet link parsing, date parsing)

### Frontend (`/frontend`)
- React 18 + TypeScript 5 + Vite
- Telegram Web App SDK integration (`@telegram-apps/sdk-react`)
- Telegram UI component library (`@telegram-apps/telegram-ui`)
- `Root.tsx` initializes SDK, `App.tsx` is the main component

### Database
- SQLite via `modernc.org/sqlite` (pure Go driver)
- Migrations in `/migrations/` using sql-migrate
- Database file persisted in Docker volume at `/db/`

## Key Patterns

### Backend
- Repository pattern for data access (task-store)
- Provider pattern for tracker integrations — each provider implements `CanHandle(url)` / `Parse(ctx, url)` / `Name()` and owns its parsing; fetching is delegated to an injected `Fetcher`
- `Fetcher` abstraction (`app/tracker/providers/fetcher.go`) — `Fetch(ctx, url) ([]byte, error)`. `main.go`
  picks the implementation per provider: RuTracker uses `solverFetcher` (FlareSolverr, because the site is
  behind a Cloudflare managed challenge) or `blockedFetcher` when `FLARESOLVERR_URL` is unset; NNM and
  Jackett use `directFetcher`. The solver reuses one session for the whole process — a cold solve is ~74s
  versus ~2.4s warm — and `main.go` destroys it on shutdown with a detached context. A nil fetcher is not
  supported; construct providers via `NewRutrackerProvider` / `NewNnmProvider` / `NewJackettProvider`
- Error taxonomy — `providers.ProviderError{Kind, Err}` wrapping a `Transient` / `Blocked` / `Permanent`
  kind, recoverable with `errors.As`. The fetcher classifies transport outcomes (403/429 and Cloudflare
  challenge markers → `Blocked`, 5xx/timeouts/net errors → `Transient`, 404 → `Permanent`); providers
  classify extraction failures such as a missing magnet link as `Permanent`
- Circuit breaker (`tracker.Breaker`) — trips a provider on the first `Blocked` error, then skips its tasks
  without issuing requests until a half-open probe is allowed; cooldown doubles `1h → 24h` and resets on
  success. It gates only the cron sweep — manual refresh and task creation bypass it. Failure state is
  persisted per task (`consecutive_failures` / `last_error` / `last_error_at`); a task is *failing* at
  `FailureThreshold` (3) consecutive failures, which drives the 24h retry stretch, the health `failing`
  count, and one-shot Telegram transition messages
- Context-based graceful shutdown
- Retry mechanism for database operations
- Structured logging via `log/slog` with global default logger (`slog.SetDefault`) — use `slog.ErrorContext(ctx, ...)` in HTTP handlers for trace_id correlation
- OpenTelemetry tracing via global `otel.Tracer()` provider with noop fallback when endpoint not configured

### Frontend
- Strict ESLint config enforcing:
  - No arrow functions in props (use useCallback)
  - File naming: camelCase for .ts, PascalCase for .tsx
  - No default exports in component files
  - Max JSX nesting depth: 5 levels

## Configuration

Environment variables (see compose.yaml):
- `QBITTORRENT_URL/USERNAME/PASSWORD/DESTINATION`: qBittorrent connection
- `TELEGRAM_TOKEN`: Bot token
- `TELEGRAM_SUPER_USERS`: Comma-separated admin user IDs
- `HTTP_PORT`: Web server port (default 8080)
- `DRY_MODE`: Testing mode flag
- `JACKETT_URL`: Jackett instance base URL (optional, include API key in URL query string)
- `FLARESOLVERR_URL`: FlareSolverr command endpoint including the `/v1` path (optional). Empty = RuTracker gets `blockedFetcher` and the service still starts
- `OTEL_SERVICE_NAME`: OpenTelemetry service name (default: "magnet-feed-sync")
- `OTEL_EXPORTER_OTLP_ENDPOINT`: OTLP HTTP endpoint for trace export (optional, tracing disabled when empty)
- `LOKI_URL`: Grafana Loki base URL for centralized logging (optional, logs go to stdout only when empty). The code appends `/loki/api/v1/push` automatically

## Commit Convention

Format: `type(scope): description`
- Types: `feat`, `fix`, `refactor`, `perf`
- Example: `feat(database): add retry mechanism for database operations`
