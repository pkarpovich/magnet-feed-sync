# Cross-indexer release watcher with NATS notification

## Overview

The tuclaw agent hunts for not-yet-released media using scheduled tasks that wake the LLM on a timer with
**no condition pre-check**. The LLM itself re-runs the same searches and almost always finds nothing:
one such task logged **38 runs in 5 days, ~35s each, results overwhelmingly `[SILENT]`, zero hits**.
"Did anything new appear" is pure mechanics and must not keep an LLM in the loop.

This plan moves that mechanical delta-check into magnet-feed-sync as a **server-side watcher**: a `watch`
object (queries + regex filters + seen-state), an hourly cron that searches Jackett and ext.to, and a
**NATS publish that wakes the agent only on a genuine new match**. Verification stays with the agent —
indexers only index the title, and the title lies.

Why the watcher cannot live on the agent side: the tuclaw scheduler's `condition` pre-check has a **hard
30s timeout**, while a cold cross-indexer Jackett search measures **40s** and a cold ext.to search **79s**.
A condition doing the search would time out on every tick and the scheduler would read that as "condition
not met" — the watch would silently never fire. The search needs a home without a 30s cap.

### Non-goals

- **No auto-download.** The service never grabs a release by itself. It notifies; the agent verifies via
  defuddle (`Перевод:` / `Аудио N:` / `Реклама:`) and decides. Explicitly chosen by the user.
- **No `exclude_ids` denylist.** Superseded by seen-marking at publish time: anything already announced
  never fires again, so a second denylist mechanism is dead weight.
- **No per-watch tuning of source parameters** (ext.to `sort`/`order`/`cat`/`adult`, Jackett category).
  They are fixed constants shared by the cron and the reproduction endpoint — that is what makes the two
  identical — a property Task 9 asserts with a test, not just prose. Per-watch tuning is a later concern.
- **Do NOT touch the `files` table, `CreateOrReplace`, or the existing files cron.** New tables only.
- **Do NOT modify the `TUCLAW` NATS stream.** It is created and owned by the tuclaw daemon
  (`CreateOrUpdateStream`); this service is a publisher only.
- **Do NOT reformat unrelated files.** `gofmt -s -l .` is clean on master — keep it that way.
- **No frontend work.** The watcher has no UI in this plan.
- **Do NOT build a second FlareSolverr client.** Extending the prerequisite's existing solver with one
  cookie-returning method **is in scope** for Task 3 — see "Solver seam" in Technical Details. The
  prerequisite's `Fetcher` returns only the response body, so it structurally cannot carry the cookie and
  User-Agent the ext.to source needs; that gap is closed by extending the existing client, not by a new one.

### Rejected alternatives

- **A `condition` script on the agent's scheduled task.** The cheapest imaginable fix and it reuses a
  mechanism already proven by five production tasks — but the 30s condition timeout versus a 40-80s cold
  search makes it silently non-functional. This is the reason the whole plan exists; do not re-propose it.
- **Jackett-only watcher.** Rejected: measured, ext.to is the source that carries EN releases nothing else
  has, and the flagship task (the 38-wake one) hunts exactly an EN reference. A Jackett-only watcher would
  leave that task burning tokens.
- **A separate Python worker service reusing `extto.py` as-is.** Avoids a second HTML parser, but adds a
  whole deployment unit with its own compose, updater entry, health check and logs — a new place to fail
  silently, which is precisely the failure class the prerequisite plan exists to kill.
- **Search parameters inside the NATS payload.** Rejected during design: a watch's search is defined by
  queries + sources + `include_regex` + `exclude_regex` + source constants, so any field-copying payload
  silently diverges from what the watcher evaluated. Reproducing with queries+sources alone returns the
  unfiltered 226-result set instead of the 1 that matched. Reproduction is done **by the service from the
  watch row**; the payload carries identity and delta only.
- **Storing found results for a `hits` endpoint.** Rejected by the user: live reproduction covers it, and
  the Jackett cache (35 min) plus the shared in-process ext.to cookie make the agent's post-wake search
  effectively free.
- **Marking `seen` before publishing.** Rejected: it converts a crash into a permanently and silently lost
  release. Publish-then-mark converts the same crash into one duplicate wake.

## Dependency — this plan runs AFTER the error-handling plan

The error-handling plan (in `docs/plans/`, or `docs/plans/completed/` once it has landed) must land first.
It delivers, and this plan **consumes rather than rebuilds**: the `Fetcher` interface and its
FlareSolverr-backed implementation, typed error kinds and the per-provider circuit breaker, the real
`/api/health` with ordered `ok | degraded | unhealthy` evaluation, and a DB-backed repository test harness.

**Probe the code, not the plan file** — run these first and record the result in the progress log:

```bash
grep -rq 'type Fetcher interface' app/tracker/providers/   # solver plumbing exists
grep -rq 'Breaker'               app/tracker/              # circuit breaker exists
grep -rq 'degraded'              app/http/client.go        # health has the ok|degraded|unhealthy model
grep -rq 'ClientCtx'             app/http/client.go        # NewClient is an options struct
```

The health probe matches on `degraded` and **not** on `"status"`: the pre-existing one-shot download
handler already returns `{"status":"ok"}`, so a `"status"` probe reports present for something absent.

- **All present** ⇒ proceed normally.
- **Any missing** ⇒ do **not** rebuild them and do **not** stall. Mark the affected work in this file with
  `⚠️ blocked — prerequisite not landed`, complete everything else, and treat the plan as done.
  **A reviewer MUST accept a `⚠️ blocked` marker as terminal and MUST NOT re-report it as a finding.**

Blast radius per missing artifact, so a blocked run still has a finite done-condition:

| missing | blocked |
|---|---|
| `Fetcher` / solver | Task 3 entirely; the solver leg of Task 7; the magnet bullets, `magnet`/`magnet_error` keys and magnet test of Task 9; the ext.to leg of Task 11 — the acceptance run then uses `sources jackett` only |
| breaker | nothing structural: source errors still land in `last_status`; skip the breaker bullet in Task 4 |
| health status model | Task 10 entirely |
| `ClientCtx` | nothing is blocked — Task 8 performs the conversion itself, moving the existing four positional parameters into the struct before adding its own field |

The test harness is the one exception to "consume, don't rebuild": if no reusable helper exists, create the
equivalent local helper in the new package (Task 1 specifies it).

## Skills to invoke

The `Code-Quality Rules` section below is the **complete and sole** acceptance bar for style. If the `go`
skill is available, loading it is optional context; if not, proceed — it is not a blocker, and a reviewer
MUST NOT fail a task on any rule not written in that section.

## Context (from discovery)

Everything in this section was verified live during design. **Do not re-derive it, do not re-measure it,
do not re-query the hosts.**

**Root cause evidence (tuclaw scheduler DB, table `scheduled_tasks` / `task_run_logs`):**

| fact | value |
|---|---|
| flagship task | `poll_until`, `3h`, `condition = NULL`, prompt "[AllSpeak — One Night Only (2026), EN-референс]" |
| its run log | 38 runs 2026-08-05 → 2026-08-10, avg `duration_ms` 34826, results overwhelmingly `[SILENT]`, zero hits |
| other condition-less watch tasks | 2 × `poll_until 168h` (anime, RU dub for a cartoon), 1 × `cron 0 17 * * 1` (House of the Dragon) |
| tasks that DO use `condition` | 5 (spirit-tracker, Vlad, Stephen, calendar, repo-watcher) — mechanism proven in production |
| condition timeout | **30s**, hard; timeout or error ⇒ task skipped |
| event payload clamp | **10240 runes** |

**Measured latencies (n≥3 where noted — do not re-measure):**

| operation | measurement |
|---|---|
| Jackett `indexers/all` search, cold | 41s, 40s (n=3; third run 0.1s from cache) |
| Jackett result cache | `CacheEnabled=true`, `CacheTtl=2100` (35 min), `CacheMaxResultsPerIndexer=1000` |
| ext.to search, cold (Cloudflare challenge via FlareSolverr) | **79s** |
| ext.to search, warm cookie | **~1s** |

**Measured search behaviour on the acceptance title (this is why the design filters the way it does):**

| query | source | result |
|---|---|---|
| `One Night Only 2026` | Jackett | **1** item: `Только на одну ночь / One Night Only (2026) TSRip [H.264] [AD]`, nnmclub `t=1883913` |
| `One Night Only` | Jackett | **226** items, nearly all junk (adult, RuPaul, Top Chef, Bee Gees, Def Leppard, the 2016 Chinese film) |
| `One Night Only 2026` | ext.to | **0** items |
| `One Night Only` | ext.to | concerts only (Bee Gees, Def Leppard, One Desire, Barbra Streisand) |

