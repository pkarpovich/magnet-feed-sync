# Error handling, real health check, and the RuTracker fix

## Overview

Three connected problems, fixed together because each one is why the other went unnoticed:

1. **RuTracker has been broken for two weeks and nobody knew.** It moved behind a Cloudflare managed
   challenge; every hourly cron run fired ~23 doomed requests (~7700 wasted requests total).
2. **The service could not tell anyone.** `/api/health` returns a hardcoded `"OK"`, so the external
   uptime monitor — which polls it every minute and alerts to Telegram — stayed green the whole time.
3. **The logs could not explain it.** Error text is serialized as `{}`, log lines carry no task id, and
   the fetcher sends no User-Agent.

The fix introduces a real error model (typed provider errors, a per-provider circuit breaker, per-task
failure counters), makes health report the truth, notifies Telegram on state transitions, and routes
RuTracker through the already-deployed FlareSolverr.

### Non-goals

- Do NOT rewrite the RuTracker/NNM HTML parsing — the selectors work once the page is actually fetched.
- Do NOT change the `/api/files` vs `/api/downloads` contract, and do NOT touch the frontend (it does
  not consume `/api/health` — grep-verified).
- Do NOT auto-pause dead tasks. Explicitly rejected: rare and unproven in the data.
- Do NOT move NNM or Jackett to the solver. NNM returns 200 directly (verified); Jackett is a local API.
- Do NOT make the service fail to start when the solver is unconfigured — it must degrade, not die.
- Do NOT touch unrelated smells (SQLite busy-retry string match, Russian date parsing, NNM's
  `gofeed.ParseURL` path). **In particular, do NOT reformat the four pre-existing `gofmt -s` failures
  listed in the Code-Quality section.**
- Do NOT persist the error *kind* as its own column.
- Do NOT expose the new failure fields in any JSON payload (see the `MetadataToMsg` trap).

### Rejected alternatives

- **Solver as a 403-fallback instead of always-on.** Rejected: with a warm session a fetch costs ~2.4s.
- **A `NotFound` error class.** Dropped: 30 days of data show no recurring per-item permanent failures.
- **A `status` column in the DB.** Rejected: status is derived, so it cannot contradict the counters.
- **Session-recreation logic in the solver client.** Rejected after live verification: FlareSolverr does
  not error on a missing session, so there is nothing to detect.
- **`golangci-lint` in the per-task gate.** Rejected: no `.golangci.yml`, CI does not run it, so it would
  make the gate host-dependent.
- **A whole-repo `gofmt -s -l .` gate.** Rejected: four files fail it on master already; a whole-repo
  gate would be unpassable from Task 1 and would force reformatting unrelated files.

## Skills to invoke

The `Code-Quality Rules` section below is the **complete and sole** acceptance bar for style. If the `go`
skill is available, loading it is optional context; if not, proceed — it is not a blocker, and a reviewer
MUST NOT fail a task on any rule not written in that section.

## Context (from discovery)

**Root cause (verified live — do not re-investigate).** `GET https://rutracker.org/forum/viewtopic.php?t=6810475`
returns `403` with `cf-mitigated: challenge`, `server: cloudflare`, body "Just a moment...". Not a
User-Agent problem — Go-client UA, no UA, and a browser UA all return 403. Deterministic and selective:
`/forum/index.php` returns 200 every time, `viewtopic.php` 403 every time. NNM is unaffected (200).

**Log evidence (already summarized — do not re-query Grafana/Loki).** Errors jumped 3/hour → 23/hour on
2026-07-27 between 12:00 and 15:00 UTC and stayed there. The only recurring background error in 30 days
is `msg="error parsing metadata"` (8639 lines). An unexplained 3/hour baseline predates Jul 27; the tasks
are unidentifiable because log lines carry no id. Task 1 fixes that.

**Verified repo facts (checked against the checkout — rely on these, they are current):**

| fact | value |
|---|---|
| `gofmt -s -l .` on master | fails on exactly 4 files: `app/tracker/integration_test.go`, `app/tracker/parser_test.go`, `app/tracker/providers/jackett.go`, `app/tracker/providers/jackett_test.go` |
| `NewClient(` call sites in `app/http/client_test.go` | **14** |
| `c.mu.Lock()` sites in `app/bot/download-tasks/client.go` | **6**; `c.tracker.Parse` is already OUTSIDE the mutex |
| health constant | `app/http/client.go:363` is `Message: "OK",` (capital M) |
| `MetadataToMsg` | `json.MarshalIndent` of the whole `FileMetadata` → any new tagged field leaks into every Telegram "Metadata updated" message |
| `app/task-store/repository_test.go` | **does not exist** — there is no DB-backed test anywhere in the repo |
| `database.openDB` | hardcodes `.db/<filename>` relative to the process CWD; no in-memory option |
| `CreateOrReplace` | `INSERT OR REPLACE` over 9 columns |
| `messagesForSend` | **unbuffered** (`make(chan string)`) |
| test files implementing `providers.Provider` | `app/tracker/parser_test.go`, `app/tracker/integration_test.go`, `app/tracker/providers/jackett_test.go`, `app/tracker/providers/rutracker_test.go` |
| provider construction sites in tests | 18 |
| `robfig/cron/v3` | present in `go.mod` as indirect (via gocron); this plan promotes it to direct |
| `sql-migrate` | NOT on PATH; `make new-migration` would `go install ...@latest` (network) — see Task 5 |

**Existing infrastructure to reuse:**
- The uptime monitor polls `/api/health` every minute and alerts to Telegram, asserting on the response
  body. Its config lives in a **separate repository on another host** — see Post-Completion; nothing here
  blocks on it.
- In-app Telegram channel: `messagesForSend` → `SendMessagesForAdmins`.
- FlareSolverr is deployed and proven: v3.5.0, solves the RuTracker challenge, returns HTML containing
  `a.magnet-link` and `a#topic-title`.
