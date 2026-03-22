# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Magnet Feed Sync is a Telegram bot and web interface for automating torrent download management from RSS feed trackers (RuTracker, NNMClub, Jackett/Torznab). It creates download tasks on Synology NAS DownloadStation or qBittorrent.

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
- **download-client/**: Abstraction layer for download providers
  - `download-station/`: Synology DownloadStation API
  - `qbittorrent/`: qBittorrent API client
- **events/**: Telegram event handlers for bot interactions
- **http/**: HTTP server serving web UI, REST API (file management endpoints), and health checks
- **schedular/**: Cron job scheduling via gocron
- **task-store/**: SQLite repository pattern for task persistence
- **tracker/**: RSS feed parsing with provider abstraction
  - `providers/`: RuTracker, NNMClub, and Jackett implementations
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
- Provider pattern for tracker integrations — each provider implements `CanHandle(url)` / `Parse(ctx, url)` interface, owns its own fetch + parse logic
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
- `DOWNLOAD_CLIENT`: "synology" or "qbittorrent"
- `SYNOLOGY_URL/USERNAME/PASSWORD/DESTINATION`: NAS connection
- `QBITTORRENT_URL/USERNAME/PASSWORD/DESTINATION`: qBittorrent connection
- `TELEGRAM_TOKEN`: Bot token
- `TELEGRAM_SUPER_USERS`: Comma-separated admin user IDs
- `HTTP_PORT`: Web server port (default 8080)
- `DRY_MODE`: Testing mode flag
- `JACKETT_URL`: Jackett instance base URL (optional, include API key in URL query string)
- `OTEL_SERVICE_NAME`: OpenTelemetry service name (default: "magnet-feed-sync")
- `OTEL_EXPORTER_OTLP_ENDPOINT`: OTLP HTTP endpoint for trace export (optional, tracing disabled when empty)
- `LOKI_URL`: Grafana Loki base URL for centralized logging (optional, logs go to stdout only when empty). The code appends `/loki/api/v1/push` automatically

## Commit Convention

Format: `type(scope): description`
- Types: `feat`, `fix`, `refactor`, `perf`
- Example: `feat(database): add retry mechanism for database operations`
