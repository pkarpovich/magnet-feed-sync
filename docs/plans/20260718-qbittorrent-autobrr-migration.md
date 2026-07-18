# qBittorrent client migration to autobrr/go-qbittorrent

## Overview

Migrate the qBittorrent download client off the frozen fork
`github.com/pkarpovich/go-qbittorrent-apiv2` (pinned via a `replace` to a 2024 commit) onto the
actively-maintained `github.com/autobrr/go-qbittorrent`. In the same change, remove the unused Synology
DownloadStation client and refactor the download-client subsystem from a provider-side fat interface to
Go-idiomatic consumer-side interfaces.

**Problem it solves:** qBittorrent was upgraded to v5.2.3 (WebAPI 2.15.1). Its 5.2.0 release changed two
response conventions:
- `auth/login` now returns `204 No Content` + empty body (was `200 OK` + body `"Ok."`).
- `torrents/add` now returns `200 OK` + JSON `{"added_torrent_ids":[...],"success_count":1}` (was `"Ok."`).

The frozen fork hard-codes the old contract (`RespOk` requires the status string to equal exactly
`"200 OK"`; `RespBodyOk` requires the body to equal exactly `"Ok."`), so it rejects the *successful* 204
login inside `NewCli` and every download fails with HTTP 500 `failed to create download`. Reproduced live
against the qBittorrent instance configured in `.env`.

**Key benefit:** the failure class is "unmaintained code vs a moving API." A maintained library that tracks
qBittorrent releases is the durable fix (it already accepts `200`+`204` on login and auto-relogins on 403),
and consolidating on a single qBittorrent-only client lets us delete dead Synology code and tidy the
interface layer at the same time.

### Non-goals

- Do NOT move the qbittorrent package directory (keep it at `app/download-client/qbittorrent`). Renaming is
  cosmetic import churn with no functional benefit.
- Do NOT change the HTTP `/api` contract or touch the frontend.
- Do NOT introduce session caching, retry tuning, or extra qBittorrent features (tags, categories,
  pause/resume, stats). The app uses exactly three operations; keep it that way.
- Do NOT touch unrelated repo smells (the database `withRetry` error-string matching, the RuTracker/NNM
  Russian date parsing, etc.). Out of scope for this migration.

### Rejected alternatives

- **Zero-dependency raw `net/http` client** (Option B). Rejected: it re-creates the exact failure we are
  fixing — hand-rolled code frozen against a moving qBittorrent API. The next API change breaks us again.
- **Patch the existing fork** (`pkarpovich/go-qbittorrent-apiv2`, Option C). Rejected: keeps a whole fork
  alive for three calls, work lives in a second repo, and it stays frozen against future changes unless
  hand-patched each time.

## Skills to invoke

Load each skill below with the Skill tool and follow its conventions before implementing any task in this
plan.

- `go` — signature / visibility / structure / interface conventions for all Go code changed in this plan
  (consumer-side interfaces, accept-interfaces-return-concrete, methods-vs-helpers, private-by-default).

## Context (from discovery)

- **Project:** Go 1.25 Telegram-bot + HTTP service (`/app`) that turns tracker pages / magnets into
  qBittorrent (or, until now, Synology) download tasks. Pure-Go SQLite. React frontend (untouched here).
- **Files/components involved:**
  - `app/download-client/qbittorrent/client.go` — the client being rewritten.
  - `app/download-client/client.go` — provider-side fat `Client` interface + factory switch (to be deleted).
  - `app/download-client/download-station/` — Synology client (to be deleted).
  - `app/main.go` — composition root, constructs the client via the factory.
  - `app/tracker/parser.go`, `app/bot/download-tasks/client.go`, `app/http/client.go` — the three consumers.
  - `app/config/config.go`, `compose.yaml` — Synology/`DOWNLOAD_CLIENT` config to remove.
  - `go.mod` / `go.sum` — dependency swap.
  - `README.md`, `CLAUDE.md` — docs.