- Cron: `CRON` env, default `0 * * * *`, single job = `CheckForUpdates`.

**Measured FlareSolverr performance (n≥3 — do not re-measure):** cold solve on a fresh session
74.4/71.8/75.5s → **~74s**; request in a warm session 2.9/2.1/2.4/1.7/2.7s → **~2.4s**; `sessions.create`
2.2/3.2s. For 23 tasks a run costs ~74s + 22×2.4s ≈ **2 minutes**; without session reuse ~28 minutes, so
**session reuse is mandatory**.

## Development Approach

- **Testing approach:** Regular (code first, tests in the same task), matching the repo's existing style.
- Complete each task fully before the next; keep the build green at every task boundary.
- **Every task that changes Go code under `app/` MUST include new/updated tests** (success + error cases)
  as separate checklist items. **Tasks 9 and 10 are verification/documentation only and are exempt.**
- **All tests must pass before starting the next task.**
- Update this plan when scope changes (`➕` new task, `⚠️` blocker).

## Code-Quality Rules (verify before marking each task complete)

This section is the complete style bar for this plan.

**Signatures:**
- No function or method has 4+ parameters; `ctx context.Context` does not count. Past the budget, use an
  options struct.
- No function or method has 4+ return values.
- Adjacent same-type parameters are a swap hazard — put them on a struct.
- **Exception: signatures written verbatim in Technical Details are pre-approved. A reviewer MUST NOT
  fail a task for a signature this plan prescribes.**

**Methods vs standalone helpers:** if a function is called only from methods of a single struct, it MUST
be a method on that struct. Standalone helpers are only for constructors/entry points, utilities shared
by unrelated types, and tiny cross-cutting helpers.

**Visibility:** lowercase by default; export only when an out-of-package caller exists. A method called
by other structs in the same package may be exported for clarity — **methods are exempt from the
cross-package-caller check**.

**Errors:** wrap with context `fmt.Errorf("load config: %w", err)` — lowercase, no trailing punctuation,
wrap once at the boundary. Return errors, never panic for expected failures; never discard with `_`.

**Interfaces:** define at the **consumer** side with only the methods that consumer calls; accept
interfaces, return concrete types; inject the concrete type from `main.go`.

**Comments:** default none; add one only when the WHY is non-obvious. Exported items get godoc comments
starting with the name.

**Per-task gate (before marking a checkbox `[x]`) — these five checks are the entire mechanical gate; no
other linter is required or consulted:**

1. `CHANGED=$(git diff --name-only $(git merge-base HEAD origin/HEAD)...HEAD -- '*.go')`; if that command
   fails (no `origin/HEAD`, shallow clone), fall back to the union of the `Files:` lists of the tasks
   completed so far.
2. `gofmt -s -l $CHANGED` prints nothing. **A whole-repo `gofmt -s -l .` is expected to print exactly the
   four pre-existing files listed in Context — do not reformat them.**
3. `go vet ./...`, `go build ./...`, `go test ./... -race` all exit 0.
4. `grep -nE '^func.*\(.*,.*,.*,.*\)' $CHANGED` — a hit is a violation **only if** the parameter list
   excluding `ctx context.Context` has 4+ entries **and** the signature is not one prescribed verbatim in
   Technical Details. Zero violations = pass.
5. For each **non-method** identifier added in this task starting with an uppercase letter, run
   `grep -rn "<pkg>\.<Name>" app/ --include="*.go" | grep -v "^app/<pkg>/"` — at least one hit required.
   Exported methods are exempt.

## Solution Overview

The failure of a sync becomes **data** instead of a discarded side effect. One source of truth (per-task
counters in the DB + in-memory provider breaker state) feeds four consumers: the health endpoint,
Telegram notifications, the retry/backoff policy, and the logs.

The request path becomes pluggable: providers depend on a `Fetcher` interface, so `main.go` decides that
RuTracker goes through FlareSolverr while NNM and Jackett go direct. The fetcher is the single place that
knows HTTP status codes, so it is where transport errors get classified.

Two monitoring levels: the **uptime monitor** answers "is the service OK?", the **bot** answers "what
exactly broke?".

## Technical Details

### Fixed constants (single source of truth — no task invents its own)

| constant | value | where | meaning |
|---|---|---|---|
| `FailureThreshold` | `3` | **exported** from `app/bot/download-tasks` | a task is *failing* at `consecutive_failures >= 3`; gates the 24h stretch, the ok→failing notification, and the health `failing` count. Exported because `main.go` passes it into the HTTP client. |
| `deadTaskInterval` | `24h` | `app/bot/download-tasks/client.go` | a failing task is retried at most once per this period |
| breaker cooldowns | `1h, 2h, 4h, 8h, 16h, 24h` | `app/tracker/breaker.go` | doubles per failed probe, capped at 24h, resets to 1h on success |
| `solverHTTPTimeout` | `180s` | `app/tracker/providers/solver.go` | must exceed the measured ~74s cold solve and the 120s `maxTimeout` |
| `solverMaxTimeout` | `120000` (ms) | `app/tracker/providers/solver.go` | value sent to FlareSolverr |
| `directUserAgent` | `Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36` | `app/tracker/providers/fetcher.go` | unexported; tests assert equality |
| `staleRunFallback` | `2h` | `app/main.go` | used when the cron expression cannot be parsed |

**Definition of "failing" (used in exactly this sense everywhere):** a task is failing when
`consecutive_failures >= FailureThreshold`. `consecutive_failures` between 1 and 2 is a ramp-up: not
failing, not notified, not counted in health.

### Error model

```go
type ErrorKind int   // KindTransient | KindBlocked | KindPermanent

func (k ErrorKind) String() string   // exactly "Transient" | "Blocked" | "Permanent"

type ProviderError struct {
    Kind ErrorKind
    Err  error
}
```
`ProviderError` implements `Error() string` and `Unwrap() error` so `errors.As`/`errors.Is` work.
`last_error` stores `fmt.Sprintf("%s: %s", pe.Kind, pe.Err)`, e.g. `Blocked: flaresolverr not configured`.

