# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Magnet Feed Sync is a Telegram bot and web interface for automating torrent download management from RSS feed trackers (RuTracker, NNMClub). It creates download tasks on Synology NAS DownloadStation or qBittorrent.

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
- **http/**: HTTP server serving web UI and health endpoints
- **schedular/**: Cron job scheduling via gocron
- **task-store/**: SQLite repository pattern for task persistence
- **tracker/**: RSS feed parsing with provider abstraction
  - `providers/`: RuTracker and NNMClub implementations

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
- Context-based graceful shutdown
- Retry mechanism for database operations

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

## Commit Convention

Format: `type(scope): description`
- Types: `feat`, `fix`, `refactor`, `perf`
- Example: `feat(database): add retry mechanism for database operations`
