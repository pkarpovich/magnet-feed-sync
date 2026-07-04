# One-shot Magnet / .torrent-URL Download (RAL-56)

## Overview

Add a fire-and-forget "download this now" mode to the magnet-feed-sync Go backend: accept a
magnet link OR an http(s) URL to a `.torrent` file (e.g. a Jackett `/dl/` download URL) and hand
it straight to qBittorrent, bypassing feed-monitoring logic entirely. No DB row, no infohash
extraction, no persistence.

**Problem it solves:** the download-agent (`download.py`, separate repo) finds releases on
trackers that magnet-feed-sync cannot parse (TPB / TorrentGalaxy / Kinozal, surfaced via Jackett).
Today it cannot enqueue them autonomously and falls back to handing magnets to the user by hand.
A magnet and a Jackett `/dl/` URL are self-contained, so the agent just needs a passthrough
endpoint that drops the source into qBittorrent.

**Why no file storage:** qBittorrent's WebUI API (`AddNewTorrentViaUrl`) accepts both a magnet and
an http(s) URL to a `.torrent` and downloads the file itself. So the source is always just a string
we forward — there is nothing to store. A Jackett `/dl/` URL embeds the Jackett `apikey` (a secret),
which is a second reason to keep this path fire-and-forget: the URL must never land in the DB or logs.

**Integration / semantic split (locked during brainstorm):**
- `POST /api/files {url}` — tracked: a provider parses the tracker page, we persist a row in `files`,
  the cron feed re-checks it for updates. Unchanged.
- `POST /api/downloads {source}` — NEW: one-shot fire-and-forget, no row, no monitoring.

The existing `{magnet}` branch of `POST /api/files` is removed and folded into the new path: a bare
magnet cannot be monitored anyway (no page to re-fetch — `processFileMetadata` already early-returns
on empty `OriginalUrl`), so persisting it as a "tracked" item was semantically wrong.

### Non-goals (v1)

- No raw-bytes `.torrent` upload (multipart) — sources are always a magnet or a fetchable URL.
- No server-side infohash resolution (downloading + bencode-parsing the `.torrent` to compute a hash).
- No DB persistence / history for one-shot downloads — fire-and-forget by design.
- No Telegram-bot changes (teaching the bot to accept bare magnets is a possible later follow-up).
- No `download.py` / download-agent changes — that lives in a different repo.

### Rejected alternatives

- **Persist one-shot downloads with a synthetic ID (Option B, brainstorm):** rejected — ID would not
  equal the infohash so the same release via two URLs yields duplicate rows (qBittorrent dedups by
  infohash regardless), and it would write the Jackett apikey secret into the DB.
- **Server resolves infohash from the `.torrent` (Option C):** rejected — needs a bencode parser and
  transient byte handling for no product value; qBittorrent already fetches and dedups.
- **New field on `POST /api/files` instead of a new endpoint:** rejected — `/files` are resources you
  GET/DELETE; a one-shot creates no resource, it is an action. A separate `/api/downloads` keeps REST
  semantics honest and avoids conflating "add to library" with "just download now".

## Skills to invoke

Load each skill below with the Skill tool and follow its conventions before implementing any task in this plan.

- `go` — signature / visibility / methods-vs-helpers / comment conventions for all Go code in this service.

## Context (from discovery)

- **Files/components involved:**
  - `app/bot/download-tasks/client.go` — core task client; holds `dClient`, `store`, `dryMode`, `mu`.
    Currently has `CreateFromMagnet` (client.go:85) and `createWithLock` (client.go:97).
  - `app/http/client.go` — HTTP layer. `TaskCreator` interface (client.go:21-28), `DownloadClient`
    interface (client.go:35-40, has `GetDefaultLocation()`), route registration in `Start` (client.go:63-73),
    `handleCreateFile` (client.go:161-216), `CreateFileRequest` struct (client.go:154-159).
  - `app/download-client/qbittorrent/client.go` — `CreateDownloadTask(url, destination)` →
    `AddNewTorrentViaUrl` (client.go:29-41). No change needed.