**Classification is split across two layers; both must be implemented.**

*Transport (`directFetcher`), by status first:*
- `403` / `429` → `KindBlocked` (regardless of body)
- `5xx`, `context.DeadlineExceeded`, net errors → `KindTransient`
- `404` → `KindPermanent`
- any other non-200 → `KindTransient`
- a **200** whose body contains `Just a moment...` **or** whose headers include `cf-mitigated` →
  `KindBlocked`

*Provider layer (`rutracker.go`, `nnm.go`, `jackett.go`):* extraction failures that today return bare
errors (e.g. "no magnet link found") must be wrapped as `KindPermanent`. Without this, `KindPermanent`
has no producer.

### Fetcher

```go
type Fetcher interface {
    Fetch(ctx context.Context, url string) ([]byte, error)
}
```
- `directFetcher` — today's `fetchPage` plus `directUserAgent` plus the classification above; keeps
  charset decoding and the 10 MiB `io.LimitReader` cap.
- `solverFetcher` — FlareSolverr; returns `solution.response` (already UTF-8, no charset decoding).
- `blockedFetcher` — used when the solver is unconfigured; always returns
  `&ProviderError{Kind: KindBlocked, Err: errors.New("flaresolverr not configured")}`.
- **A nil `fetcher` is not supported.** All construction goes through the new constructors; every
  struct-literal site in tests must be updated (18 sites).
- New config `FLARESOLVERR_URL` — the **full command endpoint including the `/v1` path**, e.g.
  `https://flaresolverr.pkarpovich.space/v1`. The client appends nothing to it. Empty = RuTracker gets
  `blockedFetcher` and the service still starts.

### FlareSolverr contract (verified live against v3.5.0 — this plan is the source of truth; do not consult upstream docs)

Requests: `POST <FLARESOLVERR_URL>` with header `Content-Type: application/json` and one of these bodies:
- `{"cmd":"sessions.create","session":"<id>"}`
- `{"cmd":"request.get","url":"<url>","session":"<id>","maxTimeout":120000}`
- `{"cmd":"sessions.destroy","session":"<id>"}`

**Success bodies differ per command — a single shared validator that requires `solution` is WRONG:**

| command | success response |
|---|---|
| `sessions.create` | HTTP 200 + `{"status":"ok","message":"Session created successfully.","session":"<id>"}` — **no `solution`** |
| `sessions.destroy` | HTTP 200 + `{"status":"ok","message":"The session has been removed."}` — **no `solution`** |
| `request.get` | HTTP 200 + `{"status":"ok","message":"...","solution":{"status":200,"response":"<html>","cookies":[...],"userAgent":"..."}}` |

**Errors:** HTTP **500** + `{"status":"error","message":"Error: ...","version":"3.5.0"}` and **no
`solution`**. Observed messages: `Error: Request parameter 'cmd' = '...' is invalid.`,
`Error: Error solving the challenge. Timeout after 60.0 seconds.`

**Success rule:** for **every** command, `HTTP 200 AND body.status == "ok"`. **Additionally, for
`request.get` only,** `solution != nil`. Anything else → `&ProviderError{Kind: KindBlocked, Err: <body.message or transport error>}`.

**A request naming a session that does not exist still succeeds** (`status: ok`) — FlareSolverr silently
uses a throwaway browser. There is therefore **no "session not found" error to detect and no recreation
logic to write**; a lost session costs performance (cold solve), never correctness. `sessions.destroy` on
an unknown session also returns `status: ok`.

### Per-provider circuit breaker

Providers gain identity:
```go
// added to providers.Provider
Name() string      // stable lowercase key: "rutracker" | "nnm" | "jackett"

// added to tracker.Parser
func (p *Parser) ProviderName(url string) string   // "" when no provider handles the url
```

```go
// app/tracker/breaker.go, package tracker
type State struct {
    Tripped     bool
    NextProbeAt time.Time
    Cooldown    time.Duration
}

func NewBreaker(now func() time.Time, names ...string) *Breaker   // nil now -> time.Now; seeds an
                                                                  // untripped entry per name
func (b *Breaker) BeginRun()                                      // clears probedThisRun
func (b *Breaker) Allow(name string) bool                         // mutating, see rules
func (b *Breaker) RecordFailure(name string, kind providers.ErrorKind)
func (b *Breaker) RecordSuccess(name string)
func (b *Breaker) Snapshot() map[string]State
```

Rules:
- `NewBreaker` initializes the internal map and **seeds one untripped entry per provider name**, so a
  freshly started process reports every provider as `ok` instead of an empty map. `main.go` passes the
  names of the providers it actually built (`"rutracker"`, `"nnm"`, and `"jackett"` only when
  `JACKETT_URL` is set).
- `RecordFailure` trips on the **first** `KindBlocked`; other kinds never trip it.
- `Allow(name)`: not tripped → true. Tripped and `now < NextProbeAt` → false. Tripped, `now >= NextProbeAt`
  and this provider has not been probed this run → mark probed, return **true** (this is the half-open
  probe). Tripped and already probed this run → **false, even if the probe succeeded** — `RecordSuccess`
  clears `Tripped` but does not clear `probedThisRun`, so at most one task per provider per run is
  attempted while recovering.
- `BeginRun()` is called once at the top of `CheckForUpdates` and clears `probedThisRun`.
- A failed probe doubles the cooldown (capped 24h) and sets a new `NextProbeAt`; a successful probe closes
  the breaker and resets the cooldown to 1h.
- **It gates only the cron sweep.** `CheckFileForUpdates` and `CreateFromURL` bypass it.
- State is a map guarded by `sync.Mutex` — HTTP refresh handlers run concurrently with the cron job.
- `name == ""` (no provider handles the url) → the breaker is not consulted at all.

