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
  Consumers depend on small consumer-side `DownloadClient` interfaces; `main.go` injects the concrete client.
  `CreateDownloadTask` returns the **hash**, not just an error: `added_torrent_ids[0]` when qBittorrent
  supplies it, otherwise `utils.ExtractBtihHash` for a `magnet:` source — but only when that yields a
  40-char hex infohash, since a base32 magnet would be stored as a hash `torrents/info` never reports
  and the sweep would read the miss as a torrent deleted by hand. An add nothing identifies is
  **success with an empty hash, not an error**: `added_torrent_ids` is absent on every qBittorrent below
  the WebAPI version that added it (it answers `text/plain` "Ok."), so failing there would reject every
  plain `.torrent` add. The check belongs to whoever needs the hash — `POST /api/downloads` with
  `notify: true` refuses the empty hash with 503 rather than writing a row the sweep can never match,
  while the fire-and-forget path never needed it. A 200 carrying `failure_count > 0` with no added id
  is the one exception: qBittorrent refused the source, so it is an **error** — the magnet fallback
  would otherwise hand back a hash for a torrent that was never added, and the sweep would publish it
  as deleted by hand ten minutes later. Every add error the library builds embeds the source verbatim
  and a jackett `.torrent` link carries `JACKETT_API_KEY` in its query, so the message is redacted with
  `utils.RedactURL` **inside the client** (the library error stays underneath, so `errors.Is` still
  sees it) rather than at each caller that logs it. `TorrentStates(ctx, hashes)` is the paired lookup the
  sweep and the duplicate path share; a hash qBittorrent does not know is **absent from the map**, never
  a zero entry, because absence is what the sweep reads as "deleted by hand". It asks in **batches of
  100**: the library joins the hashes into the query string of a GET `torrents/info` at ~41 chars each,
  and a pending set that outgrows the request-line limit would fail a lookup whose failure aborts the
  whole sweep, stalling every pending row rather than one. The subject both halves of
  the promise use is built once by `downloads.Subject(id)` — the HTTP response hands back exactly what
  the sweep will publish on