- **Consumer usage map (grep-verified):**
  - `tracker.Parser` uses `GetDefaultLocation()` only (`app/tracker/parser.go:55`).
  - `download_tasks.Client` uses `CreateDownloadTask()` only (`app/bot/download-tasks/client.go:91,116,215`).
  - `http.Client` uses `GetDefaultLocation`, `GetLocations`, `GetHashByMagnet`, `SetLocation`
    (`app/http/client.go:215,284,323,330`).
- **Patterns found:** consumer-side interfaces already used for other deps (`FileParser`, `FileStore` in
  download-tasks; `TaskCreator`/`FileStore`/`DownloadClient` in http). The download client is the one dep
  still using a provider-side interface — this migration makes it consistent.
- **Dependency facts (grep-verified):** only `app/download-client/qbittorrent/client.go` imports the old
  fork. Synology (`download-station`) uses its own raw HTTP and imports `config.SynologyConfig`.

## Development Approach

- **Testing approach:** Regular (code-first, tests written in the same task). We are modifying
  already-tested code; each task updates/adds tests and leaves `go test ./...` green before the next task.
- Complete each task fully before moving to the next; keep the build green at every task boundary.
- **Every task MUST include new/updated tests** for the code it changes (success + error/edge cases), listed
  as separate checklist items.
- **All tests must pass before starting the next task.**
- Update this plan file if scope changes during implementation (`➕` new task, `⚠️` blocker).

## Code-Quality Rules (verify before marking each task complete)

Materialized verbatim from the `go` skill's Hard rules. This is the per-task gate — a fresh task session
must verify against it before marking any `[x]`.

### Go (from the `go` skill)

**Signatures:**
- No function or method has 4+ parameters; `ctx context.Context` does not count. Past the budget, use an
  options struct (`type fooOpts struct { ... }`).
- No function or method has 4+ return values; split into single-purpose functions or return a struct.
- Adjacent same-type parameters (`oldLine, newLine int`) are a swap hazard - put them on a struct.

**Methods vs standalone helpers:**
- If a function is called only from methods of a single struct, it MUST be a method on that struct. Calling
  pattern decides, not field access.
- Standalone helpers are only for: constructors/entry points (`New...`, `Parse...`, `Decorate...`),
  utilities shared by multiple unrelated types, and tiny cross-cutting helpers.
- Before adding a standalone helper, walk its callers; if every caller is a method of one type, make it a
  method.

**Visibility (private by default):**
- Lowercase identifiers by default; export only when an out-of-package caller exists.
- Exception (per CLAUDE.md): a method called by other structs in the same package may be exported for
  inter-component API clarity - methods only, not types, functions, constants, or variables.
- Before exporting a new identifier, grep for cross-package callers; if none, lowercase it.

**Comments (default: none):**
- Default to no comments; add one only when the WHY is non-obvious (a hidden invariant, a workaround,
  surprising behavior).
- Exported items get godoc comments starting with the name; unexported get a lowercase comment or none.
- Never describe WHAT self-evident code does; no multi-paragraph comments on routine helpers.

**Per-task gate (before marking a checkbox `[x]`):**
1. `gofmt -s`/`goimports` clean, `golangci-lint run` zero issues (if available), `go vet ./...`,
   `go build ./...`, `go test ./... -race` all pass.
2. Grep new code for the rules above: `grep -nE '^func.*\(.*,.*,.*,.*\)'` for 4+ params (excluding `ctx`);
   for each new standalone helper confirm a non-method caller; for each new exported identifier confirm a
   cross-package caller.
3. Only after 1-2 pass: mark complete.

Note on CI parity: the repo CI runs only `go build ./...` + `go test ./...` (no gofmt/vet), so run
`gofmt -s -l` and `go vet ./...` locally as part of the gate.

## Solution Overview

The concrete `*qbittorrent.Client` becomes the single download-client implementation, constructed once in
`main.go` (the composition root) and injected into three consumers, each of which declares a small local
interface of only the methods it uses. The download-client wrapper package (fat interface + factory switch)
disappears, and Synology is deleted.