**Wiring (this is what makes the breaker actually work — without it the feature is dead code):** in
`processFileMetadata`, after `c.tracker.Parse` returns:
- `name := c.tracker.ProviderName(fileMetadata.OriginalUrl)`; if `name == ""`, skip breaker calls.
- on error: `var pe *providers.ProviderError; if errors.As(err, &pe) { c.breaker.RecordFailure(name, pe.Kind) }`
  — unclassified errors record nothing.
- on success: `c.breaker.RecordSuccess(name)`.
- Only when the call came from the cron sweep (see the manual-path rule below).

### Manual vs cron path

`CheckFileForUpdates` (the per-file refresh endpoint) calls the same `processFileMetadata`. It must
**record store outcomes** (so the counter stays truthful) but must **not** feed the breaker and must
**not** send transition notifications — a human pressing refresh should not trip the breaker for the whole
sweep or fire alerts. Implement by passing `fromCron bool` into `processFileMetadata` and gating the
breaker and notification calls on it.

### Per-task failure state

Migration 1 (Task 5). Write the file by hand — `sql-migrate` is not installed and `make new-migration`
would download it. The filename only needs the timestamp prefix convention, and the content must use the
repo's existing marker format (see `migrations/20240805004743-add-location-column.sql`):

```sql
-- +migrate Up
ALTER TABLE files ADD COLUMN consecutive_failures INTEGER NOT NULL DEFAULT 0;
ALTER TABLE files ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE files ADD COLUMN last_error_at TIMESTAMP;

-- +migrate Down
ALTER TABLE files DROP COLUMN consecutive_failures;
ALTER TABLE files DROP COLUMN last_error;
ALTER TABLE files DROP COLUMN last_error_at;
```