- **Related patterns found:**
  - Every handler opens an OTel span: `otel.Tracer("http").Start(r.Context(), "<METHOD /path>")` then
    `defer span.End()`; errors logged via `slog.ErrorContext(ctx, ...)`.
  - Handlers are thin and call `c.taskCreator.<Method>`; the download-tasks client owns `dClient`.
  - `utils.ExtractBtihHash(req.Magnet)` (client.go:191) is the ONLY `utils.` reference in the http file —
    removing the magnet branch makes the `magnet-feed-sync/app/utils` import unused (must be dropped).
- **Dependencies identified:** `stretchr/testify` for tests; HTTP handlers tested with `httptest` and
  hand-written mocks implementing the interfaces (`app/http/client_test.go`); download-tasks tested with
  a mock download client (`app/bot/download-tasks/client_test.go`).

## Development Approach

- **Testing approach:** Regular (implement, then tests within the same task) — matches the repo's
  colocated `*_test.go` convention.
- Complete each task fully before moving to the next; small, focused changes.
- **Every task includes new/updated tests** covering success AND error/edge scenarios.
- **All tests must pass before starting the next task.**
- Update this plan file if scope changes during implementation.
- Maintain backward compatibility for the `POST /api/files {url}` path (only the `{magnet}` branch is removed).

## Code-Quality Rules (verify before marking each task complete)

Lifted verbatim from the `go` skill's Hard rules. This is the per-task gate.

**Signatures:**
- No function or method has 4+ parameters; `ctx context.Context` does not count. Past the budget, use an options struct.
- No function or method has 4+ return values; split into single-purpose functions or return a struct.
- Adjacent same-type parameters are a swap hazard — put them on a struct.

**Methods vs standalone helpers:**
- If a function is called only from methods of a single struct, it MUST be a method on that struct. Calling pattern decides, not field access.
- Standalone helpers are only for constructors/entry points, utilities shared by multiple unrelated types, and tiny cross-cutting helpers.
- Before adding a standalone helper, walk its callers; if every caller is a method of one type, make it a method.

**Visibility (private by default):**
- Lowercase identifiers by default; export only when an out-of-package caller exists.
- Exception: a method called by other structs in the same package may be exported for inter-component API clarity — methods only.
- Before exporting a new identifier, grep for cross-package callers; if none, lowercase it.

**Comments (default: none):**
- Default to no comments; add one only when the WHY is non-obvious.
- Exported items get godoc comments starting with the name; unexported get a lowercase comment or none.
- Never describe WHAT self-evident code does.

**Per-task gate (before marking a checkbox `[x]`):**
1. `gofmt -s`/`goimports` clean, `go vet ./...` clean, `go test ./... -race` passes.
2. Grep new code: `grep -nE '^func.*\(.*,.*,.*,.*\)'` for 4+ params (excluding `ctx`); for each new standalone helper confirm a non-method caller; for each new exported identifier confirm a cross-package caller.
3. Only after 1-2 pass: mark complete.

Note: this repo has no `golangci-lint` config; substitute `go vet ./...`. `DownloadNow(ctx, source, location string)`
is ctx + 2 params — within the signature budget.

## Testing Strategy

- **Unit tests:** required for every task (see Development Approach). No e2e/UI harness in this repo,
  so backend unit tests only.
- The download client (`CreateDownloadTask` → real qBittorrent) is never hit in tests — tests exercise
  `DownloadNow` and the HTTP handler against mocks that record calls.

## Progress Tracking