The qBittorrent client holds one reused `*qbt.Client` from autobrr. autobrr establishes the session lazily
(first request without a session gets 403, autobrr's request wrapper re-logs-in and retries), so the
constructor does no network I/O and app startup stays decoupled from qBittorrent availability. Torrent hash
lookup switches from magnet-string comparison to BTIH-hash comparison (reusing `utils.ExtractBtihHash`,
consistent with the existing `magnetsEqual` in download-tasks), which lets us delete the `removeDnFromMagnet`
helper.

## Technical Details

### autobrr API contract (this plan is the source of truth — do not re-audit upstream)

- `qbt.NewClient(qbt.Config{Host, Username, Password}) *qbt.Client` — no network I/O, no auto-login.
  `Host` is the base URL (e.g. the value of `QBITTORRENT_URL`); autobrr appends `/api/v2/` internally.
- Login is lazy: a request with no session returns 403; autobrr's HTTP wrapper calls `LoginCtx` and retries.
  Its `Login` accepts both `200 OK` and `204 No Content` (handles the 5.2.0 break).
- `AddTorrentFromUrl(url string, options map[string]string) (*TorrentAddResponse, error)` — set
  `options["savepath"] = destination`; autobrr sets `options["urls"] = url` internally and posts
  `torrents/add`, accepting `200/202/204`. Savepath is passed verbatim (no `filepath.Abs` munging the old
  lib did; our paths are already absolute like `/downloads/movies`).
- `GetTorrents(qbt.TorrentFilterOptions{}) ([]qbt.Torrent, error)`; `qbt.Torrent` has fields `Hash` and
  `MagnetURI` (`json:"magnet_uri"`).
- `SetLocation(hashes []string, location string) error` — note `hashes` is a slice and the arg order is
  `(hashes, location)`, the reverse of the old lib's `(location, taskID)`.

### qBittorrent client method mapping (contract — bodies born during execution)

- `NewClient(cfg config.QBittorrentConfig) *Client` — builds `qbt.NewClient(qbt.Config{Host: cfg.URL,
  Username: cfg.Username, Password: cfg.Password})`; stores the client + `cfg.Destination`. No login call.
- `CreateDownloadTask(url, destination string) error` → `AddTorrentFromUrl(url, {"savepath": destination})`,
  discard the response, wrap the error (`fmt.Errorf("add torrent: %w", err)`).
- `GetHashByMagnet(magnet string) (string, error)` → `GetTorrents({})`, match a torrent whose
  `utils.ExtractBtihHash(t.MagnetURI)` equals `utils.ExtractBtihHash(magnet)` (case-insensitive on the
  extracted hash), return `t.Hash`; `"torrent not found"` otherwise. `removeDnFromMagnet` is deleted.
- `SetLocation(taskID, location string) error` → `SetLocation([]string{taskID}, location)`.
- `GetLocations() []types.Location` and `GetDefaultLocation() string` — unchanged.

### Consumer-side interface signatures (locked contracts)

- `app/tracker/parser.go`: `type DownloadClient interface { GetDefaultLocation() string }`.
- `app/bot/download-tasks/client.go`:
  `type DownloadClient interface { CreateDownloadTask(url, destination string) error }`.
- `app/http/client.go`: ensure the existing local `DownloadClient` interface declares exactly:
  `GetDefaultLocation() string`, `GetLocations() []types.Location`,
  `GetHashByMagnet(magnet string) (string, error)`, `SetLocation(taskID, location string) error`.

Each consumer drops its `magnet-feed-sync/app/download-client` import. `main.go` constructs
`*qbittorrent.Client` directly and injects it (the concrete type satisfies all three narrow interfaces).

## Testing Strategy

- **Unit tests:** required per task. New `app/download-client/qbittorrent/client_test.go` uses
  `httptest.NewServer` to fake a qBittorrent WebAPI (login route returns `204`; assert `torrents/add`
  receives the url + savepath and success is accepted for a `200`+JSON body; assert `GetHashByMagnet`
  matches by BTIH hash; assert `SetLocation` posts hashes+location). Point `qbt.Config.Host` at the test
  server URL. Table-driven with `testify` (`assert`/`require`), one `_test.go` per source file, same package.
- Existing suites (`app/bot/download-tasks`, `app/http`, `app/tracker`, `app/config`) must stay green; mocks
  are supersets of the narrowed interfaces so they keep satisfying them — trim only if it stays clean.
- **e2e:** no automated e2e harness in this repo; live acceptance verification is a manual task (see Task 4
  and Post-Completion).

## Progress Tracking

- mark completed items `[x]` immediately when done.
- add newly discovered tasks with `➕`; document blockers with `⚠️`.
- keep this plan in sync with the actual work.

## What Goes Where

- **Implementation Steps** (`[ ]`): code, tests, config, docs changes achievable in this repo.
- **Post-Completion** (no checkboxes): the live end-to-end verification against the real qBittorrent, which
  requires the running service + a real qBittorrent instance and manual cleanup.

## Implementation Steps

### Task 1: Swap dependency and rewrite the qBittorrent client on autobrr

**Files:**
- Modify: `go.mod`, `go.sum` (add `github.com/autobrr/go-qbittorrent`; remove the old
  `github.com/NullpointerW/go-qbittorrent-apiv2` require and the `replace` directive)
- Modify: `app/download-client/qbittorrent/client.go`
- Create: `app/download-client/qbittorrent/client_test.go`

- [x] `go get github.com/autobrr/go-qbittorrent@latest`.
- [x] rewrite `client.go` per the method mapping in Technical Details: one reused `*qbt.Client` built in
      `NewClient` (no login call); `CreateDownloadTask` via `AddTorrentFromUrl` with `savepath`;
      `GetHashByMagnet` via `GetTorrents` + `utils.ExtractBtihHash`; `SetLocation` via slice+location;
      `GetLocations`/`GetDefaultLocation` unchanged; delete `removeDnFromMagnet`.
- [x] keep the exported method set identical to the current `Client` so it still satisfies the existing
      `download_client.Client` interface (that interface is removed in Task 2, not here — build stays green).
- [x] remove the old require + the `replace` directive from `go.mod`; run `go mod tidy`; confirm the old
      fork is gone from `go.mod`/`go.sum`.
- [x] write `client_test.go` with an `httptest` fake qBittorrent: `CreateDownloadTask` posts url+savepath and
      accepts a `200`+JSON success body; login route returns `204`.
- [x] write test cases for `GetHashByMagnet` (BTIH match success + not-found error) and `SetLocation`
      (posts hashes+location; error path).
- [x] run `gofmt -s -l`, `go vet ./...`, `go build ./...`, `go test ./... -race` — must pass before Task 2.

### Task 2: Consumer-side interfaces and composition root

**Files:**
- Delete: `app/download-client/client.go`
- Modify: `app/main.go`
- Modify: `app/tracker/parser.go`
- Modify: `app/bot/download-tasks/client.go`
- Modify: `app/http/client.go`
- Modify: `app/tracker/parser_test.go`, `app/bot/download-tasks/client_test.go`, `app/http/client_test.go`
  (only if mock trimming is needed to compile)

- [x] delete `app/download-client/client.go` (fat `Client` interface, factory switch, client-type
      constants).
- [x] add the three local `DownloadClient` interfaces per the locked signatures in Technical Details; update
      each consumer's field/param type and drop the `app/download-client` import.
- [x] update `app/main.go` to construct `qbittorrent.NewClient(cfg.QBittorrent)` directly and inject the
      concrete `*qbittorrent.Client` into `NewParser`, `download_tasks.NewClient`, and `http.NewClient`;
      remove the now-obsolete `dClient, err := downloadClient.NewClient(...)` error branch.
- [x] confirm each mock in the three `_test.go` files still satisfies its narrowed interface (supersets are
      fine); trim only if needed to keep tests compiling cleanly.
- [x] run the per-task gate (`gofmt -s -l`, `go vet ./...`, `go build ./...`, `go test ./... -race`) — must
      pass before Task 3.

### Task 3: Remove the Synology client and its config

**Files:**
- Delete: `app/download-client/download-station/` (whole directory)
- Modify: `app/config/config.go`
- Modify: `app/config/config_test.go` (if it references removed fields)
- Modify: `compose.yaml`

- [x] delete the `app/download-client/download-station/` directory.
- [x] remove `SynologyConfig`, the `SYNOLOGY_*` fields, and the `DownloadClient`/`DOWNLOAD_CLIENT` field
      (with its default) from `config.go`; keep `QBittorrentConfig`, Telegram, Http, Jackett, DryMode, Cron,
      Otel*, LokiURL.
- [x] remove the `SYNOLOGY_URL/USERNAME/PASSWORD/DESTINATION` and `DOWNLOAD_CLIENT` env entries from
      `compose.yaml`; keep `QBITTORRENT_*`.
- [x] update `config_test.go` to drop assertions on removed fields; add/keep a case asserting
      `QBittorrentConfig` still loads from env.
- [x] run the per-task gate — must pass before Task 4.

### Task 4: Verify acceptance criteria

- [ ] verify all requirements from Overview are implemented (autobrr in use, old fork + `replace` gone,
      Synology removed, consumer-side interfaces in place).
- [ ] run the full suite: `go test ./... -race` (from repo root).
- [ ] run `gofmt -s -l .` (expect no output) and `go vet ./...` (expect clean).
- [ ] confirm the grep checks from the Code-Quality gate pass for all new/changed code.
- [ ] perform the live end-to-end verification described in Post-Completion (bare magnet, Jackett `.torrent`
      URL, tracked `/api/files`, web-UI location change) against the real qBittorrent, then clean up any test
      torrents.

### Task 5: Update documentation and close out

**Files:**
- Modify: `README.md`
- Modify: `CLAUDE.md`

- [ ] update `README.md`: remove `DOWNLOAD_CLIENT` + `SYNOLOGY_*` from Configuration; state qBittorrent is
      the only download client; keep the `/api/files` vs `/api/downloads` description.
- [ ] update `CLAUDE.md`: drop the `synology`/DownloadStation and `DOWNLOAD_CLIENT` references in
      Architecture + Configuration; note the `autobrr/go-qbittorrent` dependency; keep the two-endpoint
      download split note.
- [ ] move this plan to `docs/plans/completed/`.

## Post-Completion

*Items requiring the running service + a real qBittorrent instance — no checkboxes, informational.*

**Live acceptance verification** (anchor on the real user flow; qBittorrent v5.2.3 from `.env`):

1. `POST /api/downloads` bare magnet
   `{"source":"magnet:?xt=urn:btih:2566E2B012EA1EF9087465BC97A7AC4449F4F0DE","location":"/downloads/movies"}`
   → expect `201 {"status":"ok"}` and the torrent present in qBittorrent under `/downloads/movies`.
2. `POST /api/downloads` with a Jackett `/dl/...` `.torrent` URL → expect `201` and the torrent added.
3. `POST /api/files` with a supported tracker URL → parsed, added, row persisted (`GET /api/files` shows it).
4. Web-UI "change location" path → `GetHashByMagnet` + `SetLocation` succeed (relocates in qBittorrent).
5. Clean up any test torrents afterward (`torrents/delete`, `deleteFiles=false`).

**Risks / operational notes:**
- ⚠️ The Telegram listener panics on a `409 Conflict` if a second poller runs with the same bot token. Do
  the live verification with a single running instance (reuse the already-running local instance on `:8080`,
  or build/run with the bot disabled) — do not start a second full instance alongside it.
- The savepath must be a path qBittorrent is allowed to write to; this is qBittorrent-side config and
  unchanged by this migration.
