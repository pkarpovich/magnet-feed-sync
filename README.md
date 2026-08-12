# Magnet Feed Sync

## Introduction

**Magnet Feed Sync** is a Telegram bot and web interface for automating torrent download management. It parses
tracker pages to extract magnet links, creates download tasks on qBittorrent, and logs task details in a database. The bot
also monitors for updates on tracked pages and schedules new download tasks as needed.

## Features

- Automated creation of download tasks on qBittorrent from provided magnet links.
- Real-time interaction and management via Telegram.
- Persistent storage and management of download tasks.
- Database logging for task status and history.
- Release watcher: saved cross-indexer hunts (Jackett + ext.to) that publish a NATS notification and a
  Telegram message when a genuinely new release shows up.

## Usage

**Magnet Feed Sync** can be interacted through Telegram commands and automated cron jobs:

### Telegram Commands

Users can send commands to initiate downloads, view active tasks, or manage settings.

To create a new download task, send a message to the bot with tracker page.

**Supported Trackers:**

- [rutracker.org](https://rutracker.org) - requires a running [FlareSolverr](https://github.com/FlareSolverr/FlareSolverr)
  instance (see `FLARESOLVERR_URL`); the site sits behind a Cloudflare managed challenge and cannot be fetched directly
- [nnmclub.to](https://nnmclub.to)
- [Jackett](https://github.com/Jackett/Jackett) (Torznab API) - any indexer supported by your Jackett instance

**Commands:**

- `/get_active_tasks` - Retrieve tasks for monitoring
- `/ping` - Check if bot is running

### HTTP API

Manage tracking tasks programmatically via the REST API:

- `POST /api/files` - Create a new tracked download task from a tracker URL (enables update monitoring)
- `POST /api/downloads` - One-shot fire-and-forget download from a magnet or `.torrent` URL (not monitored, no history)
- `GET /api/files` - List all tracked tasks
- `DELETE /api/files/{fileId}` - Remove a tracked task
- `PATCH /api/files/{fileId}/refresh` - Force refresh a specific task
- `PATCH /api/files/refresh` - Force refresh all tasks
- `GET /api/file-locations` - Get available download locations
- `POST /api/file-locations` - Update download location for a task
- `GET /api/health` - Health check (see below)

Release watcher (see [Release watcher](#release-watcher)):

- `POST /api/watches` - Create a watch
- `GET /api/watches` - List watches
- `GET /api/watches/{watchId}` - One watch plus the releases it has already announced
- `PATCH /api/watches/{watchId}` - Edit queries/sources/regexes (increments `rev`)
- `DELETE /api/watches/{watchId}` - Soft delete (sets `disabled_at`)
- `POST /api/watches/{watchId}/search` - Re-run this watch's own search and return the full results
- `POST /api/search` - Ad-hoc search with caller-supplied queries, sources and regexes

**POST /api/files** - tracker URL only (parses the page, persists a row, monitors for updates):
```json
{"url": "https://rutracker.org/forum/viewtopic.php?t=6810475", "location": "/downloads/tv shows"}
```
A bare `magnet` is no longer accepted here (it cannot be monitored); use `POST /api/downloads` instead.

**POST /api/downloads** - one-shot download, handed straight to the download client. The `source` is
forwarded verbatim (qBittorrent fetches a `.torrent` URL or raises a magnet itself); nothing is parsed,
persisted, or monitored. `location` is optional and defaults to the client's configured location.
Responds `201 {"status":"ok"}`.

With a magnet:
```json
{"source": "magnet:?xt=urn:btih:...", "location": "/downloads/movies"}
```

With a Jackett `/dl/` `.torrent` URL:
```json
{"source": "https://jackett.example.com/dl/indexer/?jackett_apikey=...&path=...", "location": "/downloads/movies"}
```

**GET /api/health** - reports real service state, not a hardcoded string:
```json
{
  "status": "ok",
  "tracked": 42,
  "failing": 0,
  "last_run_at": "2026-08-10T12:00:00Z",
  "providers": {"rutracker": "ok", "nnm": "ok"},
  "watches": {"active": 1, "oldest_run_at": "2026-08-10T12:20:00Z", "with_errors": 0}
}
```

- `status` - `ok`, `degraded`, or `unhealthy`. Evaluated in order, first match wins:
  1. `unhealthy` (HTTP **503**) - any provider circuit breaker is tripped, or the last cron run is stale
     (older than twice the cron interval; before the first run the service start time is used instead)
  2. `degraded` (HTTP 200) - at least one tracked task is failing, or the watcher is in trouble: a watch
     cycle staler than twice the `WATCH_CRON` interval, an active watch carrying a non-empty
     `last_status`, or an active watch that has never run once the service has been up longer than that
     window. The watcher check can only move `ok` to `degraded`; it never lowers a verdict already
     reached above, and never reports `unhealthy` itself. Zero watches is `ok`, not degraded
  3. `ok` (HTTP 200)
- `tracked` - number of tracked tasks.
- `failing` - tasks with 3 or more consecutive sync failures. Fewer than 3 is a silent ramp-up and is not
  counted here.
- `last_run_at` - when the cron sweep last finished; omitted until the first run completes.
- `providers` - per-provider circuit breaker state, `ok` or `blocked`. Keys are the providers the service
  actually built, so `jackett` only appears when `JACKETT_URL` is set.
- `watches` - watcher state: `active` (not disabled, not expired), `oldest_run_at` (the oldest
  `last_run_at` across active watches, omitted while none has run) and `with_errors` (active watches
  whose `last_status` is non-empty).

Consumers should assert on `status`, not on the HTTP body text.

> Breaking change: `GET /api/health` no longer returns `{"count": N, "message": "OK"}`, and it no longer always
> answers HTTP 200 - an `unhealthy` status comes with a 503. Uptime monitors and container healthchecks that
> asserted on `message == "OK"` or on a 200-only contract must be updated to assert `status == "ok"`, and that
> change has to land together with the service deploy or the monitor will flap.

### Cron Jobs

Runs on the `CRON` schedule (hourly by default), checking for updates on tracked pages and initiating new download tasks if updates are found.

A tracker that returns a blocked response (HTTP 403/429 or a Cloudflare challenge) trips a per-provider
circuit breaker: its tasks are skipped without issuing requests, and a single half-open probe is retried
after a cooldown that doubles from 1h up to 24h. Tasks that have failed 3 times in a row are retried at
most once per 24 hours instead of every run. Both transitions are announced once in Telegram.

A second job runs on the `WATCH_CRON` schedule (`20 * * * *` by default, offset from the files sweep) and
drives the release watcher below.

## Release watcher

A **watch** is a saved hunt for a release that does not exist yet: a list of queries, a set of sources
(`jackett`, `extto`), an `include_regex`, an `exclude_regex`, and the set of releases it has already
announced. Every `WATCH_CRON` tick searches each source for each query, merges and dedups the results,
applies the two regexes, and diffs what is left against the seen set. A non-empty diff is published to
NATS (`tuclaw.releases.found.<watch_id>`) and mirrored to the admin Telegram channel.

The watcher **never downloads anything** — it notifies. Indexers only index the title, so verification and
the download decision stay with the consumer.

```bash
curl -X POST http://localhost:8080/api/watches -d '{
  "id": "one-night-only-en",
  "queries": ["One Night Only 2026"],
  "sources": ["jackett", "extto"],
  "include_regex": "(?i)one[ ._-]night[ ._-]only.*2026",
  "exclude_regex": "(?i)bee gees|def leppard|rupaul|concert",
  "expires_at": null
}'
```

- The id becomes a NATS subject token, so it must match `^[a-z0-9_-]+$`. At least one non-empty query is
  required, both regexes must compile, and source names must be known — each failure is a 400 with a
  specific message; a duplicate id is a 409.
- `sources` is optional and defaults to **every** known source (`jackett`, `extto`). The names are checked
  against a static list, not against what is actually running, so a source disabled at startup (no
  `JACKETT_API_KEY`, no `FLARESOLVERR_URL`) is still accepted here. Such a source reports
  `<name>: source is not configured` in `last_status` on every cycle; it does not hold the watch back.
- The **narrow query is the primary filter**; `include_regex` is the second and `exclude_regex` only a
  backstop. A broad query returns hundreds of junk rows that no regex reliably sorts out.
- The **first cycle seeds silently**: everything it matches is recorded without publishing, so a new watch
  cannot immediately announce releases that already existed. `GET /api/watches/{id}` returns those rows,
  so the seed is inspectable. A first cycle in which a source **failed** still records what it did match
  but leaves the watch unseeded, so the next clean cycle seeds again before anything is published. A
  source that is not configured at all does not count as a failure here — it never comes back on its own,
  and waiting for it would leave the watch silent forever.
- Publishing happens **before** marking seen. A crash between the two costs one duplicate notification;
  the reverse would lose the release permanently and silently.
- The NATS payload is
  `{"watch_id":…,"watch_rev":…,"found_at":…,"total":…,"matched":…,"new_total":…,"new":[{"source","id","title"}]}`.
  The `new` list is capped (and trimmed further to fit 8 KiB), while `new_total` is the size of the whole
  delta — every item of which is marked seen, so `new_total > len(new)` is the only way a consumer can
  tell that releases beyond the cap existed. `found_at` is RFC3339 in UTC.
- A source failure is never silence: it lands in the watch's `last_status` and turns `/api/health`
  `degraded`. A partial run can still publish what it did find.
- `DELETE` is a soft delete, and a watch past its `expires_at` is skipped and disabled on the next cycle.
  Both show up as a non-null `disabled_at` on `GET /api/watches`, and both are reversible: `POST` on the
  same id re-creates the watch with the new parameters and clears the flag. The `watch_seen` rows survive
  that, so a revived watch does not replay everything it already announced.

`POST /api/watches/{id}/search` re-runs the watch's **own** stored search and returns the full result set -
`page_url`, host-rewritten `download_url`, the resolved `magnet` for ext.to items, `seeders`,
`published_at`, and a `new` flag per item. `?raw=true` returns the pre-filter set instead, which is how
you answer "why was I woken with this junk" and "why was I *not* woken". The cron and this endpoint call
the same code over the same stored parameters, so what you see cannot drift from what woke you.
`POST /api/search` is the ad-hoc equivalent for parameters that are not saved as a watch.

```json
{"watch_id":"one-night-only-en","total":226,"matched":1,"raw":false,"errors":[],
 "items":[{"source":"jackett","id":"nnmclub.to/1883913","title":"…","page_url":"…","download_url":"…",
           "magnet":"","magnet_error":"","seeders":12,"published_at":"…","new":true}]}
```

- `total` counts the deduped pre-filter set and `matched` the post-filter one, so a short `items` list
  still tells you how much was thrown away. A non-empty `errors` means a source failed and the set is
  **partial** — do not read it as "nothing new".
- `magnet` is resolved on demand for `extto` rows only, and only for rows that passed the filters. A
  per-item failure sets `magnet_error` (`extto magnet resolver is not configured` when `FLARESOLVERR_URL`
  is unset) and never fails the request.
- `POST /api/search` answers with an empty `watch_id` and flags every matched item `new`, since an ad-hoc
  search has no seen set.

Both search endpoints are slow by nature: a cold Jackett search takes ~40s and a cold ext.to search ~80s
(Cloudflare challenge). A warm cookie and Jackett's own cache make a repeat within the hour near-instant.

**Idempotency is explicitly not guaranteed.** An id present in a NATS payload's `new` list but absent from
`POST /api/watches/{id}/search` means the release was taken down, and is not an error. Only *query
reproducibility* is guaranteed — a release can be pulled between the cron tick and the call, and Jackett's
result cache expires after 35 minutes.

## Database migrations

Migrations live in `app/migrations/` and are the **single source of truth** for the schema. The server
creates no tables; it verifies on startup that the expected columns are present and refuses to start
otherwise, so a half-migrated database fails loudly at boot instead of failing every read with
`no such column`.

### Deployments

Nothing to do by hand. `docker compose up -d` runs a one-shot `magnet-feed-sync-migrate` container that
applies whatever is pending and exits; the app only starts once it has exited 0
(`depends_on: condition: service_completed_successfully`). Re-running is free — already-applied migrations
are recorded in `gorp_migrations` and a no-op run exits 0 immediately. A failed migration stops the deploy
before the app starts rather than crash-looping it.

The migration runner is published as its own small image (`…/magnet-feed-sync-migrate`), built from the
same repository and tagged alongside the app on every release. The app image is unchanged and does not
contain the migration tooling.

### Local development

A bare `go run ./app` no longer creates its own schema, so apply the migrations first:

```bash
go run ./cmd/migrate    # same code path the deploy container uses
```

A database created by an older build of the server — `files` present, no `gorp_migrations` — is adopted on
the first run: the migrations its columns already satisfy are recorded rather than replayed, so nothing
needs deleting by hand.

`sql-migrate` is still available for everything the runner deliberately does not do — `down`, `status`, and
creating new migrations. It reads `dbconfig.yml`, which points at `app/migrations`:

```bash
make apply-migrations              # go run ./cmd/migrate
make new-migration name=add-thing  # sql-migrate new, lands in app/migrations/

go install github.com/rubenv/sql-migrate/...@latest  # needed for the two below
sql-migrate status
sql-migrate down
```

Do not use `sql-migrate up`. The CLI has no adoption step, so on an unmanaged database it records the
baseline and then fails on `DROP COLUMN rss_url` — and with the baseline recorded, the runner's adoption no
longer fires either, leaving a database no command can migrate. Use `go run ./cmd/migrate` to apply.

New migrations must go in `app/migrations/` — the runner embeds that directory with `go:embed`, and a file
placed anywhere else is silently not part of the image.

## Configuration

Configure the bot using the following environment variables:

- `QBITTORRENT_URL`: URL to your qBittorrent instance.
- `QBITTORRENT_USERNAME`: qBittorrent username.
- `QBITTORRENT_PASSWORD`: qBittorrent password.
- `QBITTORRENT_DESTINATION`: Default download location on qBittorrent.
- `TELEGRAM_TOKEN`: Telegram bot token.
- `TELEGRAM_SUPER_USERS`: Comma-separated list of Telegram user IDs allowed to manage the bot.
- `CRON`: cron expression for the update sweep, standard 5 fields (default `0 * * * *`). It also sets the health staleness window: a last run older than twice the schedule's longest gap between firings makes `/api/health` report `unhealthy`. An expression that cannot be parsed falls back to a 2h window and logs a warning.
- `WATCH_CRON`: cron expression for the release-watcher sweep, standard 5 fields (default `20 * * * *`, offset from `CRON` so the two sweeps never start together). It also sets the watch staleness window on `/api/health` in the same way `CRON` does, with the same 2h fallback and warning.
- `JACKETT_URL`: Jackett instance base URL (optional, enables Jackett/Torznab support).
- `JACKETT_API_KEY`: Jackett api key, required by the watcher's search (`JACKETT_URL` carries no key). Without it the Jackett watch source is disabled with a startup warning and the service still runs.
- `JACKETT_PUBLIC_URL`: publicly reachable Jackett base URL (defaults to `JACKETT_URL`). Jackett puts its own internal base URL in the download links it returns; the watcher rewrites the scheme and host of `download_url` to this value, otherwise the link resolves nowhere at download time.
- `NATS_URL`: JetStream endpoint for watch notifications, e.g. `nats://nats:4222`. Empty disables publishing — the service still starts and still runs cycles, but nothing is marked as announced, so releases are retried once publishing is configured. The stream is owned by its consumer; this service only publishes to it and never creates or reconfigures one. A failed connect is logged, not fatal.
- `FLARESOLVERR_URL`: FlareSolverr command endpoint including the `/v1` path, e.g. `https://flaresolverr.example.com/v1` (optional). RuTracker sits behind a Cloudflare challenge and is fetched through FlareSolverr; when this is empty the service still starts, but RuTracker pages are reported as blocked. NNM and Jackett are always fetched directly. The ext.to watch source uses the same solver for its cookie refresh, so an empty value also disables `extto`: a watch listing it records `extto: source is not configured` in `last_status` every cycle and `/api/health` stays `degraded`. Use `"sources": ["jackett"]` on such a deployment.

> Breaking change: the Synology DownloadStation client has been removed. qBittorrent is now the only supported download client. Remove any `DOWNLOAD_CLIENT` and `SYNOLOGY_*` variables from your environment.

## Contributors

To contribute to `magnet-feed-sync`, please fork the repository, create a feature branch, and submit a pull request.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