- **events/**: Telegram event handlers for bot interactions
- **http/**: HTTP server serving web UI, REST API, and health checks. Two download entry points with a
  deliberate split: `POST /api/files` is tracked (provider parses the tracker page, a row is persisted,
  the cron feed re-checks it); `POST /api/downloads` is one-shot fire-and-forget by default (magnet or
  `.torrent` URL forwarded verbatim to the download client, no row, no monitoring, nothing
  logged/persisted). `"notify": true` is the explicit exception on both: it persists a `downloads` row
  and promises one terminal event, and it is **refused with 503 before qBittorrent is touched** when the
  notifier is disabled (and, on `/api/downloads` only, in dry mode) — a promise nobody can keep must not
  be accepted. `GET /api/health` reports real state (`ok` / `degraded` / `unhealthy` + 503), derived from
  per-task failure counters, the breaker snapshot, and the last cron run — it is not a hardcoded string.
  Both halves of the run state matter: a stale `last_run_at` is `unhealthy`, `last_run_ok = false` is
  `degraded` (a sweep that died at `GetAll` still refreshed the timestamp without checking anything).
  It also serves the watch CRUD routes (`/api/watches`), the two search entry points
  (`POST /api/watches/{id}/search` reproduces a stored watch, `POST /api/search` is ad-hoc), and the
  `watches` and `downloads` objects on `/api/health`. A watch that has not run yet is `pending`: counted
  separately in `watches.pending` (and reported as `state: pending` on `/api/watches`, a value derived
  from the timestamps, never stored), and `degraded` only once it is older than the watch staleness
  window measured from its **own** `created_at`. Never from process start: the process runs for weeks,
  so a start-time grace read every watch the agent created as degraded until the next `:20` tick and
  paged Gatus each time. Every error the two download entry points answer
  is JSON `{"isError": true, "error": "<reason>"}` written by `encodeError` - the agent reads the body,
  not the status, and a fixed `failed to create file from URL` hid a 409 behind a 500 for two days.
  `createFileFailure` picks the status for `POST /api/files`: 400 no provider, 409 file already
  tracked (`createWithLock` is a strict create: an active row is refused before qBittorrent is touched,
  a soft-deleted one is re-created) or torrent already in qBittorrent (a tracked file adds its own
  download, so it is **not** created for a torrent added through `/api/downloads` first), 422
  permanent provider error, 502 blocked/transient, 500 otherwise; the reason carries the error chain,
  which is already redacted at the source (`WithoutURL`, `withoutSource`, `stripAPIKey`).
  `PATCH /api/files/{fileId}` is the consumer's edit: `notify` and/or `location`, both optional and an
  omitted one unchanged, written with the targeted `UpdateSettings` rather than `CreateOrReplace`,
  which stays the sweep's whole-row primitive. A location is validated against `GET /api/file-locations`,
  stored first, and then the downloaded files are moved best-effort with the outcome under `move`
  (`POST /api/file-locations` does the same for the web UI and delegates to the same store call)
- **schedular/**: Cron job scheduling via gocron. `AddJob(name, cronExpr, cb)` registers one job and
  `Start()` runs them all; there are three — the files sweep on `CRON`, the watcher sweep on
  `WATCH_CRON` and the download sweep on `DOWNLOAD_CRON`. Every job runs in singleton mode — a sweep can
  outrun its interval, and overlapping runs would double-probe the breaker, race the run state, publish
  the same download event twice, and (for the watcher) publish twice while racing the shared ext.to
  cookie/token state
- **task-store/**: SQLite repository pattern for task persistence
- **watcher/**: the release watcher — `SearchSource` implementations for Jackett (torznab search over all
  indexers) and ext.to (Cloudflare-fenced HTML plus a signed magnet POST), the `Engine` that merges,
  filters and diffs, and the JetStream publisher. `Evaluate` is side-effect free and is what both the
  cron and the search endpoints call, so the two cannot drift; `RunCycle` applies the effects.
  The Jackett external id is `<page host>/<t>`, not the bare `t=`: the torznab endpoint aggregates every
  indexer at once and `t` is the topic id on RuTracker and NNM alike, so an un-namespaced id collides
  across them and buries the second release as already announced. Both sources wrap their transport
  errors in `providers.WithoutURL` and cap the body at `maxSearchResponseSize` — the jackett endpoint
  carries the api key in its query string, the solver endpoint (reached through ext.to's cookie refresh)
  carries whatever userinfo `FLARESOLVERR_URL` holds, and a `*url.Error` reaches `last_status`, the
  unauthenticated search responses and loki verbatim. A search that fails stops that source's remaining
  queries — a source that just refused us will refuse them too, and on ext.to each attempt holds the
  single solver slot for up to 180s — but what its earlier queries returned is kept, since discarding it
  would withhold a release that was genuinely found. ext.to's page tokens are stored together with the
  session (cookie + User-Agent) that fetched them, and the signed magnet POST is sent under *that* session:
  the cookie is process-wide state the cron and the http handlers share, so a refresh landing between the
  search and the POST would otherwise pair this query's page/csrf tokens with a different session, which
  ext.to refuses
- **watch-store/**: SQLite repository for `watches` / `watch_seen`, verifying its schema the same way
  `task-store` does — table existence first, then the expected **columns** (`requiredColumns`), because a
  table check passes a table whose columns a half-applied migration never added. `Disable` / `Revive` are
  a pair: nothing else writes `disabled_at`, and without `Revive` a soft-deleted id could never be
  re-created, since the row still exists and a create is a conflict. `Revive` clears the run lifecycle
  (`seeded_at`, `last_run_at`, `last_status`) and re-stamps `created_at`, which the health grace for a
  watch that has not run yet is measured from, along with the soft delete — a re-create is a *create*, so it
  seeds silently again (`Update` names the request's columns only and would leave a re-created watch
  publishing whatever its new queries or wider regex match) and does not report the dead watch's status as
  its own on `/api/health`. `watch_seen` is deliberately untouched, which is what keeps that seed from
  being a replay
- **notify/**: the shared JetStream transport — `NewClient(Options{URL})`, `Publish(ctx, Message)`,
  `Enabled()`, `Close()`, and `ErrDisabled`. One client is built in `main.go` and shared by the watcher
  publisher, the download sweeper and `bot/download-tasks`; **`main.go` alone closes it**, no consumer
  dials or closes what it did not open. Each consumer declares its own narrow interface over it rather
  than taking `*notify.Client`. An empty URL or a failed connect yields a disabled client whose `Publish`
  returns `ErrDisabled` — never a silent success, so nothing is marked published and the event is retried
- **downloads/**: the `Download` domain type, the single classifier `Classify` and the `Sweeper` that
  publishes terminal download events on `DOWNLOAD_CRON`. Mirrors how `watcher` defines `Watch` while
  `watch-store` persists it: the store, the torrent lookup and the notifier each sit behind a
  consumer-side interface declared here
- **download-store/**: SQLite repository over the `downloads` table, verifying its schema by table then
  **columns** the same way `task-store` and `watch-store` do. Outcome writes are explicit `UPDATE`s, and
  `MarkPublished` carries `WHERE id = ? AND published_at IS NULL`, which is what makes the
  publish-then-mark ordering safe to run twice after a crash. `Create` writes `created_at` from a Go
  `time.Time` rather than leaning on `CURRENT_TIMESTAMP`, whose one-second resolution would make the
  ordering of two rows added in the same second arbitrary; `Pending` and `NewestBySource` break ties on
  `rowid` so the order is total either way. Every timestamp is stored **in UTC**: the driver writes a
  `time.Time` as RFC3339 text carrying its offset, so `ORDER BY created_at` compares wall clocks, and
  under a DST zone the autumn rollback hour would sort a newer row before an older one — a difference
  the `rowid` tiebreak cannot repair, because the two values are not equal
- **tracker/**: RSS feed parsing with provider abstraction
  - `providers/`: RuTracker, NNMClub, and Jackett implementations
  - `breaker.go`: per-provider circuit breaker consumed by the cron sweep and the health endpoint
- **migrations/**: the `*.sql` migration set plus `embed.go`, which embeds it with `//go:embed *.sql` and
  exposes `Apply(db *sql.DB) (int, error)`. Imported by `cmd/migrate` and by the `task-store` test helper,
  **never by `app/main.go`** — the server binary's dependency graph stays free of `sql-migrate`
- **types/**: Shared type definitions (`Location`, `TorrentState`), so `app/downloads` and `app/http` can
  name what a torrent lookup returns without importing the download client
- **observability/**: Structured logging (slog) with Loki backend and OpenTelemetry tracing setup
- **utils/**: Shared utility functions (magnet link parsing, date parsing, `RedactURL` — masks the
  api key / userinfo a source or tracker URL carries before it reaches a log, and returns a URL that
  holds no credential verbatim so a clean one is not re-encoded for the reader)

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
- Tables: `files` (tracked tasks, including `consecutive_failures` / `last_error` / `last_error_at` and
  `notify`), `app_state` (key/value; `last_run_at` + `last_run_ok`, written by the cron sweep), `watches`
  (saved hunts: queries, sources, both regexes, `rev`, `seeded_at`, run state), `watch_seen` (one row per
  announced release, primary key `(watch_id, source, external_id)`) and `downloads` (one row per
  `POST /api/downloads` made with `notify: true`, keyed by the id this service generates; `published_at`
  NULL is the sweep's work queue)
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
- `files.notify` shares that trap and is guarded by `TestCreateOrReplacePreservesNotify`. The cron sweep
  calls `CreateOrReplace` on every check of every tracked file, so the flag has to be held in **two**
  places or it is cleared on the first tick after it is set: the column list and values of
  `CreateOrReplace`, and the carry-over from the stored row into the freshly parsed metadata in
  `processFileMetadata` — beside the one `Location` already has, but **unconditional**, because `notify`
  has no sentinel value and a `!= ""`-style guard would wipe the flag on every file with an empty location
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
- Circuit breaker (`tracker.Breaker`) — trips a provider on the second `Blocked` error in a row
  (`tripAfter`), then skips its tasks without issuing requests until a half-open probe is allowed;
  cooldown doubles `1h → 24h` and resets on success. A lone `Blocked` is far more often a FlareSolverr
  timeout than a refusal (5 of alpha's 18 timeout runs between July and September were singles, and
  each cost an hour of skipped tasks), while a real block fails every fetch in a row, so the streak
  spans runs, ignores other error kinds, and only a successful fetch clears it. It gates only the cron sweep — manual refresh and task creation bypass it. Failure state is
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
  publishing** (a fresh watch would otherwise wake the agent with releases it already has); the matched
  rows are written either way, but `seeded_at` itself is set only when nothing *failed*, because seeding
  from a partially failed run buries whatever the dead source never reported. An
  `unavailableSourceError` — a source the watch names that this process does not run, no api key or no
  solver — is deliberately **not** a failure for that rule (`hasSearchFailure`): it never comes back on
  its own, and holding the seed for it leaves the watch unseeded, and therefore permanently silent, for
  the whole life of the deployment. It is still reported in `last_status`.
  Afterwards the order is **publish first, mark seen second** — a crash between the two costs one
  duplicate wake, the reverse loses the release permanently and silently. A non-empty `Errs` never
  suppresses a publish; it lands in `last_status`, which is what health and the operator read. Items
  filtered out by the regexes are never written to `watch_seen`, so loosening a regex resurfaces them
- The watcher never downloads anything. It notifies; the agent verifies (indexers only index the title,
  and the title lies) and decides. Every published hit is also mirrored to the admin Telegram channel
  with a **non-blocking** send — `messagesForSend` is unbuffered, so a blocking send would wedge the cron
  behind a stalled reader, and the mirror is what makes a missed re-arm on the agent side visible
- NATS publishing is JetStream with a message id of `<watch_id>:<sha256 of the sorted SeenKeys of New>`, so
  a duplicate publish from the publish-then-mark ordering is collapsed by the stream's dedup window. The id
  digests the **whole** set, not its maximum: after an acked publish whose `MarkSeen` failed, the retry
  carries the same releases plus whatever the cycle found since, and a maximum-only key is unchanged by an
  added item that sorts lower — JetStream would ack the retry as a duplicate while the cycle marks the
  whole set seen, losing that item permanently and silently. The
  `TUCLAW` stream is owned by the tuclaw daemon — this service connects and publishes only, never creates
  or reconfigures a stream. A failed connect is logged and the service starts anyway (a fatal connect
  would crash-loop the container on every NATS restart); while disconnected `Publish` errors, so nothing
  is marked seen and the release is retried next cycle. An empty `NATS_URL` disables publishing the same
  way — a would-be publish is an error, never a silent success
- Download and release-update notifications are **opt-in per request** (`"notify": true` on
  `POST /api/downloads` and `POST /api/files`); a request without the flag behaves exactly as before and
  writes no row. There are exactly two subjects: `tuclaw.downloads.completed.<download_id>` and
  `tuclaw.releases.updated.<file_id>`. **Both terminal download outcomes publish on the `completed`
  subject** — the one the HTTP response handed back — and are told apart only by `status`
  (`completed` / `failed`). There is deliberately no `tuclaw.downloads.failed.*`: the agent arms a
  one-shot event task on the single subject it was given, so a failure published anywhere else would
  never fire it, which is the "event that never arrives" this feature exists to prevent. Message ids for
  JetStream dedup are `<download_id>:<status>` and `<file_id>:<sha256 of the new magnet>`. On a tracked
  file the flag lives on the `files` row: `POST /api/files` sets it from the request (`OnMessage` passes
  `false`) and `PATCH /api/files/{id}` is the only way to change it afterwards. A re-`POST` of an active
  file is refused with 409 (`types.ErrFileAlreadyTracked`, checked in `createWithLock` before qBittorrent
  is touched), so nothing disarms the flag by accident; a soft-deleted row is re-created and takes the
  request's flag. Only the carry-over in `processFileMetadata` protects it during a sweep
- The completion criterion lives in **one** function, `downloads.Classify`, called by both the sweep and
  the HTTP duplicate path — a second copy is how one call site quietly ends up with `!= 0`. Failure rules
  run first (`error` / `missingFiles`, or the hash absent from a lookup that *succeeded*), so a row that
  classifies `failed` is never also `completed` however good its progress looks. Otherwise `completed`
  needs `progress >= 1`, `completion_on > 0` (never `!= 0` — an unfinished torrent reports `-1`) and a
  state outside the deny list `checkingUP` / `checkingResumeData` / `moving` / `allocating`. It is a
  **deny list on purpose**: an allow list of "finished" states would silently stop firing on a state
  qBittorrent adds later, and `moving` is excluded because `content_path` then still points at the
  directory the files are leaving. A lookup that *errored* is never expressed as "not found" — that would
  publish a false failure for every pending row — so a failed `TorrentStates` aborts the whole cycle
- The download sweep reads `Pending()` first and returns **without calling qBittorrent at all** when it is
  empty, so an idle tick costs one SQL query; otherwise it makes exactly one `TorrentStates` call for the
  whole set and matches in memory. Ordering is **publish first, `MarkPublished` second**, the same rule
  the watcher follows: a crash between the two costs one duplicate wake, the reverse loses the event
  permanently and silently. One row's publish failure leaves that row unmarked for the next tick instead
  of aborting the cycle
- A duplicate add (`409`) is success, not failure: `types.ErrTorrentAlreadyExists` is returned **only**
  when `errors.Is(err, qbt.ErrTorrentAddFailed)` *and* the message contains `conflicts detected`, because
  that sentinel is also used for the 415 "torrent file not valid" case — over-matching it would report a
  broken `.torrent` URL as a finished download. The handler resolves the hash (`ExtractBtihHash` for a
  magnet, and only when `utils.IsInfoHash` accepts it — the same guard the client applies, since a
  base32 hash names a torrent `torrents/info` never reports, so the caller would be told the download
  failed and handed no subject; otherwise `NewestBySource`) and answers 200 with the current state inline. With `notify: true` an
  already-complete duplicate gets its row written with `published_at` already set so the sweep skips it —
  the caller was just told inline and must not be woken twice — and a `failed` one gets **no row at all**,
  since the sweep would otherwise publish a failure on a subject the caller was never given. Only a
  lookup that *succeeded* and did not list the hash may classify at all: an errored (or unwired) lookup
  established nothing while the 409 proved the torrent is there, so it is left **undecided** and gets a
  row and a subject like any unfinished duplicate — the same rule the sweep follows when it aborts the
  cycle on a failed `TorrentStates`. Collapsing the error into "not found" would answer `200 ok` with no
  subject and no row, leaving the caller waiting for the event this feature exists to guarantee
- A release update publishes only when **all** hold: the sweep is the cron one (a human pressing refresh
  must not wake the agent — the same rule the breaker and the run state follow), dry mode is off, the
  magnet actually changed, `CreateDownloadTask` returned nil (so the revert path publishes nothing), and
  the file id matches `^[A-Za-z0-9_-]+$`. That last check is not cosmetic: a dot or a space grows the
  subject an extra token and stops matching the filter the agent armed, and Jackett file ids fall back to
  a btih hash. A publish failure is logged and never aborts the sweep or the re-download
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
- `DOWNLOAD_CRON`: download-sweep schedule, standard 5-field expression (default `*/10 * * * *`). Declared like `WATCH_CRON` — an empty value falls back to the constant. Ten minutes is a latency choice, not a load one: an idle tick costs one SQL query and no qBittorrent call, and the latency is noise against a download measured in tens of minutes
- `JACKETT_URL`: Jackett instance base URL (optional, include API key in URL query string)
- `JACKETT_API_KEY`: Jackett api key for the watcher's torznab search. `JACKETT_URL` carries no key, so without this the Jackett watch source is disabled with a startup warning
- `JACKETT_PUBLIC_URL`: public Jackett base used to rewrite the scheme+host of a search result's download link (defaults to `JACKETT_URL`). Jackett emits its own *internal* base there, which would resolve nowhere at download time
- `NATS_URL`: JetStream endpoint for watch, download and release-update notifications, e.g. `nats://nats:4222`. **Empty disables publishing** (warned once at startup); the service still starts and still runs cycles, but a request asking for `notify: true` is refused with 503 rather than accepted with an event that could never arrive
- `FLARESOLVERR_URL`: FlareSolverr command endpoint including the `/v1` path (optional). Empty = RuTracker gets `blockedFetcher` **and the ext.to watch source is disabled** (it refreshes its cookie through the same solver); the service still starts
- `OTEL_SERVICE_NAME`: OpenTelemetry service name (default: "magnet-feed-sync")
- `OTEL_EXPORTER_OTLP_ENDPOINT`: OTLP HTTP endpoint for trace export (optional, tracing disabled when empty)
- `LOKI_URL`: Grafana Loki base URL for centralized logging (optional, logs go to stdout only when empty). The code appends `/loki/api/v1/push` automatically

## Commit Convention

Format: `type(scope): description`
- Types: `feat`, `fix`, `refactor`, `perf`
- Example: `feat(database): add retry mechanism for database operations`
