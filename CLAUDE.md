# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Magnet Feed Sync is a Telegram bot and web interface for automating torrent download management from RSS feed trackers (RuTracker, NNMClub, Jackett/Torznab). It creates download tasks on qBittorrent.

## Build and Development Commands

### Backend (Go)
```bash
# Apply database migrations (same code path the deploy uses)
go run ./cmd/migrate

# Install migration tool (only needed for `new`, `down` and `status`)
go install github.com/rubenv/sql-migrate/...@latest

# Create new migration — lands in app/migrations/ via dbconfig.yml
sql-migrate new <migration_name>

# Build binaries
go build -o server ./app
go build -o migrate ./cmd/migrate
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

Two images are built from this Dockerfile: the app (`final`) and the migration runner (`migrate-final`).
`final` must stay the **last** stage — a build without `target:` builds whatever is last, which is how the
migrate binary could end up published under the app's tags. The release workflow pins `target:` on both
steps, so stage order no longer decides that, but keep the ordering anyway.

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
  failure counters, the breaker snapshot, and the last cron run — it is not a hardcoded string. Both
  halves of the run state matter: a stale `last_run_at` is `unhealthy`, `last_run_ok = false` is
  `degraded` (a sweep that died at `GetAll` still refreshed the timestamp without checking anything).
  It also serves the watch CRUD routes (`/api/watches`), the two search entry points
  (`POST /api/watches/{id}/search` reproduces a stored watch, `POST /api/search` is ad-hoc), and the
  `watches` object on `/api/health`
- **schedular/**: Cron job scheduling via gocron. `AddJob(name, cronExpr, cb)` registers one job and
  `Start()` runs them all; there are two — the files sweep on `CRON` and the watcher sweep on
  `WATCH_CRON`. Every job runs in singleton mode — a sweep can outrun its interval, and overlapping runs
  would double-probe the breaker, race the run state, and (for the watcher) publish twice while racing
  the shared ext.to cookie/token state
- **task-store/**: SQLite repository pattern for task persistence
- **watcher/**: the release watcher — `SearchSource` implementations for Jackett (torznab search over all
  indexers) and ext.to (Cloudflare-fenced HTML plus a signed magnet POST), the `Engine` that merges,
  filters and diffs, and the JetStream publisher. `Evaluate` is side-effect free and is what both the
  cron and the search endpoints call, so the two cannot drift; `RunCycle` applies the effects
- **watch-store/**: SQLite repository for `watches` / `watch_seen`, verifying its schema the same way
  `task-store` does
- **tracker/**: RSS feed parsing with provider abstraction
  - `providers/`: RuTracker, NNMClub, and Jackett implementations
  - `breaker.go`: per-provider circuit breaker consumed by the cron sweep and the health endpoint
- **migrations/**: the `*.sql` migration set plus `embed.go`, which embeds it with `//go:embed *.sql` and
  exposes `Apply(db *sql.DB) (int, error)`. Imported by `cmd/migrate` and by the `task-store` test helper,
  **never by `app/main.go`** — the server binary's dependency graph stays free of `sql-migrate`