- Mark completed items `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix; blockers with ⚠️ prefix.
- Keep this plan in sync with actual work.

## Solution Overview

Thin new HTTP endpoint delegating to a new one-line method on the existing task client:

`download.py` → `POST /api/downloads {source, location?}` → `handleCreateDownload` (validate + resolve
default location) → `taskCreator.DownloadNow(ctx, source, location)` → (unless dry-mode)
`dClient.CreateDownloadTask(source, location)` → qBittorrent fetches the `.torrent` / raises the magnet.

Key design decisions:
- **Passthrough, not parse:** the source string is forwarded verbatim; magnet-vs-URL is only a
  validation concern (qBittorrent handles both identically), so `DownloadNow` does not branch on it.
- **No persistence:** the new path never touches `store`, `mu`, or infohash — so the Jackett apikey
  secret never reaches the DB/logs, and there is no ID to invent.
- **Default location** resolved in the handler via `downloadClient.GetDefaultLocation()`, matching the
  behavior the removed magnet branch had.

## Technical Details

**New request shape (`POST /api/downloads`):**
- `source string` (required) — a magnet URI or an http(s) URL to a `.torrent`.
- `location string` (optional) — download destination; defaults to `downloadClient.GetDefaultLocation()`.

**Validation:** `source` non-empty AND begins with one of `magnet:`, `http://`, `https://` → else `400`.
Any other prefix → `400`. `DownloadNow` error → `500`. Success → `201`, body `{"status":"ok"}`.

**New method signature:** `func (c *Client) DownloadNow(ctx context.Context, source, location string) error`.
Dry-mode → log + return nil (do NOT call `dClient`); otherwise `return c.dClient.CreateDownloadTask(source, location)`.

**`TaskCreator` interface delta (`app/http/client.go`):** remove
`CreateFromMagnet(ctx, hash, magnet, name, location string) (*tracker.FileMetadata, error)`; add
`DownloadNow(ctx context.Context, source, location string) error`.

**`handleCreateFile` after edit:** only the `req.URL` path remains (provider parse + persist + monitor);
validation message becomes `"url is required"`; the `else` magnet branch (with `utils.ExtractBtihHash`
and `CreateFromMagnet`) is deleted; the now-unused `magnet-feed-sync/app/utils` import is removed.

**`CreateFileRequest`:** drop `Magnet` and `Name` fields (only used by the removed branch; confirm no
other reader — `FileMetadataResponse.Magnet`/`.Name` are populated from the store, not this struct, so
they stay).

## What Goes Where

- **Implementation Steps** (`[ ]`): all code + tests below live in this repo.
- **Post-Completion** (no checkboxes): manual smoke test against real qBittorrent, and the `download.py`
  integration (separate repo).

## Implementation Steps

### Task 1: Add `DownloadNow` to the download-tasks client and remove `CreateFromMagnet`

**Files:**
- Modify: `app/bot/download-tasks/client.go`
- Modify: `app/bot/download-tasks/client_test.go`

- [x] add method `DownloadNow(ctx context.Context, source, location string) error` on `*Client`:
      dry-mode → `slog.InfoContext(ctx, ...)` and return nil; otherwise return
      `c.dClient.CreateDownloadTask(source, location)`. No `store`, no `mu`, no hash.
- [x] remove the `CreateFromMagnet` method (client.go:85) — confirm via grep it has no remaining callers
      in the package before deleting (the http handler caller is removed in Task 2; if Task 2 is not yet
      done the build will break — do Task 1 and Task 2 as one compile unit, running the build after Task 2).
- [x] write test: `DownloadNow` in dry-mode does NOT invoke the mock download client and returns nil.
- [x] write test: `DownloadNow` in normal mode calls `dClient.CreateDownloadTask` exactly once with the
      exact `source` and `location` passed in, and propagates a returned error.
- [x] extend the existing mock download client in `client_test.go` if needed to record
      `CreateDownloadTask` call args; remove any test that exercised `CreateFromMagnet` (no such test in this package).
- [x] run `go test ./app/bot/download-tasks/... -race` — must pass (see Task 2 note re: cross-package build).