Conclusion baked into the design: **the narrow query is the primary filter**, `include_regex` is the
second, `exclude_regex` is only a backstop. It also independently confirms the 38 empty wakes were
genuine — no EN rip exists yet.

**Infrastructure (verified live):**

| fact | value |
|---|---|
| deployment host | this service and NATS, Jackett and FlareSolverr all run on the **same host** |
| NATS reachability | the `nats` container is on the external `proxy` docker network, which this service's compose already joins ⇒ reachable as `nats:4222` container-to-container |
| JetStream stream | name `TUCLAW`, subjects `["tuclaw.>"]`, 1 durable consumer (the tuclaw daemon's event listener is running) |
| event subject rule | the **first token MUST be the literal `tuclaw`**; an `event` task fires **once** then completes (the agent re-arms) |
| Jackett internal base | Jackett emits `<link>` / `<enclosure url>` pointing at its **own internal base URL**, not a reachable one |

**Verified repo facts (checked against the checkout):**

| fact | value |
|---|---|
| `gofmt -s -l .` on master | **clean — prints nothing** |
| migrations | live in `app/migrations/`, embedded via `//go:embed *.sql` in `embed.go`, applied by a one-shot container before the app starts; `migrations.Apply(db *sql.DB) (int, error)` is the entry point and `database.Client` exposes `DB()` |
| `app/task-store/repository.go` | `NewRepository` **verifies** the schema (`PRAGMA table_info` for required columns, `sqlite_master` for tables) and returns `ErrSchemaNotInitialised`; it no longer creates anything |
| `app/task-store/repository_test.go` | `newTestRepo` runs `migrations.Apply` on a temp database before constructing the repository |
| `JACKETT_URL` in the live env | a bare base URL with **no api key** ⇒ the service currently cannot run a search at all |
| `app/tracker/providers/jackett.go` | `parseXML` hardcodes `rss.Channel.Items[0]`; `extractMagnet` requires a `magnet:` prefix and returns `""` otherwise; the torznab structs parse **no** `torznab:attr` (so no seeders) ⇒ **not reusable as-is** for searching |
| `<comments>` vs `<guid>` vs `<link>` | `<comments>` is the real tracker page URL; `<guid>` is a tracker `download.php` link; `<link>`/`<enclosure url>` are the Jackett proxy `.torrent` (internal host) |
| `app/schedular/schedular.go` | `Service.Start(cb)` registers exactly **one** gocron job |
| `app/main.go` | `messagesForSend := make(chan string)` is **unbuffered** |
| `app/task-store/repository.go` | `CreateOrReplace` is `INSERT OR REPLACE` over 9 columns (SQLite ⇒ DELETE+INSERT, unlisted columns reset to DEFAULT) |
| `app/http/client.go` | `POST /api/downloads` already exists (one-shot: magnet or `.torrent` URL forwarded verbatim, nothing persisted) |
| migrations | `/migrations/`, sql-migrate, e.g. `20240805004743-add-location-column.sql` |
| NATS client | not yet a dependency; tuclaw uses `github.com/nats-io/nats.go` with its `jetstream` subpackage (`jetstream.New(nc)`) |

### ext.to wire protocol (captured live from the site during design)

**This section is the complete and sole specification for the port. Everything a fresh session needs is
here; there is no external script to consult and none is required.**

Base URL: `https://search.extto.com` (constant `exttoBaseURL`).

**Request shape.** Every call carries `User-Agent: <the UA that came with the cookie>`, `Cookie: <cookie
string>`, `Accept: */*`, `Referer: https://search.extto.com/`. The signed POST additionally carries
`X-Requested-With: XMLHttpRequest`, `Content-Type: application/x-www-form-urlencoded; charset=UTF-8` and
`Origin: https://search.extto.com`. **The UA must be exactly the one returned alongside the cookie** — a
UA/cookie mismatch re-triggers the Cloudflare challenge immediately.

**Challenge detection.** A response is a challenge when its body contains `Just a moment` **or**
`_cf_chl_opt`. Status code is not a reliable signal. A solved page still mentions `challenge-platform`, so
do not match on that. **Only GETs may be auto-retried** after a cookie refresh; a signed POST must be
re-issued by the caller with fresh tokens, never blindly retried.

**Cookie refresh** is the only step that uses FlareSolverr: `POST` the solver endpoint with
`{"cmd":"request.get","url":"https://search.extto.com/","maxTimeout":120000}`. Require `status == "ok"`
and a non-empty `solution.cookies`; build the cookie header as `name=value` pairs joined by `"; "`, and
take the UA from `solution.userAgent`. Cookie and UA are then held in memory for subsequent direct calls.

**Search** is a plain GET to `/browse/` with query parameters `q`, `sort`, `order` (see the constants
table for the fixed `sort`/`order` values; `cat` and `with_adult` are not sent).

**Page tokens.** The search page carries two distinct 32-hex tokens and *they are not interchangeable*:

| token | where it lives | used for |
|---|---|---|
| search-page token | inline script: `window.searchPageToken = '<32 hex>';` | the third component of the signature |
| csrf token | `<meta name="csrf-token" content="<32 hex>">` | sent as the `sessid` form field |

**Row markup.** Rows live in `<tbody>`; a row is a torrent row iff it contains an anchor with class
`search-magnet-btn`. Verbatim shape of the load-bearing parts of one real row (whitespace trimmed):

```html
<tr>
  <td class="text-left">
    <a href="/dune-prophecy-s01-2160p-uhd-eur-blu-ray-hevc-hdr10-truehd-7-1-mteam-20151803/"
       class="torrent-title-link"><b><span>Dune</span>.Prophecy.S01.2160p.UHD...-MTeam</b></a>
    <div class="related-posted">Posted by <a ...>Knroad</a> in <a href="/tv/">TV</a> - <a>Season Packs</a></div>
    <a class="dwn-btn search-magnet-btn" href="javascript:void(0);" data-id="20151803"></a>
  </td>
  <td><div class="add-block-wrapper"><span class="add-block">Size</span><span>235.61 GB</span></div></td>
  <td><div class="add-block-wrapper"><span class="add-block">Files</span><span>692</span></div></td>
  <td><div class="add-block-wrapper"><span class="add-block">Age</span><span title="09 May 2025">1 year ago</span></div></td>
  <td><div class="add-block-wrapper"><span class="add-block">Seeds</span><span class="text-success">9</span></div></td>
  <td><div class="add-block-wrapper"><span class="add-block">Leechs</span><span class="text-danger">0</span></div></td>
  <td><div class="add-block-wrapper"><span class="add-block">Source</span><a class="source-link-tor">...</a></div></td>
</tr>
```

There are exactly **7 `<td>`** cells, and the numeric ones are keyed by their `span.add-block` **label
text**, not by position — match on the label, so a column reorder degrades to a missing field rather than
a wrong one.

Field mapping for `SearchResult`:

| field | rule |
|---|---|
| `ExternalID` | the `data-id` attribute of `a.search-magnet-btn` |
| `Title` | text of `a.torrent-title-link` **with inner tags stripped** — the query terms come wrapped in `<span>` highlight tags, so the raw inner HTML is not the title |
| `PageURL` | `exttoBaseURL` + the `href` of `a.torrent-title-link` (already absolute-from-root, of the form `/<slug>-<id>/`) |
| `Seeders` | integer in the cell whose label is `Seeds`; commas stripped |
| `PublishedAt` | the `title` attribute of the value span in the `Age` cell, layout `02 Jan 2006`; zero time when absent or unparseable (the visible text is a relative age and must not be parsed) |
| `DownloadURL` | always empty — ext.to has no direct `.torrent`; the magnet is fetched on demand |

**Magnet retrieval — the only signed call.** With `ts` = current unix seconds as a decimal string:

- digest = lowercase hex `sha256("<torrent_id>|<ts>|<searchPageToken>")` — **the search-page token, NOT
  the csrf token**;
- `POST /ajax/getSearchMagnet.php` with form fields `torrent_id`, `hash` (present but empty), `name`
  (present but empty), `timestamp` (= `ts`), `hmac` (= the digest), `sessid` (= the **csrf** token);
- the JSON response is `{"success": true, "url": "magnet:?xt=..."}`. A false or missing `success`, or an
  empty `url`, is an error.

**Golden signature vector — assert against this literal, do not recompute the expectation:**

```
torrent_id=20151803  ts=1760000000  searchPageToken=a33f33f6d813fecdb8e79f5bd3587b6d
hmac = e74f1d43b90e6c4238ae05fb87d9b67b5029f46a384a9336a3f3398069429f84
```

The tokens belong to the most recent search page, so **a magnet fetch must be preceded by a search** for
the same query (warm, ~1s).

## Development Approach

- **Testing approach:** Regular (code first, tests in the same task), matching the repo's existing style.
- Complete each task fully before the next; keep the build green at every task boundary.
- **Every task that changes Go code under `app/` MUST include new/updated tests** (success + error cases)
  as separate checklist items. **The two trailing verification/documentation tasks are exempt.**
- **All tests must pass before starting the next task.**
- **No test may hit the network.** Sources are exercised through fakes and `httptest` servers.
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

**Errors:** wrap with context `fmt.Errorf("load watch: %w", err)` — lowercase, no trailing punctuation,
wrap once at the boundary. Return errors, never panic for expected failures; never discard with `_`.

**Interfaces:** define at the **consumer** side with only the methods that consumer calls; accept
interfaces, return concrete types; inject the concrete type from `main.go`.

**Comments:** default none; add one only when the WHY is non-obvious. Exported items get godoc comments
starting with the name.

**The four numbered checks below are the only failable style criteria.** The prose rules above are
authoring guidance: a reviewer MUST NOT report a finding against them unless one of the numbered checks
fails. This removes the unfalsifiable bars ("is this comment WHY-worthy", "is this helper tiny") that
otherwise keep the review loop from converging.

**Per-task gate (before marking a checkbox `[x]`) — these four checks are the entire mechanical gate; no
other linter is required or consulted:**

1. Build the changed-file list covering **uncommitted work too**, because this gate runs before the task's
   commit:
   `CHANGED=$(git status --porcelain -- '*.go' | awk '{print $NF}'; git diff --name-only $(git merge-base HEAD origin/HEAD) -- '*.go')`.
   If that fails (no `origin/HEAD`, shallow clone), fall back to the union of the `Files:` lists of the
   tasks completed so far. **If `$CHANGED` is empty, skip checks 2 and 4** — never run `gofmt`/`grep` with
   no file arguments, as they then read stdin and report a false pass.
2. `gofmt -s -l $CHANGED` prints nothing, and a whole-repo `gofmt -s -l .` **also prints nothing** — the
   repository is clean on master and must stay that way.
3. `go vet ./...`, `go build ./...`, `go test ./... -race` all exit 0.
4. `grep -nE '^func.*\(.*,.*,.*,.*\)' $CHANGED` — a hit is a violation **only if** the parameter list
   excluding `ctx context.Context` has 4+ entries **and** the signature is not exempt. Ignore matches whose
   commas fall inside the return list, and re-check multi-line signatures by hand only in files the grep
   reported. **Exempt signatures are exactly:** `Search(ctx, query)`, `Magnet(ctx, torrentID, query)`,
   `Evaluate(ctx, w)`, `RunCycle(ctx)`, `Publish(ctx, w, o)`, `Solve(ctx, url)`, and any constructor taking
   a single options struct (`ClientCtx`, `EngineDeps` and the like). Everything else must satisfy the
   three-non-`ctx`-parameter budget.

**Export discipline is checked once, in the verification task, not per task.** Running it per task is
unpassable by construction: `SearchResult` and `SearchSource` are declared in Task 2 but get their first
out-of-package caller only in Task 8. In the verification task, for each exported **top-level type, func
or var** added by this plan (struct fields and interface methods are excluded — they can never appear as
`pkg.Name` yet must stay exported for JSON), run
`grep -rn "<gopkg>\.<Name>" app/ --include="*.go" | grep -v "^app/<dir>/"` and require at least one hit.
Note `<gopkg>` and `<dir>` differ for hyphenated directories: `app/watch-store` declares package
`watch_store`, so the command reads
`grep -rn "watch_store\.Repository" app/ --include="*.go" | grep -v "^app/watch-store/"`.
Identifiers prescribed in Technical Details (`SearchResult`, `SearchSource`, `Watch`, `RunOutcome`,
`Engine`, `SolvedPage`, `SeenRow`, and `watch_store.ErrSchemaNotInitialised`) are pre-approved and exempt.

## Solution Overview

A `watch` is a saved hunt: a list of queries, a set of sources, two regexes, and the set of releases it
has already announced. An hourly cron runs every active watch, merges results across sources and queries,
filters them, and diffs against the seen-set. A non-empty diff is published to NATS, which wakes exactly
one agent turn with the delta.

Three properties carry the design:

1. **The service owns the search.** Both the cron and the reproduction endpoint call the *same* function
   over the *same* stored parameters, so what the agent sees cannot drift from what woke it.
2. **Silence is never manufactured.** A source that fails is an error that lands in the watch's status and
   the health endpoint — never an empty result set that reads as "nothing new".
3. **Losing a release is worse than a duplicate wake.** Hence publish-then-mark ordering, and hence a
   Telegram copy of every hit so a missed re-arm on the agent side is visible the same hour.

## Technical Details

### Fixed constants (single source of truth — no task invents its own)

| constant | value | why |
|---|---|---|
| `defaultWatchCron` | `20 * * * *` | offset from the existing files job at `0 * * * *` so the two never start together |
| `watchIDPattern` | `^[a-z0-9_-]+$` | the id becomes a NATS subject token; a dot or space would corrupt the subject |
| `subjectPrefix` | `tuclaw.releases.found.` | first token must be the literal `tuclaw`; matches the `tuclaw.>` stream |
| `maxPayloadItems` | `10` | keeps the payload far below the 10240-rune clamp |
| `maxPayloadBytes` | `8192` | hard belt-and-braces cap applied after marshalling |
| `jackettSearchTimeout` | `120s` | cold search measured at 40s; leaves headroom for a slow indexer |
| `exttoSearchTimeout` | `180s` | cold challenge measured at 79s |
| `exttoSort` / `exttoOrder` | `size` / `desc` | fixed for both cron and endpoint so results are reproducible |
| `exttoBaseURL` | `https://search.extto.com` | the only host the ext.to source talks to |
| ext.to `cat` / `with_adult` | **not sent** | omitted entirely; this is what the Context measurements used |
| `jackettSearchPath` | `/api/v2.0/indexers/all/results/torznab/api` | appended to the Jackett base; all indexers, one call |
| Jackett `cat` | **not sent** | no category filter — this is what the 1-result / 226-result measurements used |
| `natsPublishTimeout` | `10s` | bound on waiting for the JetStream ack |
| `staleWatchFallback` | `2h` | staleness threshold when the cron expression cannot be parsed |
| `seedOnFirstRun` | `true` | first cycle records without publishing |

### Data model

New migration, new tables only. `files` is untouched.

The migration file goes in **`app/migrations/`**, which is an embedded Go package: `embed.go` there
carries `//go:embed *.sql`, and a one-shot container runs `migrations.Apply` to completion before the app
starts. Dropping the file in that directory is the whole delivery mechanism — there is no separate step,
and **no code may declare these tables a second time**. A constructor that also ran
`CREATE TABLE IF NOT EXISTS` is what let a schema change reach production half-applied; `task-store` no
longer does it and `watch-store` must not start.

```sql
CREATE TABLE watches (
    id            TEXT PRIMARY KEY,
    queries       TEXT NOT NULL,
    include_regex TEXT NOT NULL DEFAULT '',
    exclude_regex TEXT NOT NULL DEFAULT '',
    sources       TEXT NOT NULL DEFAULT 'jackett,extto',
    rev           INTEGER NOT NULL DEFAULT 1,
    seeded_at     TIMESTAMP DEFAULT NULL,
    created_at    TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    expires_at    TIMESTAMP DEFAULT NULL,
    last_run_at   TIMESTAMP DEFAULT NULL,
    last_status   TEXT NOT NULL DEFAULT '',
    disabled_at   TIMESTAMP DEFAULT NULL
);

CREATE TABLE watch_seen (
    watch_id      TEXT NOT NULL,
    source        TEXT NOT NULL,
    external_id   TEXT NOT NULL,
    title         TEXT NOT NULL,
    first_seen_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (watch_id, source, external_id)
);
```

- `queries` and `sources` are stored as JSON arrays / comma-separated text respectively and parsed on load.
- `seeded_at NULL` means the silent seed has not happened yet — that flag, not a row count, decides
  whether a cycle publishes. A watch whose first cycle legitimately found zero matches must still be
  marked seeded, otherwise it would seed silently forever and never announce anything.
- `rev` is incremented on any change to `queries` / `sources` / either regex.
- `last_status` holds the empty string on success and a short error summary otherwise.
- **`watch_seen` is a table, not a JSON column**, so dedup is a point insert with conflict-ignore instead
  of read-modify-write, row growth is bounded per release rather than per watch, and the
  `INSERT OR REPLACE` column-reset trap is structurally impossible here.

### Search abstraction

```go
type SearchResult struct {
    Source      string
    ExternalID  string
    Title       string
    PageURL     string
    DownloadURL string
    Query       string
    Seeders     int
    PublishedAt time.Time
}

type SearchSource interface {
    Name() string
    Search(ctx context.Context, query string) ([]SearchResult, error)
}
```

`SearchResult` is the single currency the engine, the API and the payload all speak. **`Query` is set by
each source to the query that produced the row** — the ext.to magnet call needs it (its signature depends
on tokens from that query's search page) and a watch holds a list of queries, so without this field the
handler cannot know which one to replay.

`Watch` is owned by `app/watcher`; `app/watch-store` imports it and returns it, so the import direction is
watch-store → watcher and never the reverse:

```go
type Watch struct {
    ID            string
    Queries       []string
    Sources       []string
    IncludeRegex  string
    ExcludeRegex  string
    Rev           int
    SeededAt      *time.Time
    ExpiresAt     *time.Time
    LastRunAt     *time.Time
    LastStatus    string
}
```

Regexes are compiled by the engine into a `map[string]*regexp.Regexp` cache keyed by the regex **source
string**, populated at the start of `Evaluate`. Keying on the string rather than on the watch means a
`PATCH` that changes a regex cannot serve a stale compiled value.

An **ad-hoc watch** (from `POST /api/search`) has an empty `ID`. `Evaluate` skips the seen-set lookup
entirely for it and reports every matched item as new.

### Watch repository contract

```go
func (r *Repository) WatchesForCycle() ([]*watcher.Watch, error)
func (r *Repository) GetByID(id string) (*watcher.Watch, error)
func (r *Repository) SeenKeys(watchID string) (map[string]struct{}, error)
func (r *Repository) SeenRows(watchID string) ([]SeenRow, error)
func (r *Repository) MarkSeen(watchID string, results []watcher.SearchResult) error
func (r *Repository) RecordRun(watchID, status string) error
```

`NewRepository` verifies rather than creates, mirroring what `task-store` does today:

```go
var ErrSchemaNotInitialised = errors.New("watch schema not initialised: run the migrate binary (`go run ./cmd/migrate`)")
```

It checks `sqlite_master` for `watches` and `watch_seen` and returns that error when either is absent, so a
skipped migration fails clearly at construction instead of confusingly at the first query.

Three decisions a fresh session must not invent:

- **`SeenKeys` is keyed on `source + "\x00" + externalID`, never on the bare external id.** Both sources
  hand out plain integers (`1883913` from Jackett, `20134504` from ext.to), so a bare-id set would collide
  across sources and silently swallow a real release — the exact loss this design exists to prevent.
- **`WatchesForCycle` filters on `disabled_at IS NULL` only, not on expiry.** Expired rows must still be
  returned so `RunCycle` can skip *and disable* them; a loader that filtered expiry would make the disable
  branch unreachable and `disabled_at` would never be set.

`SeenRow` carries `Source`, `ExternalID`, `Title`, `FirstSeenAt` — it is what `GET /api/watches/{id}`
renders so a silent seed is inspectable.

### Acceptance watch (verbatim — the fixture Task 11 and the tests use)

```
id            one-night-only-en
queries       ["One Night Only 2026", "Только на одну ночь 2026"]
sources       jackett,extto
include_regex (?i)one[ ._-]night[ ._-]only.*2026|только[ ._-]на[ ._-]одну[ ._-]ночь.*2026
exclude_regex (?i)bee gees|def leppard|one desire|streisand|rupaul|top chef|concert|chinese|\b2016\b
```

The separator class `[ ._-]` is deliberate: scene releases use dots where the human title uses spaces, so a
space-only pattern would miss exactly the EN rip this watch exists to catch. **These two patterns were
executed against all six titles below during design — both must-match titles pass and all four must-exclude
titles are rejected.** Do not "simplify" them.

Titles this watch **must match** (an "EN-shaped item" means a title of this form):

- `One.Night.Only.2026.1080p.WEB-DL.DDP5.1.H264-GROUP`
- `Только на одну ночь / One Night Only (2026) TSRip [H.264] [AD]` (the RU release already in Context)

Titles this watch **must exclude**, taken verbatim from the measured 226-result junk set:

- `Bee Gees One Night Only 1998 WEBRip 1080p x264 AAC ENG Lulloz`
- `Def Leppard - One Night Only: Live At The Leadmill [2024, Classic Rock, Hard Rock, Blu-ray, 1080i]`
- `RuPauls Drag Race S15E02 One Night Only Part 2 1080p AMZN WEB DL DDP2 0 H 264 FLUX TGx`
- `One Night Only / Tian Liang Zhi Qian [2016, BDRemux 1080p] VO + DVO + Sub Rus, Eng + Original Chi`

### Solver seam (closes the gap in the prerequisite's `Fetcher`)

The prerequisite plan defines `Fetcher` as `Fetch(ctx context.Context, url string) ([]byte, error)`, and
its solver implementation returns only the FlareSolverr `solution.response`. The cookie and User-Agent are
discarded, but the ext.to source needs both. **Task 3 therefore adds one method to the existing solver
client** — no second FlareSolverr client, no new HTTP plumbing:

```go
type SolvedPage struct {
    Body      []byte
    Cookies   []*http.Cookie
    UserAgent string
}
```

Two edits are needed there, not one. The merged `solverSolution` struct models **only** `Status` and
`Response`, so FlareSolverr's `cookies` and `userAgent` are parsed away today: add those two fields to it
first, then add a `Solve(ctx context.Context, url string) (*SolvedPage, error)` method that returns them
alongside the body. `Fetcher` itself is unchanged, so every existing caller is untouched. `app/watcher`
declares the consumer-side interface it needs (a single `Solve` method) and `main.go` injects the concrete
solver. The ext.to source calls `Solve` **only** on cookie refresh; all warm-path requests go through its
own `*http.Client` carrying the returned cookies and the exact returned User-Agent.

If the solver is unconfigured, the prerequisite's blocked-fetcher behaviour applies: the ext.to source is
constructed disabled and reports a configuration error rather than silently returning zero rows.

### Jackett source

Torznab over HTTP. The request is exactly:

```
GET {jackettBase}{jackettSearchPath}?apikey={JACKETT_API_KEY}&t=search&q={url-encoded query}
```

`jackettBase` is `JACKETT_URL` reduced to scheme+host+any base path, with anything from `/api/v2.0/`
onward stripped (the existing `NewJackettProvider` already normalises a base this way). No `cat`, no
`limit` — those are the conditions the Context measurements were taken under.

Requirements a fresh session cannot rediscover:

- **Every** `<item>` is mapped, not just the first — this is a search, not a single-release parse.
- `PageURL` ← `<comments>`; fall back to `<guid>` only when `<comments>` is empty or not http(s).
- `DownloadURL` ← `<link>` (or `<enclosure url>` when `<link>` is empty), **with its scheme+host rewritten
  to the configured public Jackett base**. Jackett emits its own internal base URL here; shipping it
  unrewritten produces a download link that resolves nowhere and fails only later, at download time.
- `Seeders` ← the `torznab:attr` element whose `name` attribute is `seeders`. The existing structs do not
  model `torznab:attr` at all; extend them. **Trap:** `encoding/xml` matches on the namespace *URL*, not
  the prefix, so the struct tag must be `xml:"http://torznab.com/schemas/2015/feed attr"` (or the bare
  `xml:"attr"`). A tag written as `xml:"torznab:attr"` compiles, matches nothing, and yields zero seeders
  silently.
- `ExternalID` ← the `t` query parameter of `PageURL`; when absent, the raw `<guid>` string.
- `PublishedAt` ← `<pubDate>`, RFC1123Z with an RFC1123 fallback (the existing parser already does this).
- A non-2xx response or unparseable XML is an **error**, never an empty slice.

### ext.to source

Uses the **Solver seam** above for the cookie refresh and its own `*http.Client` for everything else. It
owns the cookie / User-Agent / page-token state in memory, guarded by a mutex, because the cron job and
the HTTP handlers share one instance.

- `Search` issues the `/browse/` GET specified in the wire-protocol section, parses rows with the field
  mapping given there, sets `Query` on every result, and caches both page tokens for a subsequent magnet
  fetch.
- On a challenge response (detected by the two body markers) it refreshes the cookie through the solver
  **once** and retries the GET; a second challenge is an error. A signed POST is never auto-retried.
- Magnet retrieval implements the signed POST specified in the wire-protocol section. Its contract:

```go
func (s *exttoSource) Magnet(ctx context.Context, torrentID, query string) (string, error)
```

  `query` is required because the signature needs tokens from a search page; the method performs the
  search itself when it holds no fresh tokens.
- **State is per-process and shared.** Because both the cron and the endpoints use the same instance, an
  agent search moments after a cron cycle reuses a warm cookie (~1s instead of ~79s).

### Watcher engine

The engine is the piece both the cron and the reproduction endpoint call, so neither can drift from the
other. That claim is not left as prose: Task 9 ships a test running both paths over one scripted source and
asserting the two `Matched` slices are deep-equal.

```go
type RunOutcome struct {
    Matched []SearchResult
    New     []SearchResult
    Raw     []SearchResult
    Errs    []error
}

func (e *Engine) Evaluate(ctx context.Context, w Watch) RunOutcome
func (e *Engine) RunCycle(ctx context.Context) error
```

`Evaluate` is side-effect free — it searches, merges and filters, and reports what it found. `RunCycle`
loads `WatchesForCycle()` and, for each, calls `Evaluate` and then applies the effects (publish, mark,
status). "Active" in the health section means not disabled and not expired; `WatchesForCycle` is looser on
purpose (see the repository contract).
The endpoint calls `Evaluate` only.

Ordering inside `Evaluate`:

1. For each source in the watch, for each query, **sequentially** — FlareSolverr must not be parallelised
   and Jackett must not be hammered. Each source call gets its own timeout from the constants table.
2. A source error is appended to `Errs` and that source contributes nothing; it does **not** abort the
   other sources. `Raw`/`Matched` from a partially failed run are still returned, but the caller must not
   treat a run with a non-empty `Errs` as an authoritative "nothing new".
3. Merge and dedup by `(Source, ExternalID)`.
4. Filter into `Matched`: `include_regex` must match `Title` when set; `exclude_regex` must not match when
   set. Empty regex passes everything. Regexes are compiled once at load, not per item.
5. `New` = `Matched` minus what `watch_seen` already holds.

`RunCycle` publishes through a consumer-side interface declared next to the engine:

```go
type publisher interface {
    Publish(ctx context.Context, w Watch, o RunOutcome) error
}
```

Payload construction lives **inside** the publisher, not in the engine. Task 4 ships the interface plus a
fake implementation and tests the ordering against it; the real JetStream implementation arrives in Task 5
and is wired in Task 6.

Effects applied by `RunCycle`, in this exact order:

1. If the watch is not yet seeded (`seeded_at IS NULL`): write all of `Matched` to `watch_seen`, publish
   **nothing**, and skip to step 5. Rationale: a fresh watch on the acceptance title would otherwise
   immediately wake the agent with the RU TSRip it already downloaded. **Set `seeded_at` only when `Errs`
   is empty** — seeding from a partially failed run would bury releases the failed source never reported.
2. Otherwise, if `New` is non-empty: **publish to NATS first, then write `watch_seen`.** A crash between
   the two costs one duplicate wake next cycle; the reverse loses the release permanently and silently,
   which stalls the whole downstream flow with no signal anywhere.
3. A non-empty `Errs` does **not** suppress publishing — a partial run can still have found a real
   release, and withholding it would be the silent failure this design exists to prevent. It does mean
   `last_status` is non-empty, which is what health and the operator see.
4. If publishing fails: do not mark seen, record the error in `last_status`, retry next cycle.
5. Update `last_run_at` and `last_status` on **every** branch — including the seed branch and the
   expired-watch branch — with an empty `last_status` on a clean run and a short summary naming the failed
   sources otherwise. A cycle that touched a watch always leaves a timestamp; that is what the health
   staleness check reads.

**Items filtered out are never written to `watch_seen`**, so loosening a regex later resurfaces them.

A watch past `expires_at` is skipped and disabled.

### NATS notification

Subject: `tuclaw.releases.found.<watch_id>`.

Payload shape (identity and delta only — **never the search parameters**, see Rejected alternatives):

```json
{"watch_id":"...","watch_rev":3,"found_at":"...","total":226,"matched":1,
 "new":[{"source":"jackett","id":"1883913","title":"..."}]}
```

- `total` = `len(Raw)`, i.e. the count **after** cross-source/cross-query dedup but **before** regex
  filtering; `matched` = `len(Matched)`. Both are always reported so a truncated `new` list is never
  mistaken for the whole story. `found_at` is RFC3339 in UTC.
- `new` is capped at `maxPayloadItems`, and after marshalling the payload is re-checked against
  `maxPayloadBytes`, dropping further items until it fits.
- Publishing uses JetStream so the ack proves durability before anything is marked seen; the publish call
  is bounded by `natsPublishTimeout`. The stream is **owned by the tuclaw daemon** — connect and publish
  only; **never create or reconfigure a stream**, and treat "no stream matches subject" as an ordinary
  error return.
- Set a JetStream message id of `<watch_id>:<key>`, where `key` is the **lexicographic maximum of
  `Source + ":" + ExternalID` over `New`**, so that a duplicate publish caused by the deliberate
  publish-then-mark ordering is collapsed by the stream's dedup window instead of waking the agent twice.
  The rule must be order-independent and deterministic — "the newest item" would not be, since
  `PublishedAt` is the zero time for any ext.to row whose `Age` cell carries no `title` attribute.
- **Connect must not be fatal.** Dial with retry-on-failed-connect and unlimited reconnects and a short
  dial timeout, log a failed initial connect and **start anyway**; while disconnected, `Publish` returns an
  error, so nothing is marked seen and the release is retried next cycle. A hard failure here would put the
  container into a crash loop every time NATS restarts, which contradicts "degrade, not die".
- An empty NATS URL disables publishing entirely (the service must still start and still run cycles);
  log a warning once at startup, and treat a would-be publish as an error that prevents marking seen.

### Telegram mirror

Every published hit is also sent to the existing admin channel. **The send must be non-blocking** —
`messagesForSend` is unbuffered, so a blocking send would wedge the whole cron behind a stalled reader.
Drop with a warning log if the channel is not ready.

Reason this exists at all: an `event` task fires once and completes, so the agent must re-arm after each
wake. If it forgets, the watch keeps publishing to nobody, and that silence is indistinguishable from
"no releases" — the Telegram copy makes the failure visible the same hour.

### HTTP API

| endpoint | behaviour |
|---|---|
| `POST /api/watches` | create. Validates the id against `watchIDPattern`, requires at least one non-empty query, compiles both regexes and rejects invalid ones with 400, rejects unknown source names, rejects a duplicate id with 409 |
| `GET /api/watches` | list, including `last_run_at`, `last_status`, `seeded_at`, `rev` |
| `GET /api/watches/{id}` | one watch plus its `watch_seen` rows, so a silent seed is inspectable |
| `PATCH /api/watches/{id}` | edit queries/sources/regexes; **increments `rev`**; re-validates as on create |
| `DELETE /api/watches/{id}` | sets `disabled_at` (soft delete, mirroring how `files` are removed) |
| `POST /api/watches/{id}/search` | re-runs **this watch's own** search via `Evaluate` with the stored parameters and returns the full results — `PageURL`, host-rewritten `DownloadURL`, and for ext.to the magnet — each flagged `new` or already-seen. `?raw=true` returns the pre-filter set instead, which is the only way to debug "why was I woken with this junk" and "why was I not woken" |
| `POST /api/search` | ad-hoc search where the caller supplies queries, sources and optional regexes. Deliberately a **separate** endpoint: conflating it with the watch reproduction is exactly how the two would drift apart |

Request bodies — these field names are a contract with the agent in another repository, so they are pinned
here rather than left to the implementation:

```json
POST /api/watches   {"id":"...","queries":["..."],"sources":["jackett","extto"],
                     "include_regex":"","exclude_regex":"","expires_at":null}
PATCH /api/watches/{id}  same fields minus "id", all optional
POST /api/search    {"queries":["..."],"sources":["..."],"include_regex":"","exclude_regex":""}
```

The magnet for an ext.to item is resolved through a consumer-side seam declared in `app/http`, because
`Magnet` is a method on the ext.to source and not part of the engine's API:

```go
type magnetResolver interface {
    Magnet(ctx context.Context, torrentID, query string) (string, error)
}
```

It is carried as a field on the http options struct and injected from `main.go` with the concrete ext.to
source. When the source is disabled the field is nil and every ext.to item comes back with an empty
`magnet` and a fixed `magnet_error` — never a failed request.

Both search endpoints are slow by nature (a cold source can take over a minute). They must not be
cancelled by an impatient client mid-cycle in a way that corrupts source state; use the request context
for the search but keep the source's own state consistent on cancellation.

**Explicit non-guarantee, documented in `README.md` by Task 12:** full idempotency is impossible — a
release can be pulled between the cron tick and the agent's call, and the Jackett cache expires after
35 minutes. What is guaranteed is **query reproducibility**. An id present in the payload's `new` but
absent from the search response means "the release was taken down" and is normal behaviour, not an error.

### Health

The prerequisite plan leaves `/api/health` returning a status field with the vocabulary
**`ok | degraded | unhealthy`**, evaluated in a first-match-wins order (`unhealthy` also returns HTTP 503;
the other two return 200). Severity order is `ok < degraded < unhealthy`. Those facts are restated here so
this section is judgeable without opening the other plan.

Add a `watches` object alongside the existing keys: active watch count, the oldest `last_run_at` across
active watches, and how many active watches carry a non-empty `last_status`.

Status contribution — the watcher check may only move `ok` to `degraded`. It **must never** downgrade an
existing `unhealthy` or `degraded` to something less severe, and it never produces `unhealthy` itself:

- watcher cycle staler than the threshold ⇒ `degraded`,
- one or more active watches with a non-empty `last_status` ⇒ `degraded`,
- one or more active watches with `last_run_at IS NULL` **and** the process has been up longer than the
  threshold ⇒ `degraded`.

Watches with `last_run_at IS NULL` are excluded from the oldest-run computation (otherwise a watch created
via the API 30 seconds ago reports the service degraded before its first tick can possibly have run).
A service with zero watches is `ok`, not degraded.

The threshold is computed **once in `main.go`**, never in the handler, mirroring how the prerequisite
computes its own: parse `WATCH_CRON` with the standard 5-field cron parser (`cron.ParseStandard`; gocron
runs with seconds disabled, so a seconds-enabled parser rejects the default expression), take
`first := sched.Next(now)` and `second := sched.Next(first)`, and use `2 * second.Sub(first)`. On a parse
error use `staleWatchFallback` and log at WARN so a bad `WATCH_CRON` is visible. This promotes
`github.com/robfig/cron/v3` to a direct dependency if the prerequisite has not already done so.

The prerequisite converts `http.NewClient` to a `ClientCtx` options struct; the watcher's dependencies
(watch store, engine, staleness threshold) are **added as fields to that existing struct**, not as new
positional parameters. That struct is pre-approved under the signature exception.

### Configuration

| env | purpose |
|---|---|
| `JACKETT_API_KEY` | required to search at all; the existing `JACKETT_URL` carries no key |
| `JACKETT_PUBLIC_URL` | base used to rewrite the internal host out of `DownloadURL`; defaults to `JACKETT_URL` |
| `NATS_URL` | e.g. `nats://nats:4222`; **empty disables publishing** |
| `WATCH_CRON` | defaults to `defaultWatchCron` |

The FlareSolverr URL is already introduced by the prerequisite plan; reuse that config value rather than
adding a second one.

## Testing Strategy

- **Unit tests are required in every code task** (success + error paths), as separate checklist items.
- **No network access in tests.** Jackett is exercised against an `httptest` server serving a torznab
  fixture (including the internal-host `<link>` so the rewrite is actually asserted); ext.to likewise
  against fixture HTML and a fake solver; NATS publishing goes through a small publisher interface with a
  fake implementation.
- The engine is tested with a **fake `SearchSource`** whose results and errors are scripted per call.
- DB-backed tests use the prerequisite plan's repository test harness; if none exists, create a local
  helper in the new package that opens a temporary file-backed SQLite database and applies the schema.
- **Race detector on**: `go test ./... -race`, because the ext.to source state is shared between the cron
  and HTTP handlers.

## Progress Tracking

- Mark completed items `[x]` immediately when done.
- New tasks discovered during implementation get a `➕` prefix; blockers get `⚠️`.
- Update this plan when the scope changes.

## What Goes Where

- **Implementation Steps** (`[ ]`): everything achievable inside this repository — code, tests, docs.
- **Post-Completion** (no checkboxes): work in the tuclaw scheduler and the agent's skills, which lives in
  other repositories and on other hosts.

## Implementation Steps

### Task 1: Watch schema, migration and repository

**Files:**
- Create: `app/migrations/<timestamp>-add-watches.sql`
- Create: `app/watcher/source.go`
- Create: `app/watch-store/repository.go`
- Create: `app/watch-store/repository_test.go`

- [ ] declare `Watch` in `app/watcher/source.go` exactly as in Technical Details — it is declared here,
      before the repository, because the repository returns it and the mandated import direction is
      watch-store → watcher (Task 2 adds `SearchResult` and `SearchSource` to the same file)
- [ ] add the migration creating `watches` and `watch_seen` exactly as specified in Technical Details
      (`-- +migrate Up` / `-- +migrate Down` sections, matching the existing migration files) **into
      `app/migrations/`** — that directory is an embedded Go package, so the new file is picked up by the
      `//go:embed *.sql` in `app/migrations/embed.go` and applied by the migrate container automatically
- [ ] create `app/watch-store/repository.go` with a `Repository` holding the existing `*database.Client`.
      **It must NOT create tables.** Migrations are the single source of schema; a constructor that also
      declared it is exactly the duplication that shipped a broken production schema. Instead mirror the
      current `task-store`: verify the schema and return a sentinel error when it is absent
- [ ] implement the methods with the signatures in "Watch repository contract", plus `Create`, `GetAll`,
      `Update` (bumps `rev`), `Disable` and `MarkSeeded`
- [ ] key `SeenKeys` on `source + "\x00" + externalID` — a bare-id set collides across sources
- [ ] make `WatchesForCycle` filter on `disabled_at IS NULL` **only**, so expired rows still reach
      `RunCycle` and can be disabled there
- [ ] use **explicit `UPDATE` statements only** — `INSERT OR REPLACE` is forbidden here (see Context)
- [ ] parse/serialize `queries` as a JSON array and `sources` as comma-separated text at the repository
      boundary, so callers work with typed slices
- [ ] add the DB test harness for this package, mirroring `newTestRepo` in
      `app/task-store/repository_test.go`: `t.Chdir(t.TempDir())`, `database.NewClient`, then
      **`migrations.Apply(db.DB())`** — tests reach their schema by the same path production does, so a
      migration that forgets a column fails the suite instead of only failing on deploy
- [ ] write a test asserting the constructor returns the sentinel error on a database with no watch tables
- [ ] write tests for round-tripping a watch, `rev` incrementing on `Update`, `WatchesForCycle` excluding
      disabled rows but **including** expired ones, and `MarkSeen` being idempotent on a repeated insert
- [ ] write a test asserting two results with the same external id from different sources are two distinct
      `SeenKeys` entries
- [ ] write tests for error cases: unknown id, malformed stored JSON in `queries`
- [ ] run tests — must pass before Task 2

### Task 2: SearchSource interface and the Jackett source

**Files:**
- Modify: `app/watcher/source.go`
- Create: `app/watcher/jackett.go`
- Create: `app/watcher/jackett_test.go`
- Modify: `app/config/config.go`
- Modify: `app/config/config_test.go`

- [ ] add `SearchResult` (including `Query`) and `SearchSource` to `app/watcher/source.go` exactly as
      prescribed in Technical Details (`Watch` is already there from Task 1)
- [ ] add `JACKETT_API_KEY` and `JACKETT_PUBLIC_URL` to `JackettConfig` (the latter defaulting to the
      existing URL when empty) and extend the config test
- [ ] implement the Jackett source using the exact request template in Technical Details
      (`jackettSearchPath`, `apikey`, `t=search`, `q`; no `cat`, no `limit`), map **all** `<item>`
      elements, apply the field mapping rules, and set `Query` on every result
- [ ] extend the torznab structs to parse `torznab:attr` with the **namespace-URL** struct tag so
      `Seeders` is populated (see the trap in Technical Details)
- [ ] rewrite the scheme+host of `DownloadURL` to the configured public base, preserving path and query
- [ ] treat a non-2xx response or unparseable XML as an error, never as an empty result set
- [ ] write tests against an `httptest` server with a torznab fixture containing several items, asserting
      the count, `PageURL` from `<comments>`, seeders from `torznab:attr`, and the external id from `t=`
- [ ] write a test whose `httptest` handler asserts on `r.URL.Path` and the full query string, so a wrong
      endpoint or a missing `t=search` fails the test rather than only production
- [ ] write a test asserting the internal host in `<link>` is rewritten to the public base
- [ ] write tests for error cases: non-2xx, malformed XML, item with neither `<comments>` nor `<guid>`
- [ ] run tests — must pass before Task 3

### Task 3: ext.to source — search and signed magnet

**Files:**
- Create: `app/watcher/extto.go`
- Create: `app/watcher/extto_test.go`
- Create: `app/watcher/testdata/extto_browse.html`
- Modify: the solver implementation under `app/tracker/providers/` (adds the `Solve` method)

- [ ] add `Cookies` and `UserAgent` to the existing `solverSolution` struct — it currently parses only
      `Status` and `Response`, so FlareSolverr's cookie and UA are discarded and `Solve` would have
      nothing to return
- [ ] add `SolvedPage` and the `Solve(ctx, url) (*SolvedPage, error)` method to the existing solver in
      `app/tracker/providers` as specified in "Solver seam"; leave `Fetcher` and all its callers unchanged
- [ ] implement the ext.to source holding cookie, User-Agent and both page tokens behind a mutex, calling
      the consumer-side solver interface only on cookie refresh and using its own `*http.Client` otherwise
- [ ] implement `Search` as the `/browse/` GET in the wire-protocol section, sending the exact headers
      listed there, parsing rows with the given field mapping, setting `Query`, and caching both tokens
- [ ] detect a challenge by the two body markers, refresh the cookie once and retry the GET; a second
      challenge is an error and a signed POST is never auto-retried
- [ ] implement `Magnet` with the signed POST: hex sha256 of `id|ts|searchPageToken`, `sessid` = the csrf
      token, `hash` and `name` present but empty; performing a search first when no fresh tokens are held,
      and treating a false/missing `success` or an empty `url` as an error
- [ ] create `testdata/extto_browse.html` by copying the verbatim row markup from the wire-protocol
      section, repeated three times with different ids, titles and seed counts — **do not invent markup**
- [ ] write tests for row parsing against the fixture: id from `data-id`, title with highlight `<span>`
      tags stripped, page URL, seeders matched by the `Seeds` label, `PublishedAt` from the `Age` cell's
      `title` attribute
- [ ] write a test asserting the signature against the golden vector in the wire-protocol section
      (assert the literal 64-hex string; do not recompute the expectation) and that the form carries all
      six fields
- [ ] write tests for error cases: challenge twice, HTML with no matching rows, magnet response with
      `success:false`, solver unconfigured
- [ ] run tests — must pass before Task 4

### Task 4: Watcher engine — merge, filter, delta

**Files:**
- Create: `app/watcher/engine.go`
- Create: `app/watcher/engine_test.go`

- [ ] implement `Engine` holding the source set and the watch store, with `Evaluate` and `RunCycle` as
      prescribed in Technical Details
- [ ] declare the consumer-side `publisher` interface here and ship a **no-op/fake implementation only**;
      the real JetStream publisher is Task 5 and its wiring is Task 6
- [ ] implement `Evaluate`: sequential per source and per query with per-source timeouts, error collection
      that never aborts the remaining sources, dedup by `(Source, ExternalID)`, regex filtering through the
      compiled-regex cache keyed on the regex source string, and the `New` diff against the seen set
- [ ] make `Evaluate` skip the seen-set lookup entirely for an ad-hoc watch (empty `ID`) and report every
      matched item as new
- [ ] implement `RunCycle` covering **effects 1 and 5 only** — the silent seed keyed on `seeded_at`, the
      "only seed when `Errs` is empty" rule, the skip-and-disable of expired watches, and the always-write
      of `last_run_at`/`last_status`. **Publish-then-mark (effects 2-4) is Task 6**; here `RunCycle` calls
      the fake publisher so the ordering can be tested, and Task 6 swaps in the real one
- [ ] add a fake `SearchSource` in the test file whose per-call results and errors are scripted
- [ ] write tests for the acceptance scenario using the **Acceptance watch** block verbatim: first cycle
      seeds without publishing; a subsequent cycle with one new EN-shaped title yields exactly one item in
      `New`; a repeat cycle yields none
- [ ] write tests for filtering using the literal must-match and must-exclude titles from the Acceptance
      watch block, plus a case with both regexes empty passing everything
- [ ] write tests for error cases: a failing source contributes an error and does not clear the delta of
      the other source; a first cycle with a source error does **not** set `seeded_at`; a clean first cycle
      that matches nothing **does** set it
- [ ] write a test asserting filtered-out items are absent from the seen set afterwards
- [ ] write a test asserting an ad-hoc watch with an empty `ID` reports all matched items as new
- [ ] write a test asserting an expired watch is skipped without being searched and ends with `disabled_at`
      set
- [ ] run tests — must pass before Task 5

### Task 5: NATS publisher

**Files:**
- Create: `app/watcher/publisher.go`
- Create: `app/watcher/publisher_test.go`
- Modify: `app/config/config.go`
- Modify: `app/config/config_test.go`
- Modify: `go.mod`, `go.sum`

- [ ] add the NATS client dependency (`go get github.com/nats-io/nats.go`) and add `NATS_URL` to config
- [ ] implement the concrete JetStream publisher satisfying the `publisher` interface from Task 4, with
      payload construction living inside the publisher
- [ ] connect with retry-on-failed-connect, unlimited reconnects and a short dial timeout; a failed initial
      connect is logged and the service starts anyway (see Technical Details — a fatal connect would
      crash-loop the container whenever NATS restarts)
- [ ] build the payload exactly as prescribed, capping `new` at `maxPayloadItems` and re-trimming after
      marshalling to respect `maxPayloadBytes`, always reporting `total` (post-dedup, pre-filter) and
      `matched`, with `found_at` as RFC3339 UTC
- [ ] publish to `subjectPrefix + watch.ID` under `natsPublishTimeout`, set the message id to
      `<watch_id>:<newest external id>` for stream-side dedup, and require the ack before returning
      success; **never create or reconfigure a stream**
- [ ] make an empty `NATS_URL` disable publishing: warn once at startup and return an error from the
      publish call so the engine does not mark anything seen
- [ ] write tests for payload construction: field values, item cap, byte cap trimming, `total`/`matched`
      preserved when items are dropped, `found_at` format
- [ ] write tests for subject and message-id construction from the watch id
- [ ] write tests for error cases: publish failure surfaces as an error; disabled publisher returns an
      error rather than silently succeeding; a construction-time connect failure does not return a fatal
      error from the constructor
- [ ] run tests — must pass before Task 6

### Task 6: Effects wiring — publish-then-mark and the Telegram mirror

**Files:**
- Modify: `app/watcher/engine.go`
- Modify: `app/watcher/engine_test.go`

- [ ] wire the publisher into `RunCycle` with the prescribed ordering: publish first, mark seen only after
      a successful publish, and leave the seen set untouched when publishing fails
- [ ] send a short human-readable summary of each published hit to the admin message channel using a
      **non-blocking** send that drops with a warning when the channel is not ready
- [ ] record `last_run_at` and `last_status` for every cycle, clean or not
- [ ] write a test asserting that a failing publish leaves the seen set unchanged and the item is
      re-published on the next cycle
- [ ] write a test asserting a successful publish marks exactly the published items as seen
- [ ] write a test asserting the cycle completes when nobody is reading the message channel (no deadlock)
- [ ] run tests — must pass before Task 7

### Task 7: Second cron job and composition-root wiring

**Files:**
- Modify: `app/schedular/schedular.go`
- Create: `app/schedular/schedular_test.go`
- Modify: `app/main.go`
- Modify: `app/config/config.go`
- Modify: `app/config/config_test.go`

- [ ] split the scheduler API into `AddJob(name, cronExpr string, cb func()) error` and `Start()`, so more
      than one job can be registered; the existing files job becomes `AddJob("files", cfg.Cron, ...)`
- [ ] register **every** job with gocron's singleton mode. This is correctness-critical and non-obvious: a
      watcher cycle is sequential over sources and queries with 120s/180s per-source timeouts, so it can
      easily outlast its tick — without singleton mode gocron starts an overlapping cycle that publishes
      twice and races the shared ext.to cookie/token state
- [ ] add `WATCH_CRON` to config with `defaultWatchCron` as its default
- [ ] construct the watch store, the two sources, the publisher and the engine in `main.go` and register
      the watcher cycle as the second job, injecting concrete types into consumer-side interfaces
- [ ] pass the existing `messagesForSend` channel into the engine (a field on its options struct) —
      without it the Telegram mirror from Task 6 never fires, and a nil channel would make the
      non-blocking send succeed silently in tests while doing nothing in production
- [ ] make the watcher callback log a cycle error and continue — only a **registration** error is fatal
      and goes to the existing scheduler error channel
- [ ] make a missing Jackett api key or a missing solver disable the corresponding source with a startup
      warning instead of failing to start — the service must degrade, not die
- [ ] write tests for registering two jobs, for singleton mode being set, and for an invalid cron
      expression surfacing as a registration error
- [ ] write tests for the degraded-construction paths and name them exactly `TestDegradedNoJackettKey`,
      `TestDegradedNoSolver`, `TestDegradedNoNATS` so Task 11 can require them by name
- [ ] run tests — must pass before Task 8

### Task 8: Watch CRUD endpoints

**Files:**
- Modify: `app/http/client.go`
- Modify: `app/http/client_test.go`
- Modify: `app/main.go`

- [ ] add the watch store to the existing `ClientCtx` options struct as a new field (do **not** add
      positional parameters) and pass it from `main.go`
- [ ] add the five CRUD routes from the API table, defining a consumer-side watch-store interface in the
      http package with only the methods these handlers call
- [ ] validate on create and update: id against `watchIDPattern`, at least one non-empty query, both
      regexes compiling, known source names only; return 400 with a specific message per failure and 409
      on a duplicate id
- [ ] make `GET /api/watches/{id}` include the watch's seen rows so a silent seed is inspectable
- [ ] make `DELETE` a soft delete setting `disabled_at`
- [ ] write tests for create success and for each validation failure (bad id charset, no queries, invalid
      regex, unknown source, duplicate id)
- [ ] write tests for list, get-with-seen-rows, update bumping `rev`, and soft delete
- [ ] run tests — must pass before Task 9

### Task 9: Search endpoints

**Files:**
- Modify: `app/http/client.go`
- Modify: `app/http/client_test.go`
- Modify: `app/main.go`

- [ ] add the engine **and** the `magnetResolver` seam to the existing `ClientCtx` options struct as new
      fields and pass both from `main.go` (the resolver is the concrete ext.to source, nil when disabled)
- [ ] add `POST /api/watches/{id}/search` calling the engine's `Evaluate` with the stored watch, returning
      the full results with each item flagged as new or already-seen
- [ ] emit each response item with exactly these keys: `source`, `id`, `title`, `page_url`,
      `download_url`, `magnet`, `magnet_error`, `seeders`, `published_at`, `new`
- [ ] resolve the magnet for ext.to items sequentially via `Magnet(ctx, item.ExternalID, item.Query)`,
      reusing the warm token cache; on failure set `magnet_error` on that item, leave `magnet` empty, and
      do not fail the request
- [ ] support `?raw=true` returning the pre-filter set
- [ ] add `POST /api/search` taking queries, sources and optional regexes from the request body, building
      an ad-hoc `Watch` with an empty `ID` and sharing the same `Evaluate` path
- [ ] return 404 for an unknown or disabled watch id
- [ ] write tests asserting the watch-search endpoint applies the stored regexes (the junk-heavy title set
      from the Acceptance watch block collapses to the matching item) and that `?raw=true` returns the
      unfiltered set
- [ ] write the equality test that backs the "cannot drift" claim, named `TestCronAndEndpointAgree`:
      pre-seed the watch so `RunCycle` takes the publish branch, run it with the fake publisher and capture
      `o.Matched` from the `Publish` call, then POST the endpoint against the same scripted source; project
      both sides to `[]struct{Source, ID, Title string}` in slice order and compare with `reflect.DeepEqual`
      (the endpoint returns JSON objects of a different shape, so the projection is the comparison)
- [ ] write tests asserting items already in the seen set are flagged as not new
- [ ] write tests for error cases: unknown watch id, a source error surfacing in the response, a per-item
      magnet failure not failing the request
- [ ] run tests — must pass before Task 10

### Task 10: Health reporting for watches

**Files:**
- Modify: `app/http/client.go`
- Modify: `app/http/client_test.go`
- Modify: `app/main.go`

- [ ] add the `watches` object to the health payload: active count, oldest `last_run_at` (excluding NULLs),
      count of watches with a non-empty `last_status`
- [ ] compute the staleness threshold **in `main.go`** with `cron.ParseStandard(cfg.WatchCron)` and
      `2 * (sched.Next(sched.Next(now)) - sched.Next(now))`, falling back to `staleWatchFallback` with a
      WARN log on a parse error, and pass it in through `ClientCtx`
- [ ] contribute `degraded` for a stale cycle, for any active watch with a non-empty `last_status`, and for
      an active watch with `last_run_at IS NULL` once the process has been up longer than the threshold
- [ ] ensure the watcher check can only move `ok` to `degraded` and never lowers an existing
      `degraded`/`unhealthy`
- [ ] make zero watches report `ok`
- [ ] write tests for: `ok` with no watches, `ok` with fresh watches, `degraded` on a stale cycle,
      `degraded` on a watch carrying an error status
- [ ] write a test asserting a freshly created watch with `last_run_at IS NULL` reports `ok` before the
      threshold elapses and `degraded` after
- [ ] write a test asserting the watcher check leaves an `unhealthy` status untouched
- [ ] run tests — must pass before Task 11

### Task 11: Verify acceptance criteria

- [ ] verify the acceptance scenario at test level against the **Acceptance watch** block verbatim: it
      seeds silently on its first cycle, publishes exactly once when a title from the must-match list
      appears, publishes nothing on a repeat, leaves every title from the must-exclude list unpublished,
      and leaves `last_status` non-empty when a source fails
- [ ] confirm no test reaches the network:
      `grep -rnE 'http\.Get|http\.Post|net\.Dial|http\.DefaultClient' app/ --include="*_test.go"` prints
      nothing outside `httptest` usage
- [ ] confirm the degraded-startup paths actually ran, by name — `go test ./app/... -run
      'TestDegradedNoJackettKey|TestDegradedNoSolver|TestDegradedNoNATS' -v` reports three `=== RUN` lines
      and passes. A bare `-run Degraded` would exit 0 even if the tests were never written, so require the
      count. Do **not** attempt to boot the binary: `main.go` also needs Telegram, DB and qBittorrent
      settings unrelated to this plan
- [ ] run the deferred export-discipline check described in Code-Quality Rules over every exported
      top-level type/func/var added by Tasks 1-10
- [ ] run the full suite: `go test ./... -race`
- [ ] run `go vet ./...` and `go build ./...`
- [ ] confirm `gofmt -s -l .` prints nothing

### Task 12: Update documentation and close out

**Files:**
- Modify: `CLAUDE.md`
- Modify: `README.md`
- Modify: `compose.yaml`

- [ ] update `CLAUDE.md`: the new `watcher` and `watch-store` packages, the second cron job, the new env
      vars, and the watch endpoints
- [ ] update `README.md`: add the watch endpoints to its HTTP API section and
      `JACKETT_API_KEY` / `JACKETT_PUBLIC_URL` / `NATS_URL` / `WATCH_CRON` to its configuration section
- [ ] document the idempotency non-guarantee in `README.md` in these words: an id present in a NATS
      payload's `new` list but absent from `POST /api/watches/{id}/search` means the release was taken
      down, and is not an error
- [ ] update `compose.yaml` with the new environment variables
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

*Items requiring external systems — informational only, no checkboxes.*

**The Go work alone saves zero tokens.** The four condition-less scheduled tasks keep waking the LLM until
the tuclaw side is rewired. That follow-up work lives in the agent-plugin repository and the tuclaw
scheduler:

- create a watch per hunt via `POST /api/watches`, then replace each of the four tasks (the `poll_until 3h`
  EN-reference hunt, the two `poll_until 168h` hunts, and the weekly House of the Dragon cron) with an
  `event` task subscribed to `tuclaw.releases.found.<watch_id>`
- write the agent's wake prompt so it calls `POST /api/watches/{id}/search` for details rather than
  re-running its own searches, verifies the candidate through defuddle (`Перевод:` / `Аудио N:` /
  `Реклама:` — the title lies), downloads through `POST /api/downloads`, and then **re-arms the event
  task**, since an `event` task fires once and completes
- document in the agent's search skill that *monitoring* now goes through the watcher while
  *interactive, user-initiated* search is unchanged
- confirm `NATS_URL` reaches the container (same docker network as the NATS container) and that the
  Jackett api key and public base URL are present in the deployed environment

**Manual verification:**

- create the acceptance watch against the live service and confirm the first cycle seeds silently
- confirm a published hit arrives both on the NATS subject and in Telegram
- confirm the uptime monitor turns degraded when a watch is left in an error state