- **types/**: Shared type definitions (Location)
- **observability/**: Structured logging (slog) with Loki backend and OpenTelemetry tracing setup
- **utils/**: Shared utility functions (magnet link parsing, date parsing)

### Migration runner (`/cmd/migrate`)
Flagless one-shot binary: opens the database with `database.NewClient("tasks.db")` — the same `.db/<file>`
resolution and pragmas as the server — calls `migrations.Apply`, logs the count with plain `slog` to stderr
and exits. Exit code is the whole contract with compose: 0 on success including "nothing to apply",
non-zero on any failure. It closes the database explicitly rather than with `defer`, because `os.Exit`
skips deferred calls and a skipped `Close` leaves the WAL unrolled for the app container starting seconds
later. No config loading, no Loki, no tracing — the migrate image must not pull the observability stack.

### Frontend (`/frontend`)
- React 18 + TypeScript 5 + Vite
- Telegram Web App SDK integration (`@telegram-apps/sdk-react`)
- Telegram UI component library (`@telegram-apps/telegram-ui`)
- `Root.tsx` initializes SDK, `App.tsx` is the main component

### Database
- SQLite via `modernc.org/sqlite` (pure Go driver)
- Migrations in `/app/migrations/` using sql-migrate, applied by a **separate one-shot container** that
  compose runs to completion before the app starts (`depends_on: condition: service_completed_successfully`),
  so the app can only ever see a migrated schema. They live under `app/` because `go:embed` cannot reach
  files above the directory declaring it; `dbconfig.yml` points the CLI at the same `dir`
- Database file lives at `/.db/tasks.db` in **both** containers: neither image sets a `WORKDIR`, so CWD is
  `/` and `database.openDB` resolves `.db/<file>` from there; compose binds `.db:/.db` on each. The migrate
  container and the app must mount the identical path — a mismatch silently gives the app an empty database
  and `ErrSchemaNotInitialised`
- `database.Client.DB()` exposes the raw `*sql.DB`. It exists only so `migrations.Apply` can run on the
  connection the client opened; everything else goes through the retry-wrapped `Exec` / `Query` / `QueryRow`
- Tables: `files` (tracked tasks, including `consecutive_failures` / `last_error` / `last_error_at`),
  `app_state` (key/value; `last_run_at` + `last_run_ok`, written by the cron sweep), `watches` (saved
  hunts: queries, sources, both regexes, `rev`, `seeded_at`, run state) and `watch_seen` (one row per
  announced release, primary key `(watch_id, source, external_id)`)
- `watch_seen` is a **table, not a JSON column** on `watches`: dedup is a point insert with
  conflict-ignore instead of read-modify-write, growth is bounded per release rather than per watch, and
  the `INSERT OR REPLACE` column-reset trap is structurally impossible. `watch-store` uses explicit
  `UPDATE`s for the same reason
- The schema is declared **once**, in the migrations. `NewRepository` creates nothing; it *verifies* and
  returns `ErrSchemaNotInitialised` when the check fails. The check is on **columns**, not table existence —
  the incident this replaced had `files` present and the three failure columns missing, which a table check
  passes. Table existence is still checked *first*, because `PRAGMA table_info` on a missing table returns
  no rows and no error — without it an empty database is reported as a missing column. `newTestRepo(t)`
  runs `migrations.Apply` on the temp database first, so tests and production reach their schema by the
  same path
- `20240101000000-create-files.sql` is a baseline that reconstructs the *historical* shape of `files`: it
  includes `rss_url` (dropped by `20240511212753`) and omits `last_comment` / `location` (added by the two
  later 2024 migrations, which would fail with `duplicate column name`). Its `IF NOT EXISTS` is load-bearing
  — it is not in production's `gorp_migrations`, so it runs there against the live table as a no-op
- `Apply` **adopts** a database that has `files` but no `gorp_migrations` at all — what every checkout that
  ran the server before this change has, since the old `NewRepository` created the modern table and no
  history. Replaying the set there dies on `DROP COLUMN rss_url`, and the baseline is recorded before the
  failure, so the database is poisoned for every retry. `adoptUnmanagedSchema` instead counts how many
  migrations the live columns already satisfy and records them itself. The count stops at
  the first unsatisfied one: the app's own `CREATE TABLE` only ever grew, so what it produced is always a
  *prefix* of the set. A database with a **non-empty** `gorp_migrations` is left entirely to sql-migrate
- Adoption writes that prefix in **one transaction** (`recordAdopted`), rather than with `migrate.SkipMax`,
  which commits per record and creates the table before the first one. A kill in the middle of that left
  history neither absent nor complete: adoption never fired again and the leftovers replayed straight into
  `DROP COLUMN rss_url`. For the same reason "managed" is a **row count**, not table existence — sql-migrate
  creates the table before recording anything, so an aborted run can leave it empty, and an empty table is
  no history to hand over. `TestApplyAdoptsDespiteEmptyMigrationTable` reproduces the original failure
- Apply migrations with `go run ./cmd/migrate` (what `make apply-migrations` runs), never `sql-migrate up`.
  The CLI has no adoption step: on an unmanaged database it records the baseline and then dies on
  `DROP COLUMN rss_url`, and the recorded baseline disables the runner's adoption too. `sql-migrate` is for
  `new` / `down` / `status` only
- `CreateOrReplace` is `INSERT OR REPLACE`, which SQLite executes as DELETE + INSERT: any column missing from
  its INSERT list silently resets to its DEFAULT on every save. `TestCreateOrReplacePreservesConsecutiveFailures`
  guards this. Sync outcomes use targeted `UPDATE`s (`RecordSyncSuccess` / `RecordSyncFailure`) instead
- DB-backed tests go through `newTestRepo(t)` in `app/task-store/repository_test.go` — `database.openDB`
  resolves `.db/<file>` against the process CWD, so the helper does `t.Chdir(t.TempDir())` and then
  `migrations.Apply` before constructing the repository

## Key Patterns

### Backend
- Repository pattern for data access (task-store)
- Provider pattern for tracker integrations — each provider implements `CanHandle(url)` / `Parse(ctx, url)` / `Name()` and owns its parsing; fetching is delegated to an injected `Fetcher`
- `Fetcher` abstraction (`app/tracker/providers/fetcher.go`) — `Fetch(ctx, url) ([]byte, error)`. `main.go`
  picks the implementation per provider: RuTracker uses `solverFetcher` (FlareSolverr, because the site is
  behind a Cloudflare managed challenge) or `blockedFetcher` when `FLARESOLVERR_URL` is unset; NNM and
  Jackett use `directFetcher`. The solver reuses one session for the whole process — a cold solve is ~74s
  versus ~2.4s warm — and `main.go` destroys it on shutdown with a detached context. A nil fetcher is not
  supported; construct providers via `NewRutrackerProvider` / `NewNnmProvider` / `NewJackettProvider`.
  The solver serialises calls with a context-aware semaphore, not a mutex: a solve can take up to 180s,
  so `Close` must be able to give up on its context instead of waiting the in-flight fetch out.
  The held session id is dropped **only** when FlareSolverr reports it as gone (`errSessionGone`,
  matched on "session does not exist" — what it answers after a restart). Dropping it on any other
  failure would orphan a browser session there and force a cold solve on the next fetch
- Error taxonomy — `providers.ProviderError{Kind, Err}` wrapping a `Transient` / `Blocked` / `Permanent`
  kind, recoverable with `errors.As`. The fetcher classifies transport outcomes (403/429 and Cloudflare
  challenge markers → `Blocked`, 5xx/timeouts/net errors → `Transient`, 404 → `Permanent`); providers
  classify extraction failures such as a missing magnet link as `Permanent`. Only a refused *page fetch*
  can be `Blocked`: a failed `sessions.create`/`sessions.destroy` or a lost session is solver-side
  infrastructure and stays `Transient`, so a FlareSolverr restart never trips the breaker for 24h.
  A failed `request.get` is not automatically a refusal either — most of them are the solver's own
  browser/DNS/timeout trouble, so it counts as `Blocked` only when FlareSolverr's message names a
  challenge (`challengeMarkers`), and a response missing `solution` is a version mismatch, so `Transient`
- Circuit breaker (`tracker.Breaker`) — trips a provider on the first `Blocked` error, then skips its tasks
  without issuing requests until a half-open probe is allowed; cooldown doubles `1h → 24h` and resets on
  success. It gates only the cron sweep — manual refresh and task creation bypass it. Failure state is
  persisted per task (`consecutive_failures` / `last_error` / `last_error_at`); a task is *failing* at
  `FailureThreshold` (3) consecutive failures, which drives the 24h retry stretch, the health `failing`
  count, and one-shot Telegram transition messages. "Once" is held by an in-memory set
  (`Client.failingNotified`), not by the exact `2 → 3` transition: a manual refresh increments the
  counter without notifying, so the crossing run is often not a cron run and an edge trigger loses the
  alert for good. The set is cleared on any recorded success or removal, and it does not survive a
  restart — a still-failing task alerts once more after one
- Cron sweep vs manual refresh — only the cron job calls `CheckForUpdates`, which drives the breaker, the
  Telegram transitions and `last_run_at`. Both refresh endpoints are manual (`RefreshAll` /
  `CheckFileForUpdates`): they record store outcomes so the counters stay truthful, bypass the breaker gate
  and the 24h stretch so the button really retries, and touch neither the alerts nor the run state — a human
  pressing refresh must not trip a provider or hide a dead cron from `/api/health`
- Context-based graceful shutdown — the cron sweep runs on the app context, so `CheckForUpdates` stops at
  the next task when it is cancelled and neither records the aborted parse as a task failure nor overwrites
  `last_run_at`; without those guards every restart mid-sweep would trip the breaker and notify
- Every admin message goes out with `ParseMode: MarkdownV2` (`events.NewMarkdownMessage`), and Telegram
  rejects a whole message over one unescaped reserved char. Plain-text alerts are escaped with
  `escapeMarkdown`; `MetadataToMsg` wraps its JSON in a code fence and escapes only backticks/backslashes
- Watcher cycle ordering — a watch whose `seeded_at` is NULL records everything it matched **without
  publishing** (a fresh watch would otherwise wake the agent with releases it already has), and only when
  `Errs` is empty: seeding from a partially failed run buries whatever the dead source never reported.
  Afterwards the order is **publish first, mark seen second** — a crash between the two costs one
  duplicate wake, the reverse loses the release permanently and silently. A non-empty `Errs` never
  suppresses a publish; it lands in `last_status`, which is what health and the operator read. Items
  filtered out by the regexes are never written to `watch_seen`, so loosening a regex resurfaces them
- The watcher never downloads anything. It notifies; the agent verifies (indexers only index the title,
  and the title lies) and decides. Every published hit is also mirrored to the admin Telegram channel
  with a **non-blocking** send — `messagesForSend` is unbuffered, so a blocking send would wedge the cron
  behind a stalled reader, and the mirror is what makes a missed re-arm on the agent side visible
- NATS publishing is JetStream with a message id of `<watch_id>:<max Source:ExternalID over New>`, so a
  duplicate publish from the publish-then-mark ordering is collapsed by the stream's dedup window. The
  `TUCLAW` stream is owned by the tuclaw daemon — this service connects and publishes only, never creates
  or reconfigures a stream. A failed connect is logged and the service starts anyway (a fatal connect
  would crash-loop the container on every NATS restart); while disconnected `Publish` errors, so nothing
  is marked seen and the release is retried next cycle. An empty `NATS_URL` disables publishing the same
  way — a would-be publish is an error, never a silent success
- Degrade, not die — a missing `JACKETT_API_KEY` or an unconfigured FlareSolverr disables that source with
  a startup warning instead of failing the boot; a watcher cycle error is logged and the sweep continues,
  and only a job *registration* error is fatal
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
- `CRON`: update-sweep schedule, standard 5-field expression (default `0 * * * *`). `main.go` also derives the health staleness window from it (twice the longest gap among the next `staleRunSamples` firings, so a clustered schedule such as `0 9,10 * * *` is not judged by its 1h gap; `staleRunFallback` 2h + a WARN log when it cannot be parsed)
- `WATCH_CRON`: watcher-sweep schedule, standard 5-field expression (default `20 * * * *` — offset from the files job at `0 * * * *` so the two never start together). `main.go` derives the watch health staleness window from it with the same `staleRunAfter` helper the files sweep uses (2× the longest gap among the next firings; 2h fallback + WARN when unparseable)
- `JACKETT_URL`: Jackett instance base URL (optional, include API key in URL query string)
- `JACKETT_API_KEY`: Jackett api key for the watcher's torznab search. `JACKETT_URL` carries no key, so without this the Jackett watch source is disabled with a startup warning
- `JACKETT_PUBLIC_URL`: public Jackett base used to rewrite the scheme+host of a search result's download link (defaults to `JACKETT_URL`). Jackett emits its own *internal* base there, which would resolve nowhere at download time
- `NATS_URL`: JetStream endpoint for watch notifications, e.g. `nats://nats:4222`. **Empty disables publishing** (warned once at startup); the service still starts and still runs cycles
- `FLARESOLVERR_URL`: FlareSolverr command endpoint including the `/v1` path (optional). Empty = RuTracker gets `blockedFetcher` and the service still starts
- `OTEL_SERVICE_NAME`: OpenTelemetry service name (default: "magnet-feed-sync")
- `OTEL_EXPORTER_OTLP_ENDPOINT`: OTLP HTTP endpoint for trace export (optional, tracing disabled when empty)
- `LOKI_URL`: Grafana Loki base URL for centralized logging (optional, logs go to stdout only when empty). The code appends `/loki/api/v1/push` automatically

## Commit Convention

Format: `type(scope): description`
- Types: `feat`, `fix`, `refactor`, `perf`
- Example: `feat(database): add retry mechanism for database operations`