### Task 2: Add `POST /api/downloads` handler and drop the magnet branch from `POST /api/files`

**Files:**
- Modify: `app/http/client.go`
- Modify: `app/http/client_test.go`

- [x] `TaskCreator` interface: remove `CreateFromMagnet`; add
      `DownloadNow(ctx context.Context, source, location string) error`.
- [x] add `handleCreateDownload(w, r)`: open OTel span `POST /api/downloads`; decode
      `{source, location}`; validate `source` (non-empty + `magnet:`/`http://`/`https://` prefix) → `400`
      with a clear message; default `location` via `c.downloadClient.GetDefaultLocation()` when empty;
      call `c.taskCreator.DownloadNow(ctx, source, location)`; on error `slog.ErrorContext` + `500`;
      success → `201` with JSON `{"status":"ok"}`.
- [x] register route in `Start`: `mux.HandleFunc("POST /api/downloads", c.handleCreateDownload)`
      (POST already in the CORS allowed methods).
- [x] edit `handleCreateFile`: delete the `else` magnet branch; require `req.URL` only
      (message `"url is required"`); keep the provider/persist path intact.
- [x] remove `Magnet` and `Name` from `CreateFileRequest`; remove the now-unused
      `magnet-feed-sync/app/utils` import.
- [x] update the http test mock `TaskCreator`: drop `CreateFromMagnet`, add a `DownloadNow` that records
      args and returns a configurable error.
- [x] write tests for `POST /api/downloads`: `201` for a magnet source; `201` for an http(s) source;
      `400` for empty source; `400` for a garbage/non-magnet-non-http source; `500` when `DownloadNow`
      returns an error. Assert `DownloadNow` received the resolved location (default applied when omitted).
- [x] update/remove the existing `handleCreateFile` magnet-branch test; keep/adjust the `{url}` tests and
      the "url is required" validation test.
- [x] run `go build ./...` then `go test ./... -race` — must pass.

### Task 3: Verify acceptance criteria

- [ ] `POST /api/downloads {"source":"magnet:?xt=...","location":"/downloads/movies"}` returns `201`
      and (mock) forwards the source unchanged to the download client.
- [ ] a Jackett `/dl/...` http(s) source is accepted and forwarded verbatim (no parsing, no hash, no
      DB write).
- [ ] `POST /api/files {"url": <tracker url>}` still parses, persists, and is monitored (unchanged);
      `POST /api/files {"magnet": ...}` no longer exists (returns `400 "url is required"`).
- [ ] grep confirms no lingering `CreateFromMagnet` reference and no unused `utils` import in
      `app/http/client.go`.
- [ ] run full suite: `go build ./... && go test ./... -race`.
- [ ] apply the Code-Quality per-task gate (gofmt/goimports, `go vet ./...`, signature/visibility greps).

### Task 4: Documentation and plan close-out

**Files:**
- Modify: `README.md`
- Modify: `CLAUDE.md` (only if a new pattern is worth recording)

- [ ] add `POST /api/downloads` to the README HTTP API section with a magnet and a Jackett `/dl/` example;
      note it is one-shot / not monitored, and that `POST /api/files` no longer accepts `magnet`.
- [ ] update `CLAUDE.md` only if the tracked-vs-one-shot split is worth codifying for future work.
- [ ] move this plan to `docs/plans/completed/`.

## Post-Completion

*Items requiring manual intervention or external systems — informational only.*

**Manual verification:**
- Smoke test against the real qBittorrent instance: one magnet and one live Jackett `/dl/` URL, confirm
  both land in qBittorrent at the expected location and no row appears in `GET /api/files`.
- Confirm the Jackett apikey never appears in application logs after a `/dl/` download.

**External system updates:**
- `download.py` / download-agent (separate repo): call `POST /api/downloads` with `{source, location}`
  for autonomous enqueue. This is the actual RAL-56 client-side unlock and is tracked outside this repo.