Mirror the same columns in the runtime `CREATE TABLE IF NOT EXISTS` in `NewRepository` (that statement is
this repo's authoritative bootstrap).

Go fields on `FileMetadata` (`app/tracker/parser.go`) — **all three are `json:"-"`**:

```go
ConsecutiveFailures int          `json:"-"`
LastError           string       `json:"-"`
LastErrorAt         sql.NullTime `json:"-"`
```

**⚠️ Two traps, both verified in the code:**

1. **`MetadataToMsg` marshals the whole `FileMetadata`** into every "✅ Metadata updated" Telegram
   message. Without `json:"-"` the new fields would start appearing in the user's chat. They are also
   deliberately absent from `FileMetadataResponse` in `app/http/client.go`, so the REST API is unchanged.
2. **`CreateOrReplace` is `INSERT OR REPLACE`**, which SQLite implements as DELETE + INSERT: every column
   **not** in the INSERT list resets to DEFAULT. It runs on the success path of `processFileMetadata`, on
   the rollback path, and in `UpdateTaskLocation`. The three new columns MUST be added to its column list
   and carried from the passed `FileMetadata`, or `consecutive_failures` is silently zeroed on every
   location change — the breaker, the stretch and the notifications quietly stop working while tests pass.

`last_error` is `NOT NULL DEFAULT ''` so existing rows scan into a plain `string`. `last_error_at` stays
nullable and uses `sql.NullTime`.

Outcome recording uses targeted updates, never `CreateOrReplace`:

```go
type SyncFailure struct {
    Text string
    At   time.Time
}

func (r *Repository) RecordSyncSuccess(id string, syncedAt time.Time) error
func (r *Repository) RecordSyncFailure(id string, failure SyncFailure) error
```
- success → `consecutive_failures = 0, last_error = '', last_error_at = NULL, last_sync_at = ?`
- failure → `consecutive_failures = consecutive_failures + 1, last_error = ?, last_error_at = ?`

Stretched interval: a task is skipped in the cron sweep when
`ConsecutiveFailures >= FailureThreshold AND time.Since(LastErrorAt.Time) < deadTaskInterval`.

### DB test harness (does not exist yet — Task 5 creates it)

There is no `repository_test.go` and no DB-backed test in the repo, and `database.openDB` hardcodes
`.db/<filename>` relative to the process CWD. Task 5 adds to `app/task-store/repository_test.go`:

```go
func newTestRepo(t *testing.T) *Repository   // t.Chdir(t.TempDir()); database.NewClient("test.db");
                                             // NewRepository(db); t.Cleanup closes the db
```
Task 7's `app_state` tests reuse the same helper.

### Which outcomes count (`processFileMetadata` has seven exit paths)

The counters model **tracker reachability** — that is what the breaker, the stretch and health are about.

| # | path | records |
|---|---|---|
| 1 | `OriginalUrl == ""` early return | nothing |
| 2 | `c.tracker.Parse` fails | **failure** |
| 3 | `store.GetById` re-read fails | nothing |
| 4 | row deleted mid-run (`DeleteAt.Valid`) | nothing |
| 5 | magnet unchanged, `CreateOrReplace` fails | nothing |
| 6 | magnet changed, `CreateOrReplace` fails | nothing |
| 7 | `CreateDownloadTask` fails (magnet reverted) | nothing |
| — | `Parse` succeeded (either branch) | **success**, recorded right after `Parse` returns |

Rationale: if a qBittorrent outage (path 7) or a SQLite busy error counted as a task failure, one hour of
qBittorrent downtime would mark **every** task failing → health degraded and one Telegram message per
task. Those failures stay logged, as today.

### Cron run state

Migration 2 (Task 7), written by hand the same way:

```sql
-- +migrate Up
CREATE TABLE IF NOT EXISTS app_state (key TEXT PRIMARY KEY, value TEXT);

-- +migrate Down
DROP TABLE app_state;
```
Mirrored in `NewRepository`'s runtime bootstrap.

```go
func (r *Repository) SetLastRun(at time.Time, ok bool) error
func (r *Repository) GetLastRun() (at time.Time, ok bool, err error)
```
Keys `last_run_at` (RFC3339) and `last_run_ok` (`"true"`/`"false"`). Written via `defer` at the end of
`CheckForUpdates`, including the early-return path. `last_run_ok = false` **only** when the sweep itself
could not complete (`store.GetAll` failed); individual task failures do NOT make a run not-ok.

### Health endpoint

```json
{ "status": "ok | degraded | unhealthy",
  "tracked": 42, "failing": 0,
  "last_run_at": "...", "providers": {"rutracker": "ok", "nnm": "ok"} }
```

`failing` = number of rows with `consecutive_failures >= FailureThreshold`.

Evaluated **in this order** (first match wins):
1. `unhealthy` (+ HTTP 503) — any breaker tripped, **OR** (`last_run_at` present **AND**
   `time.Since(last_run_at) > staleRunAfter`), **OR** (`last_run_at` absent **AND**
   `time.Since(StartedAt) > staleRunAfter`)
2. `degraded` (+ HTTP 200) — `failing > 0`
3. `ok` (+ HTTP 200)

The third clause is why `StartedAt` is required: with a missing key `last_run_at` is the zero time, so
without it a freshly booted service would return 503 immediately.

`providers` keys are `Breaker.Snapshot()`'s keys (seeded at construction, so never empty), values `ok`
or `blocked`.

`staleRunAfter` is computed **once in `main.go`**, never in the handler:
`sched, err := cron.ParseStandard(cfg.Cron)` (the standard 5-field parser — the repo's default is
`0 * * * *` and gocron runs with seconds disabled, so a parser with seconds enabled would reject it), then
`first := sched.Next(time.Now()); second := sched.Next(first); staleRunAfter := 2 * second.Sub(first)`.
On a parse error use `staleRunFallback` and **log at WARN** so a bad `CRON` is visible.

`http.NewClient` is already at 4 positional parameters, so Task 7 converts it to an options struct
mirroring `download_tasks.ClientCtx`:

```go
type ClientCtx struct {
    Config           config.HttpConfig
    Store            FileStore
    TaskCreator      TaskCreator
    DownloadClient   DownloadClient
    Breaker          BreakerSnapshotter
    RunState         RunStateReader
    StaleRunAfter    time.Duration
    StartedAt        time.Time
    FailureThreshold int
}

func NewClient(ctx *ClientCtx) *Client

type BreakerSnapshotter interface { Snapshot() map[string]tracker.State }
type RunStateReader    interface { GetLastRun() (time.Time, bool, error) }
```
`Config` must stay — `Start` uses `c.config.Port` and `fileHandler` uses `c.config.BaseStaticPath`;
omitting it breaks the build. All **14** existing call sites in `client_test.go` become
`NewClient(&ClientCtx{Store: ..., TaskCreator: ..., DownloadClient: ...})` with zero values elsewhere; a
zero `StartedAt` means the staleness branch is skipped.

### Telegram transitions

Derived from the counter **around** the write:
- ok→failing: notify only when `before == FailureThreshold-1 && after == FailureThreshold` (never at
  `after > FailureThreshold`, or a stuck task re-notifies every run).
- failing→ok: notify only when `before >= FailureThreshold && after == 0`.
- A recovery from fewer than `FailureThreshold` failures is silent, matching the silent ramp-up.
- Breaker: accumulate skipped-task counts per provider during the sweep; **at the end of
  `CheckForUpdates`**, for each provider that tripped during this run send exactly one message with the
  provider name, the number of its tasks skipped in this run, and `NextProbeAt` from `Snapshot()`. Send
  one message when a half-open probe succeeds.

Both task messages carry the task name/id and `last_error`.

**⚠️ `messagesForSend` is unbuffered** — sends must happen **outside** any `c.mu` critical section, or the
sweep deadlocks. `c.tracker.Parse` is already outside the mutex, so outcome recording needs no lock
changes (the file has 6 `c.mu.Lock()` sites; that count must not change).

## Testing Strategy

Repo conventions: table-driven, `testify`, one `_test.go` per source file in the same package,
hand-written func-field mocks, `httptest` for fake servers.

**Test function/case names are pinned so Task 9 can verify them mechanically:**

| test | name |
|---|---|
| classification matrix cases | `blocked_403`, `blocked_429`, `blocked_cf_body`, `blocked_cf_header`, `transient_500`, `permanent_404`, `transient_timeout` |
| provider extraction failure | `TestProviderExtractionFailureIsPermanent` |
| loki error text | `TestResolveValueErrorText` |
| solver happy path / reuse / errors | `TestSolverFetchSuccess`, `TestSolverSessionReusedAcrossThreeFetches`, `TestSolverErrorIsBlocked` |
| breaker | `TestBreakerTripSkipsWithoutFetch`, `TestBreakerProbeAfterCooldown`, `TestBreakerCooldownSequence`, `TestBreakerSeededProvidersAreOK` |
| CreateOrReplace regression | `TestCreateOrReplacePreservesConsecutiveFailures` |
| outcome recording | `TestParseFailureIncrements`, `TestDownloadFailureLeavesCounterZero` |
| health | `TestHealthOK`, `TestHealthDegraded`, `TestHealthUnhealthyBreaker`, `TestHealthUnhealthyStale`, `TestHealthNeverRanWithinGrace` |
| notifications | `TestNotifyOnceAtThreshold`, `TestNotifyOnceOnRecovery`, `TestBreakerNotifiesOncePerProvider` |
| manual path | `TestManualRefreshDoesNotTripBreaker` |

Required behaviours: cooldown sequence exactly `[1h,2h,4h,8h,16h,24h,24h]` with an injected clock and no
real sleeps; the solver session created once and reused across **three** sequential fetches; a tripped
provider produces **one** message, not one per skipped task; 3 consecutive failures produce exactly one
message and the 4th and 5th produce none.

Existing suites must stay green: `go test ./... -race`.

**There is no automated e2e harness. Live verification is Post-Completion only and is explicitly NOT part
of any task's completion criteria.**

## Progress Tracking

- mark completed items `[x]` immediately when done.
- add newly discovered tasks with `➕`; document blockers found during execution with `⚠️`.

## What Goes Where

- **Implementation Steps** (`[ ]`): everything verifiable with the repo alone.
- **Post-Completion** (no checkboxes): live acceptance and a config change in a separate repository. No
  task blocks on them.

## Implementation Steps

### Task 1: Make errors and failures visible in logs

**Files:**
- Modify: `app/observability/loki.go`, `loki_test.go`
- Modify: `app/bot/download-tasks/client.go`, `client_test.go`

- [x] in `resolveValue`, if the resolved value satisfies `error`, store `err.Error()` so the text survives
      `json.Marshal` (today every error logs as `{}`)
- [x] add the task `id` and the tracker URL to the error logs in `processFileMetadata` and
      `CheckFileForUpdates`
- [x] write `TestResolveValueErrorText` asserting an `error` attr serializes to its message string
- [x] write a test asserting the failure log for a task carries its id
- [x] run the per-task gate — must pass before Task 2

### Task 2: Error model, Fetcher interface, and the direct fetcher

**Files:**
- Create: `app/tracker/providers/errors.go`, `errors_test.go`, `fetcher.go`, `fetcher_test.go`
- Modify: `app/tracker/providers/base.go` (delete `fetchPage`), `rutracker.go`, `nnm.go`, `jackett.go`
- Modify: `app/tracker/providers/rutracker_test.go`, `jackett_test.go`, `tracing_test.go`
- Modify: `app/tracker/parser_test.go`, `app/tracker/integration_test.go` (their `mockProvider` /
  `recordingProvider` implement `providers.Provider` and stop compiling once `Name()` joins it)
- Modify: `app/main.go`

- [x] add `ErrorKind`, its `String()` returning exactly `Transient`/`Blocked`/`Permanent`, and
      `ProviderError` with `Error()` and `Unwrap()`
- [x] add the `Fetcher` interface and `directFetcher` (sends `directUserAgent`, applies the full transport
      classification matrix, keeps charset decoding and the 10 MiB cap); delete `fetchPage`
- [x] add `Name() string` to `Provider` returning `rutracker` / `nnm` / `jackett`; give the two test
      doubles a `name` field and a `Name()` method
- [x] add `NewRutrackerProvider(f Fetcher)` and `NewNnmProvider(f Fetcher)` (both gain an unexported
      `fetcher` field), extend `NewJackettProvider(baseURL string, f Fetcher)`, and update all 18 provider
      construction sites — a nil fetcher is not supported
- [x] wrap provider extraction failures (e.g. "no magnet link found") as `KindPermanent`
- [x] write the classification matrix table test with the seven pinned case names
- [x] write `TestProviderExtractionFailureIsPermanent` and a test asserting the request's `User-Agent`
      equals `directUserAgent`, plus a test for `ErrorKind.String()`
- [x] run the per-task gate — must pass before Task 3

### Task 3: Route RuTracker through FlareSolverr

**Files:**
- Create: `app/tracker/providers/solver.go`, `solver_test.go`
- Modify: `app/config/config.go`, `config_test.go`
- Modify: `app/main.go`, `compose.yaml`, `README.md`

- [x] add `FLARESOLVERR_URL` to config (empty = solver disabled), to `compose.yaml`, **and to the env-var
      list in `README.md`** (documented here, not in Task 10, so Task 9 can verify it)
- [x] implement `solverFetcher` holding `baseURL`, `mu sync.Mutex`, `sessionID string` and an
      `*http.Client` with `Timeout: solverHTTPTimeout`; send `Content-Type: application/json`
- [x] on `Fetch`: lock, lazily `sessions.create` with
      `sessionID = "magnet-feed-sync-" + strconv.FormatInt(time.Now().UnixNano(), 36)` computed once per
      process, then `request.get` with `maxTimeout: solverMaxTimeout`. Apply the **per-command** success
      rule (`HTTP 200 && status=="ok"` for every command, plus `solution != nil` for `request.get` only) —
      a shared validator requiring `solution` would reject every successful `sessions.create`.
      **Write no session-recreation logic** — a missing session is not an error (verified contract)
- [x] add `blockedFetcher` and `func (f *solverFetcher) Close(ctx context.Context) error` issuing
      `sessions.destroy` (no-op when `sessionID` is empty); `Close` is NOT part of `Fetcher` — `main.go`
      holds the concrete type
- [x] in `main.go` register the close with a **detached** context: `run()` calls `cancel()` before
      deferred functions execute, so a plain `defer solver.Close(ctx)` would always fail with
      `context.Canceled` and leak the browser. Use `context.WithoutCancel(ctx)` plus a 10s timeout and log
      the error
- [x] wire RuTracker to `solverFetcher` when `FLARESOLVERR_URL` is set, else `blockedFetcher`; NNM and
      Jackett keep `directFetcher`
- [x] write `TestSolverFetchSuccess`, `TestSolverSessionReusedAcrossThreeFetches` (fake returns a
      solution-less `status:"ok"` for `sessions.create` and the fetch still succeeds), `TestSolverErrorIsBlocked`
      (HTTP 500 + `status:"error"`), and a case where `status:"ok"` lacks `solution` on `request.get`
- [x] run the per-task gate — must pass before Task 4

### Task 4: Per-provider circuit breaker

**Files:**
- Create: `app/tracker/breaker.go` (package `tracker`), `app/tracker/breaker_test.go` — do not relocate
- Modify: `app/tracker/parser.go` (add `ProviderName`)
- Modify: `app/bot/download-tasks/client.go`, `client_test.go`
- Modify: `app/main.go`

- [x] add `func (p *Parser) ProviderName(url string) string` returning `""` when nothing handles it
- [x] implement `Breaker` exactly as specified in Technical Details: `NewBreaker(now, names...)` seeding
      one untripped entry per provider, `BeginRun`, mutating `Allow` with `probedThisRun`, `RecordFailure`
      / `RecordSuccess`, `Snapshot`, mutex-guarded, doubling cooldown `1h→24h`
- [x] extend the local `FileParser` consumer interface with `ProviderName(url string) string` and update
      `mockFileParser`
- [x] add the consumer interface
      `type ProviderBreaker interface { Allow(string) bool; BeginRun(); RecordFailure(string, providers.ErrorKind); RecordSuccess(string) }`
      plus a `Breaker` field on `ClientCtx`; construct one `*tracker.Breaker` in `main.go` and inject it
      into both the download-tasks client and (as `BreakerSnapshotter`) the HTTP client in Task 7
- [x] **wire the recording calls** in `processFileMetadata` per the Wiring block in Technical Details
      (`errors.As` to recover the kind; skip when `name == ""`; only when `fromCron`) — without this the
      breaker never trips and the feature is dead code
- [x] call `BeginRun()` at the top of `CheckForUpdates` and skip a task **without issuing any request**
      when `Allow` returns false, logging the skip once per run
- [x] write `TestBreakerTripSkipsWithoutFetch`, `TestBreakerProbeAfterCooldown`,
      `TestBreakerCooldownSequence` (exactly `[1h,2h,4h,8h,16h,24h,24h]`, injected clock),
      `TestBreakerSeededProvidersAreOK`
- [x] run the per-task gate — must pass before Task 5

### Task 5: Persist per-task failure state

**Files:**
- Create: `migrations/<UTC YYYYMMDDHHMMSS>-add-failure-tracking.sql` (write by hand — see Technical Details)
- Create: `app/task-store/repository_test.go`
- Modify: `app/task-store/repository.go`, `app/tracker/parser.go`

- [ ] write the migration file by hand with the DDL from Technical Details, using the repo's
      `-- +migrate Up` / `-- +migrate Down` marker format
- [ ] mirror the three columns in the runtime `CREATE TABLE IF NOT EXISTS` in `NewRepository`
- [ ] add the three `json:"-"` fields to `FileMetadata` and extend every `SELECT`/scan site in `GetAll`
      and `GetById`
- [ ] **add the three columns to `CreateOrReplace`'s INSERT list** and carry them from the passed
      `FileMetadata` — see the trap in Technical Details
- [ ] add `SyncFailure`, `RecordSyncSuccess` and `RecordSyncFailure` as targeted `UPDATE`s
- [ ] add the `newTestRepo(t)` helper (`t.Chdir(t.TempDir())` + `database.NewClient("test.db")`) — no
      DB-backed test exists in this repo yet
- [ ] write tests for both outcome methods and for round-tripping the new fields through `GetAll`/`GetById`
- [ ] write `TestCreateOrReplacePreservesConsecutiveFailures` (set 2, round-trip, assert still 2)
- [ ] run the per-task gate — must pass before Task 6

### Task 6: Record outcomes and stretch the interval for dead tasks

**Files:**
- Modify: `app/bot/download-tasks/client.go`, `client_test.go`

- [ ] define `FailureThreshold = 3` (exported) and `deadTaskInterval = 24h`
- [ ] add `RecordSyncSuccess` / `RecordSyncFailure` to the local `FileStore` consumer interface and to
      `mockFileStore`
- [ ] add `fromCron bool` to `processFileMetadata`; record outcomes exactly per the seven-path table
      (only a `c.tracker.Parse` failure records a failure; success recorded right after `Parse` returns;
      store and download-client errors record nothing). Store recording happens on **both** paths; breaker
      and notifications only when `fromCron`
- [ ] skip a task in `CheckForUpdates` when `ConsecutiveFailures >= FailureThreshold` and
      `time.Since(LastErrorAt.Time) < deadTaskInterval`
- [ ] write `TestParseFailureIncrements`, `TestDownloadFailureLeavesCounterZero`,
      `TestManualRefreshDoesNotTripBreaker`, and a test that a task over the threshold is skipped within
      24h and attempted after
- [ ] confirm `grep -c 'c\.mu\.Lock()' app/bot/download-tasks/client.go` still returns 6 (outcome
      recording must not change the locking structure)
- [ ] run the per-task gate — must pass before Task 7

### Task 7: Cron run state and the real health endpoint

**Files:**
- Create: `migrations/<UTC YYYYMMDDHHMMSS>-add-app-state.sql` (by hand)
- Modify: `app/task-store/repository.go`, `repository_test.go`
- Modify: `app/bot/download-tasks/client.go`
- Modify: `app/http/client.go`, `client_test.go`
- Modify: `app/main.go`, `go.mod` (promote `robfig/cron/v3` to direct)

- [ ] write the `app_state` migration by hand plus the mirrored runtime `CREATE TABLE IF NOT EXISTS`, and
      add `SetLastRun` / `GetLastRun` (reusing `newTestRepo`)
- [ ] persist the run outcome via `defer` at the end of `CheckForUpdates`, including the early-return path;
      `last_run_ok = false` only when `store.GetAll` failed
- [ ] compute `staleRunAfter` in `main.go` via `cron.ParseStandard(cfg.Cron)` (gap between the next two
      fire times × 2, `staleRunFallback` on error with a WARN log)
- [ ] convert `http.NewClient` to the `ClientCtx` options struct exactly as written in Technical Details
      (including `Config` — omitting it breaks `Start` and `fileHandler`) and add the two consumer
      interfaces; update all **14** call sites in `client_test.go`
- [ ] rewrite `healthHandler` to return `status`/`tracked`/`failing`/`last_run_at`/`providers` with the
      three-branch ordered evaluation, `failing` counted as `consecutive_failures >= FailureThreshold`,
      and HTTP 503 for `unhealthy`
- [ ] write `TestHealthOK`, `TestHealthDegraded`, `TestHealthUnhealthyBreaker`, `TestHealthUnhealthyStale`
      (`last_run_at = now-3h`, `staleRunAfter = 2h` → 503; `now-90m` → not stale) and
      `TestHealthNeverRanWithinGrace` (no `last_run_at`, `StartedAt = now-10m` → ok/200;
      `StartedAt = now-3h` → 503)
- [ ] run the per-task gate — must pass before Task 8

### Task 8: Telegram notifications on state transitions

**Files:**
- Modify: `app/bot/download-tasks/client.go`, `client_test.go`

- [ ] read `consecutive_failures` before recording and notify ok→failing only when
      `before == FailureThreshold-1 && after == FailureThreshold`, failing→ok only when
      `before >= FailureThreshold && after == 0`
- [ ] accumulate skipped counts per provider during the sweep and send **one** breaker message per tripped
      provider at the **end** of `CheckForUpdates` (name, skipped count, `NextProbeAt`); one more when a
      half-open probe succeeds
- [ ] include the task name/id and `last_error` in task messages; send only when `fromCron`
- [ ] send on `messagesForSend` **outside** any `c.mu` critical section (unbuffered channel — a send under
      the mutex deadlocks the sweep)
- [ ] write `TestNotifyOnceAtThreshold` (3 failures → exactly one message; 4th and 5th → none),
      `TestNotifyOnceOnRecovery`, `TestBreakerNotifiesOncePerProvider`
- [ ] run the per-task gate — must pass before Task 9

### Task 9: Verify the implementation against the repository

*Mechanical, repo-local checks only. Live verification is Post-Completion and is NOT part of this task's
completion criteria. Every checkbox below is a command with a defined expected result.*

- [ ] `go vet ./...`, `go build ./...`, `go test ./... -race` all exit 0
- [ ] `gofmt -s -l .` prints exactly the four pre-existing files listed in Context — no more, no fewer
- [ ] `grep -c 'Message:[[:space:]]*"OK"' app/http/client.go` returns 0
- [ ] `grep -rc 'fetchPage' app/tracker` returns 0 for every file
- [ ] `grep -c 'blocked_403\|blocked_429\|blocked_cf_body\|blocked_cf_header\|transient_500\|permanent_404\|transient_timeout' app/tracker/providers/fetcher_test.go` returns 7
- [ ] `grep -c 'TestHealthOK\|TestHealthDegraded\|TestHealthUnhealthyBreaker\|TestHealthUnhealthyStale\|TestHealthNeverRanWithinGrace' app/http/client_test.go` returns 5
- [ ] `grep -c 'TestResolveValueErrorText' app/observability/loki_test.go` returns 1
- [ ] `grep -c 'TestCreateOrReplacePreservesConsecutiveFailures' app/task-store/repository_test.go` returns 1
- [ ] `grep -c 'TestBreakerTripSkipsWithoutFetch\|TestBreakerProbeAfterCooldown\|TestBreakerCooldownSequence\|TestBreakerSeededProvidersAreOK' app/tracker/breaker_test.go` returns 4
- [ ] `grep -c 'TestNotifyOnceAtThreshold\|TestNotifyOnceOnRecovery\|TestBreakerNotifiesOncePerProvider\|TestManualRefreshDoesNotTripBreaker' app/bot/download-tasks/client_test.go` returns 4
- [ ] `ls migrations | grep -c 'add-failure-tracking\|add-app-state'` returns 2, and
      `grep -c '+migrate Up' migrations/*add-failure-tracking*.sql migrations/*add-app-state*.sql` returns
      1 for each
- [ ] `grep -c 'consecutive_failures' app/task-store/repository.go` is ≥ 4 (runtime CREATE TABLE, the
      `CreateOrReplace` INSERT list, and both record methods)
- [ ] `grep -rc 'FLARESOLVERR_URL' app/config/config.go compose.yaml README.md` returns ≥ 1 for each
- [ ] `grep -c 'c\.mu\.Lock()' app/bot/download-tasks/client.go` returns 6
- [ ] the style greps from the per-task gate report zero violations over the gate's `$CHANGED` scope

### Task 10: Update documentation and close out

**Files:**
- Modify: `README.md`, `CLAUDE.md`

- [ ] document the new `/api/health` response shape in `README.md` (`FLARESOLVERR_URL` was already added
      in Task 3)
- [ ] note in `CLAUDE.md`: the `Fetcher` abstraction and which provider uses which fetcher, the error
      taxonomy, the breaker, and that health now reports real state
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

*Automated executors: stop here. Nothing below is a task, a checkbox, or a completion criterion — do not
attempt to resolve these.*

**Deployment host (open question for the operator, not a plan blocker).** The service is currently stopped
— deliberately, so it would stop hammering RuTracker — and was not found on the usual homelab hosts nor
listening locally. The operator determines the host before the acceptance run. No task depends on it.

**Live acceptance scenario:**

1. A tracked RuTracker task parses successfully through FlareSolverr — the magnet is extracted and the
   row's `last_sync_at` advances. This is the thing that has been broken for two weeks.
2. Session reuse holds in production: the run log contains exactly one session-created line per cron run.
   Wall-clock (~2 min for ~23 tasks) is informational, not a pass/fail bar.
3. With `FLARESOLVERR_URL` unset: RuTracker tasks classify as `Blocked`, the breaker trips, the run issues
   no repeated requests, `/api/health` returns `unhealthy` + 503, and exactly one Telegram message arrives.
4. `/api/health` returns `status: ok` when healthy.
5. Logs show the real error text and the task id for a deliberately failing task.
6. NNM tasks keep working through the direct fetcher (no regression).

**Separate repository — uptime monitor config.** The monitor asserts on the health response body; the old
assertion (`message == OK`) must become the new one (`status == ok`) in its own repository, and it must
land **together with** the service deploy or the monitor will flap.

**Not part of this plan — follow-up once logs are readable:** identify the 3 tasks that were failing
hourly even before Jul 27. With Task 1 shipped their ids appear in the logs; alternatively the rows with
the oldest `last_sync_at` reveal them.
