# Download and release-update notifications over NATS

## Overview

The tuclaw agent drives this service over HTTP and is woken by NATS. Two gaps make it burn cycles
guessing:

1. It cannot learn that a tracked tracker topic got a **new magnet** (a serial posting the next
   episode). The sweep already detects that and re-downloads, but the only notification is a
   Telegram message a human reads.
2. It cannot learn that a **qBittorrent download finished**. `POST /api/downloads` is
   fire-and-forget and answers `{"status":"ok"}` before a single byte is transferred, so the agent
   has no way to know when the files are on disk and it can start processing them.

Both are closed by opt-in notifications published to the existing `TUCLAW` JetStream stream, on two
new subjects. Opt-in is a per-request flag, so nothing that exists today changes behaviour unless
the caller asks for it.

**Non-goals** (out of scope; do not add them):

- No notifications for downloads created from Telegram or the web UI - only `POST /api/downloads`
  and `POST /api/files` with the flag set.
- No mirror of completion events to Telegram. Watch hits are mirrored; downloads are not.
- No progress/percentage events. One terminal event per download, nothing in between.
- No retry/backoff configuration, no per-watch notification settings, no new UI.
- No timeout that declares a slow download failed. See "Terminal failures" in Technical Details.
- No `.torrent` metadata parsing (`/api/v2/torrents/parseMetadata`, bencode libraries). The one
  corner it would close is documented as a known limitation instead.

**Rejected alternatives** (already considered - do not re-introduce):

- **qBittorrent's "Run external program on torrent finished"** hook calling back into this service.
  Rejected: the configuration lives outside this repo, is invisible to review, and is silently lost
  when the container is recreated; it also requires an HTTP client inside that image.
- **A long-lived goroutine polling `/api/v2/sync/maindata`** for deltas. Rejected: near-instant
  detection is worth nothing on a task that runs for tens of minutes and is consumed by an agent
  that then works for minutes more, and it costs a new long-running component with its own `rid`
  state and lifecycle.
- **Tagging torrents in qBittorrent (`tags=mfs:<id>`) to identify them later.** Rejected: the add
  response already carries the hash on the deployed version (see Verified Facts), and tags are *not*
  applied when the add returns 409, which is exactly the case tags were meant to cover.
- **Writing a row for every `POST /api/downloads` call** to get a history of one-shot downloads.
  Rejected as scope creep: rows exist to drive notifications, not to be a journal.

## Skills to invoke

Load each skill below with the Skill tool before implementing any task in this plan, if it is
available in this environment. It is background, not a dependency: the **Code-Quality Rules section
below is the complete and authoritative gate**, and where the skill disagrees with it - notably the
skill's godoc requirement, which this plan deliberately drops - this document wins. A reviewer must
not raise a skill-only rule as a finding.

- `go` - signature, visibility, methods-vs-helpers and comment conventions for every Go file touched
  here. Its Hard rules are materialised below as the per-task gate.

## Context (from discovery)

- Go 1.26 service, SQLite via `modernc.org/sqlite`, cron via gocron, NATS JetStream via
  `github.com/nats-io/nats.go`, qBittorrent via `github.com/autobrr/go-qbittorrent` v1.16.0.
- `app/watcher/publisher.go` already owns a JetStream connection and publishes
  `tuclaw.releases.found.<watch_id>` with a message id for dedup. Its internal `jetStream`
  interface (`publish(ctx, natsMessage) error`) is the seam this plan extracts.
- `app/schedular/schedular.go` exposes `AddJob(name, cronExpr string, cb func()) error` and `Start()`;
  every job runs in singleton mode. `app/main.go:153-159` registers `files` on `CRON` and `watcher`
  on `WATCH_CRON`.
- `app/task-store` and `app/watch-store` both *verify* schema by column and return
  `ErrSchemaNotInitialised`; neither creates tables. Schema is declared only in `app/migrations/*.sql`.
- `app/download-client/qbittorrent/client.go` is a thin wrapper; `CreateDownloadTask` currently
  discards the add response, and `GetHashByMagnet` already extracts a btih via
  `utils.ExtractBtihHash`.
- `app/bot/download-tasks/client.go:264` compares magnets on every sweep and re-downloads on change;
  line 259 carries `Location` from the stored row into the freshly parsed metadata.
- CI runs `go build ./...` and `go test ./...` (`.github/workflows/ci.yml`).

Line numbers in this document are a snapshot from the moment it was written and several tasks edit
the same files before later tasks read them. Always locate code by the quoted symbol or statement;
treat the number as a hint, never as the anchor.

## Development Approach

- **testing approach**: Regular (implementation first, then tests inside the same task)
- complete each task fully before moving to the next
- make small, focused changes; every changed line must trace to this plan
- **CRITICAL: every task MUST include new/updated tests** for the code it changes
  - both success and failure paths
  - table-driven tests where a rule has several cases (the completion criterion especially)
- **CRITICAL: all tests must pass before starting the next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**

**Backward-compatibility bar (scoped - read this before flagging a regression).** For requests that
carry no new flag, exactly **three** changes are intended by this plan, and nothing else:

1. the duplicate-add path on `POST /api/downloads`, which answers 500 today and answers 200 (or 409
   when the hash cannot be resolved) after Task 6, as specified there - the only *status code*
   change in the plan;
2. the `/api/files` response object gains exactly one key, `"notify": <bool>` (Task 8), on both
   `POST` and `GET`, since they share `toResponse`;
3. `GET /api/health` gains exactly one key, `"downloads": {"pending": <int>}` (Task 10).

No existing key anywhere changes its name, type or value, and no other status code moves. A reviewer
must not report these three as regressions, and must not accept a fourth.

## Code-Quality Rules (verify before marking each task complete)

Lifted from the `go` skill's Hard rules, with one deliberate change: its godoc requirement is
removed, because `CLAUDE.md` in this repository forbids docstrings and the project rule wins. The
rest is verbatim.

**Signatures:**
- No function or method has 4+ parameters; `ctx context.Context` does not count. Past the budget,
  use an options struct (`type fooOpts struct { ... }`).
- No function or method has 4+ return values; split into single-purpose functions or return a struct.
- Adjacent same-type parameters (`oldLine, newLine int`) are a swap hazard - put them on a struct.
  Exempt, because they pre-date this plan and their shapes are fixed by the tasks that quote them:
  `CreateDownloadTask(url, destination string)`, `DownloadNow(ctx, source, location string)` and
  `CreateFromURL(ctx, url, location string, notify bool)`. The rule applies to same-type pairs this
  plan newly introduces.

**Methods vs standalone helpers:**
- If a function is called only from methods of a single struct, it MUST be a method on that struct.
  Calling pattern decides, not field access.
- Standalone helpers are only for: constructors/entry points (`New...`, `Parse...`, `Decorate...`),
  utilities shared by multiple unrelated types, and tiny cross-cutting helpers - bounded here, so it
  is not an open-ended escape hatch, as under 10 lines and called from two or more distinct types or
  files.
- Before adding a standalone helper, walk its callers; if every caller is a method of one type, make
  it a method.

**Visibility (private by default):**
- Lowercase identifiers by default; export only when an out-of-package caller exists.
- Exception (per CLAUDE.md): a method called by other structs in the same package may be exported for
  inter-component API clarity - methods only, not types, functions, constants, or variables.
- Before exporting a new identifier, grep for cross-package callers; if none, lowercase it.

**Comments (default: none):**
- Default to no comments; add one only when the WHY is non-obvious (a hidden invariant, a
  workaround, surprising behavior).
- **No godoc requirement.** Do not add a doc comment to a type, function, method, constant or field
  just because it is exported. `CLAUDE.md` in this repository forbids docstrings outright, and that
  rule wins. An exported identifier gets a comment only under the same condition as any other line:
  a non-obvious WHY that carries risk.
- Never describe WHAT self-evident code does; no multi-paragraph comments on routine helpers.

**Comment density is a measured gate in this repo.** The reasoning in *this* document explains
decisions to the implementer; it must not be transcribed into the source. If a line explains WHAT
the code does, delete it and improve the name instead.

The threshold is mechanical, so it cannot be re-litigated. For every non-test `.go` file this plan
creates:

```
awk '/^[[:space:]]*\/\/go:/{next} /^[[:space:]]*\/\/ \+/{next} /^[[:space:]]*\/\//{c++} /\/\*/{c++} \
     END{printf "%s %.3f\n", FILENAME, (c+0)/NR}' <file>
```

The printed ratio must be **below 0.030**; build directives are skipped by the command itself, and
`*_test.go` and `*.sql` files are out of scope. A file at or above 0.030 fails the task. Files under
40 lines are allowed one comment line regardless of ratio, so a genuinely non-obvious WHY in a small
file is never forced out by arithmetic.

**Per-task gate (before marking a checkbox `[x]`):**
1. `gofmt -s -l app cmd` prints nothing and `go test ./... -race` passes (`golangci-lint run` if
   available - the repo has no config, CI runs build + test).
2. Run these two checks over the files the task created or changed, and treat their output as
   pass/fail rather than as advice:
   - **params**: `grep -nE '^func [^{]*\(' <files>` then count parameters per hit by hand, ignoring
     a leading `ctx context.Context`. Four or more non-`ctx` parameters is a fail.
   - **helpers**: for each new standalone helper, `grep -rn '<helperName>(' app --include='*.go' |
     grep -v '_test.go' | grep -v 'func <helperName>('` - the second filter drops the definition
     line, which would otherwise make the check pass unconditionally. If every remaining hit is in
     one file that defines exactly one struct type, and the helper is not a `New.../Parse...`-style
     constructor, it is a fail - make it a method. **Zero remaining hits is a pass, not a fail**: a
     helper whose callers arrive in a later task is judged at Task 11, not in the task that creates
     it. `downloads.Classify` is exempt outright - it is called from two packages by design.

3. Run the comment-density command above on each new non-test file and record the ratios.
4. Only after 1-3 pass: mark complete.

**Visibility is checked once, in Task 11, and only on functions, constants and variables.** Two
reasons, both mechanical rather than stylistic. First, nearly every identifier this plan exports
gets its first cross-package caller one to four tasks after it is created, so a per-task zero-hits
rule would force a fresh session to lowercase names that later tasks name literally. Second, an
exported *type* reached through a constructor is never written package-qualified at the call site -
`taskStore.NewRepository(db)` names the constructor, never `task_store.Repository` - so the grep
scores zero for the repo's own established convention.

At the end of Task 11, run only on the new exported **functions, constants and variables**
(`notify.NewClient`, `notify.ErrDisabled`, `downloads.Classify`, `types.ErrTorrentAlreadyExists`):
`grep -rn '<call-site token>\.<Ident>' app --include='*.go' | grep -v '^app/<dir>/'`. Note the two
spellings differ - the call-site token is the package clause or import alias (`downloadStore`,
`download_store`), the exclusion path is the directory (`app/download-store/`). Zero hits is a fail.

Exempt and pre-approved as exported: `notify.Client`, `notify.Options`, `notify.Message`,
`downloads.Download`, `downloads.Classification`, `downloads.Outcome`, `downloads.Sweeper`,
`download_store.Repository`, `types.TorrentState` - each is either a constructor's return type or a
parameter/field type in a cross-package signature. Methods are exempt too (per the CLAUDE.md
exception above); an exported method passes when it appears in an interface declared by another
package or is called from `app/main.go`.

## Testing Strategy

- **unit tests**: required in every task, in the same task as the code.
- **qBittorrent**: no live instance in tests. Stand up an `httptest.Server` that replies with the
  literal bodies quoted in Verified Facts - the JSON add response, the `409 Conflict` text, and the
  two `torrents/info` entries - and point the client at it. Every body a test needs is written in
  this document; do not compose new ones and do not go read the qBittorrent docs or the library
  source for them.
- **NATS**: no live server. Publishing is behind an interface; substitute a recorder that captures
  subject, message id and payload.
- **database**: through the existing `newTestRepo(t)` pattern - `t.Chdir(t.TempDir())` then
  `migrations.Apply` before constructing the repository, so tests and production reach their schema
  by the same path.
- **no e2e suite** exists for the backend; the frontend suite is untouched by this plan.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- keep this plan in sync with the work actually done

## Solution Overview

Three moving parts, all small:

1. **A shared NATS transport.** The JetStream connection and adapter move out of `app/watcher` into
   a new `app/notify` package that publishes an arbitrary subject/message-id/payload. The watcher
   keeps its own payload building and dedup key and delegates only the transport. One connection
   serves both publishers, and the download code never imports `app/watcher`.

2. **A registry plus a sweep for downloads.** `POST /api/downloads` with `"notify": true` records a
   row keyed by the torrent hash the add call returns, and answers with the `download_id` and the
   subject to arm. A new cron job, every ten minutes, reads unpublished rows, asks qBittorrent for
   the current state of exactly those torrents, and publishes a terminal event per row -
   `completed` with the on-disk path, or `failed` with a reason.

3. **A flag on tracked files.** `files.notify` makes the existing sweep publish
   `tuclaw.releases.updated.<file_id>` at the point where it already decides the magnet changed and
   re-queues the download.

Key decisions and why:

- **Cron, not a push hook or a delta poller.** Same shape as the two sweeps already running
  (singleton, app context, cancellable), no configuration outside the repo, and ten minutes of
  latency is noise against a download measured in tens of minutes. An idle tick costs one SQL query
  and no qBittorrent call at all, so the interval is a latency choice, not a load one.
- **Identity is the hash from the add response.** Verified on the deployed version: the response is
  JSON carrying `added_torrent_ids` for both magnets and `.torrent` URLs.
- **Publish before marking.** A crash between the two costs one duplicate wake; the reverse loses
  the event permanently and silently.
- **A promise nobody can keep is refused up front.** With `NATS_URL` empty the publisher is
  disabled, so `notify: true` answers 503 rather than accepting a request whose event will never
  arrive.
- **Failure is an event, not silence.** A torrent in `error`/`missingFiles`, or gone from
  qBittorrent entirely, publishes `status: "failed"`. The agent arms a one-shot event task and
  sleeps; an event that never comes is indistinguishable from "still downloading" forever.

## Technical Details

### Verified facts (measured against qBittorrent v5.2.3 / WebAPI 2.15.1 - the version running in production)

Do not re-derive these, and do not add fallbacks for older qBittorrent versions.

- `POST /api/v2/torrents/add` answers `200` with `Content-Type: application/json` and a body of the
  shape `{"added_torrent_ids":["<hash>"],"success_count":1,"failure_count":0,"pending_count":0}`,
  for a `.torrent` URL and for a magnet whose metadata has not been fetched yet.
  `go-qbittorrent` decodes this into `TorrentAddResponse.AddedTorrentIds`.
- Adding a torrent qBittorrent already holds answers `409` with `Content-Type: text/plain;
  charset=UTF-8` and the body `Conflict`, and any `tags` sent with the request are **not** applied.
  The library maps 409 to `ErrTorrentAddFailed`, which is why `POST /api/downloads` answers 500 for
  an already-present torrent today.
- **`ErrTorrentAddFailed` is not 409-specific, and `errors.Is` alone cannot tell the cases apart.**
  In `go-qbittorrent@v1.16.0`, `AddTorrentFromUrlCtx` wraps that same sentinel for two statuses
  (`methods.go:799` and `:801`): 409 with the message tail `| conflicts detected`, and 415 with
  `| torrent file not valid`. Treating every `ErrTorrentAddFailed` as "already present" would report
  a broken `.torrent` URL as a finished download and, with `notify: true`, register a row for a
  torrent that was never added. Discriminate on the message: it is a duplicate only when
  `errors.Is(err, qbt.ErrTorrentAddFailed)` **and** the error text contains `conflicts detected`;
  any other `ErrTorrentAddFailed` stays an ordinary add failure and keeps today's 500. For the 415
  case the library branches on the status code alone, so a bare `415` with no body is a sufficient
  test fixture - no recorded body exists or is needed.
- A **completed** torrent reports `state: "stalledUP"`, `progress: 1`, `completion_on: <unix ts>`,
  `amount_left: 0`, `content_path` (full file path for a single-file torrent, root directory for a
  multi-file one) and `size`. `completion_on` is set even when the data was already on disk when the
  torrent was added.
- An **incomplete** torrent reports `completion_on: -1`. The criterion must be `completion_on > 0`,
  never `!= 0`.
- The full `state` vocabulary: `error`, `missingFiles`, `uploading`, `pausedUP`, `stoppedUP`,
  `queuedUP`, `stalledUP`, `checkingUP`, `forcedUP`, `allocating`, `downloading`, `metaDL`,
  `pausedDL`, `stoppedDL`, `queuedDL`, `stalledDL`, `checkingDL`, `forcedDL`, `checkingResumeData`,
  `moving`, `unknown`.
- `GET /api/v2/torrents/info` accepts `filter`, `category`, `tag`, `sort`, `reverse`, `limit`,
  `offset` and `hashes` (multiple hashes joined by `|`).
- The consuming daemon matches an event task's subject filter with `*` and `>` wildcards, and an
  event task fires **once** and then completes. The `TUCLAW` stream is created by that daemon
  without a `Duplicates` setting, so the JetStream dedup window is the default **2 minutes**: a
  message id collapses a crash-retry seconds later, not a repeat an hour later.

These are the literal bodies recorded from that instance. They are the fixtures the tests serve -
copy them verbatim rather than composing new ones, and treat this plan, not the qBittorrent docs or
the library source, as their source of truth. Fields the code does not read may be omitted.

`GET /api/v2/torrents/info`, completed entry:

```json
[{"hash":"474d1403945c0768506233481557516e7af8d136","name":"sample.bin","state":"stalledUP",
  "progress":1,"completion_on":1786626099,"amount_left":0,
  "content_path":"/downloads/probe/sample.bin","save_path":"/downloads/probe",
  "size":4194304,"total_size":4194304,"added_on":1786626098,"eta":8640000}]
```

`GET /api/v2/torrents/info`, unfinished entry (note `completion_on: -1`):

```json
[{"hash":"9ecd4676fd0f0474151a4b74a5958f42639cebdf","name":"ubuntu-24.04.1-desktop-amd64.iso",
  "state":"downloading","progress":0,"completion_on":-1,"amount_left":5173995520,
  "content_path":"/downloads/probe-magnet/ubuntu-24.04.1-desktop-amd64.iso",
  "save_path":"/downloads/probe-magnet","size":5173995520,"total_size":5173995520,
  "added_on":1786626090,"eta":8640000}]
```

A hash qBittorrent does not know is simply **absent** from that array; the endpoint does not return
a zero-valued entry for it. That absence is the "deleted by hand" signal the sweep turns into a
failure event, so neither the client nor its test double may fabricate an entry.

### Subjects and payloads

```
tuclaw.downloads.completed.<download_id>
tuclaw.releases.updated.<file_id>
```

There are exactly two subjects. **Both terminal download outcomes - `completed` and `failed` - are
published on `tuclaw.downloads.completed.<download_id>`**, the same subject the HTTP response hands
back, and they are told apart only by the `status` field. There is deliberately no
`tuclaw.downloads.failed.*`: the agent arms a one-shot event task on the single subject it was
given, so a failure published anywhere else would never fire it - which is precisely the
"event that never arrives" this feature exists to prevent.

`download_id` is 8 random bytes hex-encoded (16 chars), generated by this service.

Completion payload fields: `download_id`, `status` (`completed`), `hash`, `name`, `content_path`,
`size`, `location`, `completed_at` (RFC3339 UTC).

Failure payload fields: `download_id`, `status` (`failed`), `reason`, `hash`, `name`,
`completed_at`.

Release-update payload fields: `file_id`, `name`, `page_url`, `last_comment`, `location`,
`torrent_updated_at`, `updated_at`.

Where each value comes from, so the event and the stored row can never disagree:

- `completed_at` is qBittorrent's `completion_on` converted from unix seconds to RFC3339 UTC for a
  `completed` event, and the sweep's own clock for a `failed` one (a failure has no completion time).
- `location` is the row's stored location - what the caller asked for - not qBittorrent's
  `save_path`.
- `name`, `content_path` and `size` are written onto the row in the same `UPDATE` that sets
  `status`, `reason`, `completed_at` and `published_at`; the payload is then built from the row.
- `reason` is one of exactly three strings: `qbittorrent state: error`,
  `qbittorrent state: missingFiles`, `torrent no longer present in qbittorrent`.
- `name` on a failure is whatever the row holds, which is the empty string when the torrent vanished
  before it was ever seen.
- `page_url` on a release update is the file's `OriginalUrl`; `updated_at` is the sweep clock.

The HTTP responses are a cross-system contract - the agent arms an event task from them and Task 12
documents them - so they are fixed here rather than described:

```
POST /api/downloads, no notify (unchanged)   201 {"status":"ok"}
POST /api/downloads, notify: true            201 {"status":"ok","download_id":"<16 hex>",
                                                  "subject":"tuclaw.downloads.completed.<16 hex>"}
POST /api/downloads, duplicate + hash known  200 {"status":"ok","duplicate":true,"hash":"<hash>",
                                                  "state":"<qbittorrent state>","completed":<bool>}
POST /api/downloads, duplicate, hash known
  but absent from the lookup or lookup failed 200 {"status":"ok","duplicate":true,"hash":"<hash>",
                                                  "state":"unknown","completed":false}
POST /api/downloads, duplicate + no hash     409 {"error":"torrent already present and its hash could not be resolved from the source"}
POST /api/downloads, notify + NATS disabled  503 {"error":"notifications are not configured"}
POST /api/downloads, notify + dry mode       503 {"error":"dry mode: no download is created, so no event can be published"}
POST /api/files, notify + NATS disabled      503 {"error":"notifications are not configured"}
```

Line wrapping in that block is layout only: each `error` string is a single line with single spaces
and no newline.

A duplicate reported with `notify: true` is resolved by all three `Classify` outcomes, not two:

| `Classify` on the duplicate | row written | body carries `download_id` + `subject` |
|---|---|---|
| empty status - still downloading | yes, unpublished | **yes** - an event is genuinely coming |
| `completed` | yes, with terminal outcome and `published_at` set to now | **no** |
| `failed`, or hash absent from the lookup, or the lookup errored | **no row** | **no** |

The rule behind all three: a `download_id` and a subject are handed back only when something will
actually be published there, and a row is written only when the sweep still has work to do. Writing
a row for a `failed` duplicate would make the sweep publish a failure on a subject the caller was
never given - the same broken promise the 503 rules exist to prevent - while the 200 body already
tells the caller the state inline.

Every timestamp in every payload and response this plan introduces is RFC3339 UTC, produced by
explicit formatting rather than by marshalling a `time.Time` as-is. `torrent_updated_at` is the
stored `FileMetadata.TorrentUpdatedAt`.

Message ids for JetStream dedup: `<download_id>:<status>` for downloads, and
`<file_id>:<sha256 of the new magnet, hex>` for release updates - a new version of a topic means a
new magnet, so a repeat of the same publication collapses while a genuine update does not.

**Subject-token safety.** A subject token must not contain a dot or a space, or the subject silently
grows an extra token and stops matching the agent's filter. File ids are the tracker `t=` value for
RuTracker and NNM, but for Jackett they come from `app/tracker/providers/jackett.go:108`, whose
fallback is a btih hash. Publish only when the id matches `^[A-Za-z0-9_-]+$`; otherwise log an error
and skip the publish - never emit a malformed subject.

### Completion criterion

Classification lives in **one** function, `downloads.Classify`, defined in Task 3 and called by both
the sweep (Task 7) and the HTTP duplicate path (Task 6). Neither re-derives it: a second copy of
this rule is how one call site quietly ends up using `!= 0` or an allow list.

The failure rules below are evaluated **first**. A row that classifies as `failed` is never also
`completed`, whatever its progress or completion time - a torrent in `error` can perfectly well
carry `progress 1` and a real `completion_on`, and the file on disk is still unusable.

Otherwise a row is `completed` when **all** hold:

- `progress >= 1`
- `completion_on > 0`
- `state` is **not** one of `error`, `missingFiles`, `checkingUP`, `checkingResumeData`, `moving`,
  `allocating`

This is a deny list on purpose. An allow list of "finished" states would silently stop firing if
qBittorrent introduced a new one, and "the event never arrived" is the worst failure mode this
feature has. `moving` is excluded because `content_path` during a move points at the directory the
files are leaving.

Applied to the whole 21-state vocabulary from Verified Facts, that gives a closed table with no
undefined cell - this is the finite done-condition for the criterion, and the sweep's test enumerates
exactly these states:

| states | expected outcome |
|---|---|
| hash absent from the lookup map (any state) | `failed`, reason `torrent no longer present in qbittorrent` |
| *(`found == false` is evaluated before the state rules and always yields that reason; the caller passes a zero-valued `types.TorrentState` in that case, so no state rule can compete for it)* | |
| `error`, `missingFiles` | `failed`, regardless of progress or completion time |
| `checkingUP`, `checkingResumeData`, `moving`, `allocating` | no event, regardless of progress |
| `uploading`, `pausedUP`, `stoppedUP`, `queuedUP`, `stalledUP`, `forcedUP`, `downloading`, `metaDL`, `pausedDL`, `stoppedDL`, `queuedDL`, `stalledDL`, `checkingDL`, `forcedDL`, `unknown` | `completed` **iff** `progress >= 1` **and** `completion_on > 0`; otherwise no event |

The last row is why the criterion is a deny list rather than a list of "seeding" states: a torrent
sitting in `stalledDL` or `unknown` with `progress 1` and a real completion timestamp has its files
on disk, and the agent must be woken for it.

### Terminal failures

A row is `failed`, with `reason`, when:

- `state` is `error` or `missingFiles` - reason names the state
- the hash is absent from the `torrents/info` response entirely (deleted by hand) - reason says so

There is deliberately **no** timeout. A genuinely slow torrent stays visible in the list for as long
as it needs, and a vanished one is closed on the next tick, so no row can hang forever.

Both branches are the same function:

```
Classify(s types.TorrentState, found bool) Classification   // Classification{Status, Reason string}
```

An empty `Status` means "nothing terminal yet, publish nothing". `found` is whether the hash was
present in a lookup that **succeeded**; a lookup that errored is never expressed as `found == false`,
because that would publish a false `failed` for every pending row.

`Status` has exactly three values: `completed`, `failed`, and empty. There is no fourth.

### Duplicate adds (409)

A duplicate - `ErrTorrentAddFailed` whose message contains `conflicts detected`, never any other
`ErrTorrentAddFailed`, see Verified Facts - means qBittorrent already has the torrent. It is treated
as success, not failure:

- resolve the hash - for a `magnet:` source with `utils.ExtractBtihHash`; for an `http(s)` `.torrent`
  URL by looking up the newest `downloads` row with the same `source`, which covers a re-run of the
  same agent task
- answer `200` with the torrent's current state in the body fixed under "Subjects and payloads", so
  an already-finished torrent gets its answer immediately instead of waiting for an event that can
  never arrive
- only when the hash cannot be resolved at all (an unseen `.torrent` URL for a torrent already
  present) answer `409` with an explicit reason

This replaces today's blanket 500 for duplicates, including `notify: false`. It is the only *status
code* change for flagless requests; the backward-compatibility bar lists the two additive response
keys that accompany it.

**The already-finished duplicate must not also produce an event.** The caller has just been told
inline that the download is complete; publishing on top of that would wake the agent for something
it already knows, and the 2-minute dedup window cannot suppress it. So when the duplicate is
resolved *and* already satisfies the completion criterion and `notify: true` was requested, the row
is written with its terminal outcome **and `published_at` set to now**, which makes the sweep skip
it. A duplicate that is still downloading is written unpublished and handled by the sweep like any
other row.

### Schema

Declared only in the migration; no constructor creates tables. `NewRepository` verifies the required
**columns** (a table check passes a table whose columns a half-applied migration never added) and
returns `ErrSchemaNotInitialised`.

```sql
-- +migrate Up
CREATE TABLE downloads (
    id           TEXT PRIMARY KEY,
    source       TEXT NOT NULL,
    location     TEXT NOT NULL,
    hash         TEXT NOT NULL DEFAULT '',
    name         TEXT NOT NULL DEFAULT '',
    content_path TEXT NOT NULL DEFAULT '',
    size         INTEGER NOT NULL DEFAULT 0,
    status       TEXT NOT NULL DEFAULT '',
    reason       TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    completed_at TIMESTAMP DEFAULT NULL,
    published_at TIMESTAMP DEFAULT NULL
);

ALTER TABLE files ADD COLUMN notify BOOLEAN NOT NULL DEFAULT 0;

-- +migrate Down
ALTER TABLE files DROP COLUMN notify;
DROP TABLE downloads;
```

### The `INSERT OR REPLACE` trap - the single most likely way this feature dies silently

`task-store`'s `CreateOrReplace` is `INSERT OR REPLACE`, which SQLite executes as DELETE + INSERT:
any column missing from its INSERT list resets to its DEFAULT on every save. The cron sweep calls
`CreateOrReplace` on every check of every tracked file. Therefore `notify` must be added in **two**
places or the flag is silently cleared on the first tick after it is set:

1. the column list and values of `CreateOrReplace`
2. the carry-over from the stored row into the freshly parsed metadata inside `processFileMetadata`
   - the same place `Location` is carried over today (`app/bot/download-tasks/client.go:259`)

`TestCreateOrReplacePreservesConsecutiveFailures` guards the identical trap for the failure counters;
this plan adds `TestCreateOrReplacePreservesNotify` beside it.

### Package layout

- `app/notify` - the shared JetStream transport. `Client` with `Publish(ctx, Message) error`,
  `Enabled() bool`, `Close()`; `Message{Subject, MsgID string; Payload []byte}`;
  `NewClient(Options{URL string}) *Client`; exported `ErrDisabled`, returned by `Publish` on a
  client with no connection. A failed connect logs and returns a disabled client - the service must
  still start when NATS is down.
- `app/downloads` - the domain type `Download` and the sweep. `Sweeper` with
  `RunCycle(ctx) error`, built from a deps struct holding the store, the torrent lookup and the
  notifier, each behind a consumer-side interface declared in this package. Mirrors how
  `app/watcher` defines `Watch` while `app/watch-store` persists it.
- `app/download-store` - `Repository` over the `downloads` table, importing `app/downloads` for the
  type.
- `app/types` gains `TorrentState{Hash, Name, State, ContentPath string; Progress float64;
  CompletionOn, Size int64}` in `app/types/torrent.go`, so `app/downloads` and `app/http` can name
  what the lookup returns without importing the download client.

### Ownership of the NATS connection

One `notify.Client` is built in `main.go` and shared by three consumers: the watcher publisher, the
download sweeper, and the `app/bot/download-tasks` client. `main.go` alone closes it. No consumer
dials NATS and no consumer closes what it did not open - `watcher.Publisher.Close()` therefore goes
away rather than being left as a second owner of a shared connection.

Each consumer declares its own narrow interface over it rather than taking `*notify.Client`:
`app/http` needs `Enabled() bool` (to refuse a promise it cannot keep), `app/downloads` and
`app/bot/download-tasks` need `Publish(ctx, notify.Message) error`. A nil notifier in
`download-tasks` is a no-op, never a panic.

## What Goes Where

- **Implementation Steps** (`[ ]`): everything inside this repository - migration, stores, sweep,
  handlers, wiring, tests, and this repo's own documentation.
- **Post-Completion** (no checkboxes): deployment, live verification against the real agent, and the
  agent-facing documentation that lives in the separate tuclaw-plugin repository.

## Implementation Steps

### Task 1: Extract the NATS transport into `app/notify`

**Files:**
- Create: `app/notify/client.go`
- Create: `app/notify/client_test.go`
- Modify: `app/watcher/publisher.go`
- Modify: `app/watcher/publisher_test.go`
- Modify: `app/main.go`
- Modify: `app/main_test.go`

- [x] create `app/notify` holding the connect logic, the JetStream adapter and the message type
      currently private to `app/watcher/publisher.go`: `NewClient(Options{URL string}) *Client`,
      `Publish(ctx context.Context, m Message) error`, `Enabled() bool`, `Close()`, and exported
      `ErrDisabled`
- [x] preserve today's semantics exactly: an empty URL warns once and disables publishing, a failed
      connect logs an error and returns a disabled client, and `Publish` on a disabled client
      returns `ErrDisabled` rather than a silent success
- [x] change `watcher.NewPublisher` to take the transport instead of a URL:
      `PublisherOptions{Transport publisherTransport}` where `publisherTransport` is
      `interface{ Publish(ctx context.Context, m notify.Message) error }`. Delete the dial code, the
      `URL` field and `Publisher.Close()` from `app/watcher` - `main.go` owns and closes the single
      connection. Keep `payload`, the trimming loop and `messageID` byte-identical
- [x] keep the existing unexported `jetStream` seam and its `natsMessage` type inside `app/watcher`,
      with a small adapter mapping `natsMessage` to `notify.Message`, so the fake substituted by
      `TestPublisherPayloadFields`, `TestPublisherSubjectAndMessageID` and
      `TestPublisherReturnsStreamError` keeps working untouched
- [x] move the three URL-driven tests - `TestPublisherDisabledWithoutURL`,
      `TestNewPublisherSurvivesUnreachableNats`, `TestNewPublisherSurvivesInvalidURL` - from
      `app/watcher/publisher_test.go` to `app/notify/client_test.go`, retargeted at `notify.Client`
      and `ErrDisabled`. Delete `errPublisherDisabled` from `app/watcher` and add one new test there,
      `TestPublisherForwardsDisabledTransport`: a transport stub returning `notify.ErrDisabled` makes
      `Publisher.Publish` return an error satisfying `errors.Is(err, notify.ErrDisabled)`
- [x] retarget `TestDegradedNoNATS` in `app/main_test.go`, which today calls
      `watcher.NewPublisher(watcher.PublisherOptions{URL: ""})` and `t.Cleanup(publisher.Close)` -
      both of which this task deletes. Point it at a disabled `notify.Client` instead; without this
      edit package `main` does not compile and this task's own gate cannot pass
- [x] update `app/main.go` to build the `notify.Client` once, hand it to the watcher publisher, and
      `defer` its `Close`
- [x] write tests for `notify.Client`: disabled when the URL is empty, `Publish` forwards subject,
      message id and payload to the JetStream layer, `Enabled` reflects both states
- [x] run `go test ./... -race` - must pass before task 2

### Task 2: Migration for `downloads` and `files.notify`

**Files:**
- Create: `app/migrations/20260813120000-add-downloads.sql`
- Modify: `app/migrations/embed_test.go`

- [x] add the migration exactly as written in Technical Details -> Schema, Up and Down. It is the
      eighth in `app/migrations/`, so `Apply` on an empty database now reports **8**
- [x] extend `app/migrations/embed_test.go`: after `Apply` on a fresh database, `PRAGMA
      table_info(downloads)` lists all twelve columns of the new table and `PRAGMA table_info(files)`
      contains `notify`
- [x] extend `TestApplyAdoptsDatabaseCreatedByTheOldServer` in `app/migrations/embed_test.go` - the
      case seeded by `seedLegacyServerSchema`, a `files` table with no `gorp_migrations`, which is
      what production looked like before the migration runner existed - with the same two
      assertions, so adoption plus the new migration is proven on the shape production actually has.
      Adjust whatever migration-count bookkeeping the adoption tests share. Do not touch
      `app/migrations/isolation_test.go`; it holds only the linker-isolation test
- [x] confirm the runner end to end: from an empty temp directory, `go run ./cmd/migrate` applies 8
      and an immediate second run applies 0
- [x] run `go test ./... -race` - must pass before task 3

### Task 3: `app/downloads` domain type and `app/download-store` repository

**Files:**
- Create: `app/types/torrent.go` - `TorrentState{Hash, Name, State, ContentPath string; Progress
  float64; CompletionOn, Size int64}`. It is created here, not in task 4, because `Classify` below
  takes it as a parameter and this task must compile on its own; task 4 only populates it
- Create: `app/downloads/download.go`
- Create: `app/downloads/download_test.go`
- Create: `app/download-store/repository.go`
- Create: `app/download-store/repository_test.go`

- [x] define `downloads.Download` with the fields of the table (id, source, location, hash, name,
      content path, size, status, reason, created/completed/published timestamps), plus
      `Classification` as defined in Technical Details and `Outcome` as spelled out in the query
      surface below
- [x] define the single classifier `Classify(s types.TorrentState, found bool) Classification` here,
      so the sweep and the HTTP duplicate path share one implementation of the rule; an empty
      `Status` means nothing terminal yet
- [x] `Classify` never matches a state against a list of known ones. It applies the deny list from
      the criterion, so a state qBittorrent introduces later, carrying `progress >= 1` and a real
      `completion_on`, still classifies as `completed` - that is the entire point of a deny list,
      and an unrecognised-state branch would reintroduce exactly the silent stall it avoids
- [x] test `Classify` with a table that enumerates **all 21 states** from the Verified Facts
      vocabulary, declared as a package-level `knownTorrentStates []string` in `app/downloads` so
      the list is code rather than prose. Assert `len(knownTorrentStates) == 21`, and cover each
      state twice - once with `progress 1 / completion_on > 0`, once with `progress 0 /
      completion_on -1` - against the outcome the criterion table prescribes, plus `found == false`
      for any state. Add one case for a state string absent from the vocabulary, asserting it
      behaves like the last table row rather than being swallowed
- [x] implement `Repository` following `app/watch-store/repository.go`: `NewRepository(db)` verifies
      the table exists **and then** each required column, returning `ErrSchemaNotInitialised`;
      `PRAGMA table_info` on a missing table returns no rows and no error, so the table check must
      come first
- [x] implement exactly this query surface, which is what tasks 5-10 call:
      `Create(d *downloads.Download) error`;
      `Pending() ([]*downloads.Download, error)` - `published_at IS NULL`, oldest first;
      `MarkPublished(id string, o downloads.Outcome) error` with
      `Outcome{Status, Reason, Name, ContentPath string; Size int64; CompletedAt time.Time}`;
      `Create` writes `created_at` explicitly from a Go `time.Time` rather than leaning on the
      column default: `CURRENT_TIMESTAMP` has one-second resolution, and two rows added in the same
      second would make the ordering below arbitrary. `Pending` orders `created_at ASC, rowid ASC`
      and `NewestBySource` `created_at DESC, rowid DESC`, so the order is total either way;
      `GetByID(id string) (*downloads.Download, error)`;
      `NewestBySource(source string) (*downloads.Download, error)` - newest `created_at`, any
      status, used only to resolve a duplicate add;
      `CountPending() (int, error)`
- [x] make `MarkPublished` carry `WHERE id = ? AND published_at IS NULL`, so a repeat after a
      crash-retry is a no-op that reports no error - this is what makes publish-then-mark safe to
      run twice. It writes `status`, `reason`, `name`, `content_path`, `size`, `completed_at` and
      `published_at` in that single statement
- [x] use explicit `UPDATE`s for outcome writes - never `INSERT OR REPLACE`
- [x] write tests through the `newTestRepo(t)` pattern (`t.Chdir(t.TempDir())` + `migrations.Apply`):
      round-trip a row; `Pending` excludes published rows and orders oldest first; a second
      `MarkPublished` on the same id changes nothing and returns nil; `NewestBySource` picks the
      newest of three rows sharing a source **created within the same second**, which is what the
      explicit timestamp and the `rowid` tiebreak exist for; `CountPending` counts only unpublished;
      a database
      missing the `downloads` table and one missing a single column both return
      `ErrSchemaNotInitialised`
- [x] run `go test ./... -race` - must pass before task 4

### Task 4: qBittorrent client returns the hash and reports duplicates

**Files:**
- Modify: `app/download-client/qbittorrent/client.go`
- Modify: `app/download-client/qbittorrent/client_test.go`
- Modify: `app/types/errors.go` (only the new sentinel; `TorrentState` already exists from task 3)
- Modify: `app/bot/download-tasks/client.go` (interface + call sites)
- Modify: `app/bot/download-tasks/client_test.go` (its `mockDownloadClient` must match the new
  signature or the package stops compiling)

- [x] change `CreateDownloadTask(url, destination string) error` to
      `CreateDownloadTask(url, destination string) (string, error)`, returning
      `TorrentAddResponse.AddedTorrentIds[0]`; when that slice comes back empty, fall back to
      `utils.ExtractBtihHash` for a `magnet:` source and otherwise return an explicit error - a row
      whose torrent we cannot identify later must never be created
- [x] add `types.ErrTorrentAlreadyExists` beside `ErrTorrentNotFound`, and return it **only** when
      `errors.Is(err, qbt.ErrTorrentAddFailed)` and the message contains `conflicts detected`. Every
      other `ErrTorrentAddFailed` - notably the 415 "torrent file not valid" case that shares the
      sentinel, see Verified Facts - is returned as an ordinary failure
- [x] add `TorrentStates(ctx context.Context, hashes []string) (map[string]types.TorrentState, error)`
      using `qbt.GetTorrentsCtx` with `TorrentFilterOptions{Hashes: hashes}`, keyed by hash. A hash
      qBittorrent does not know must be **absent from the map** - never a zero-valued entry, because
      absence is what task 7 reads as "deleted by hand"
- [x] update the `DownloadClient` interface in `app/bot/download-tasks/client.go` and its call sites
      for the new signature. In this task the callers assign the hash to `_` and behaviour is
      unchanged. Do not pre-implement task 6: after this task a duplicate add must **still** answer
      500, which is asserted by a test here that task 6 then flips - that assertion, not a diff
      against an unnamed baseline, is what pins the intermediate state, and a reviewer must not flag
      the still-500 duplicate as a defect
      (`TestDownloadNow_PropagatesAlreadyExists` in `app/bot/download-tasks/client_test.go` pins it:
      a duplicate stays an error out of `DownloadNow`, and the handler turns every such error into
      500)
- [x] write tests against an `httptest.Server` serving the bodies recorded in Verified Facts:
      JSON add response -> hash returned; JSON add response with an empty `added_torrent_ids` +
      magnet -> btih fallback; same + `.torrent` URL -> explicit error; 409 `Conflict` ->
      `ErrTorrentAlreadyExists`; **415 -> not `ErrTorrentAlreadyExists`**; `torrents/info` completed
      and unfinished entries -> all seven `TorrentState` fields populated; a requested hash missing
      from the response -> missing from the map
- [x] run `go test ./... -race` - must pass before task 5

### Task 5: `notify` flag on `POST /api/downloads`

**Files:**
- Modify: `app/http/client.go`
- Modify: `app/http/client_test.go`
- Modify: `app/bot/download-tasks/client.go`
- Modify: `app/bot/download-tasks/client_test.go` (`TestDownloadNow_DryMode_SkipsDownloadClient`,
  `TestDownloadNow_ForwardsSourceAndLocation` and `TestDownloadNow_PropagatesError` call
  `DownloadNow` directly and will not compile against the new signature; the dry-mode one also
  asserts the returned hash is empty)
- Modify: `app/main.go`

The seam is fixed here rather than left to judgement, because tasks 6, 7, 9 and 10 all build on it:

- `DownloadNow(ctx, source, location string) (string, error)` returns the hash from task 4;
  `TaskCreator` in `app/http/client.go` is updated to match.
- **The HTTP handler writes the `downloads` row**, not `download-tasks`. That keeps the one-shot
  path in `download-tasks` free of the registry and keeps `notify: false` exactly as it is today.
- `app/http` declares two new consumer-side interfaces - `downloadStore` (the subset of task 3's
  methods it calls) and `notifier` (`Enabled() bool`) - and `ClientCtx` gains `DownloadStore`,
  `Notifier` and `DryMode bool` **in this task**, wired in `main.go` in this task too (`DryMode`
  from `cfg.DryMode`), so the tree is complete and testable at the end of it. Task 9 only adds the
  cron job.
- Dry mode is read from that field, never inferred from `DownloadNow` returning an empty hash -
  an empty hash also arises from a qBittorrent response without ids, and conflating the two would
  make a real failure look like dry mode.

- [x] add optional `notify` to the request body; when it is absent or false the handler responds
      `201` with body exactly `{"status":"ok"}`, inserts no `downloads` row, and makes no notifier
      call - asserted, not asserted-about
- [x] when true and `Notifier.Enabled()` is false, or `DryMode` is set, answer `503` with the body
      fixed under "Subjects and payloads" **before** touching qBittorrent: a promised event nobody
      can deliver must be refused, not accepted
- [x] when true, generate the 16-char hex `download_id` (8 random bytes), add the torrent, record
      the row with the returned hash, and answer the `notify` body fixed under "Subjects and
      payloads"
- [x] dry mode: `DownloadNow` keeps its short-circuit and returns an empty hash with a nil error;
      the handler refuses `notify: true` on the `DryMode` field above and records no row. There is
      no torrent to ever complete, so an id would be a promise that cannot be kept
- [x] write handler tests asserting the **full decoded body**, not just the status: flag absent ->
      `201 {"status":"ok"}`, `SELECT COUNT(*) FROM downloads` is 0, recorder saw no publish; flag
      true + notifier disabled -> 503 with the fixed error body, no row; flag true + dry mode -> 503
      with its fixed error body, no row; flag true -> row created carrying the hash and the response
      body matches the fixed shape; add failure -> 500, no row
- [x] run `go test ./... -race` - must pass before task 6

### Task 6: Treat an already-present torrent as success

**Files:**
- Modify: `app/http/client.go`
- Modify: `app/http/client_test.go`
- Modify: `app/main.go`

- [x] on `types.ErrTorrentAlreadyExists` from task 4, resolve the hash: `ExtractBtihHash` for a
      magnet source, otherwise `NewestBySource` from the download store
- [x] declare a third consumer-side interface in `app/http` -
      `torrentLookup{ TorrentStates(ctx context.Context, hashes []string) (map[string]types.TorrentState, error) }` -
      give `ClientCtx` a `TorrentLookup` field, and pass the existing qBittorrent client into it
      from `main.go`. Without this the handler tests pass with a fake while production nil-panics on
      the first 409
- [x] answer `200` with the duplicate body fixed under "Subjects and payloads", deciding `completed`
      with `downloads.Classify` - never a second copy of the rule. When the hash is not in the
      returned map, or the lookup errors, use the `state: "unknown"` body from that same block
- [x] with `notify: true`, follow the three-outcome table under "Subjects and payloads" exactly:
      empty status -> row unpublished, body carries `download_id` and `subject`; `completed` -> row
      with its terminal outcome and `published_at` set to now, no id and no subject, so the sweep
      publishes nothing and the caller is not woken twice for what it was just told; `failed` (or
      hash absent, or lookup error) -> **no row at all**, no id and no subject
- [x] when the hash cannot be resolved at all, answer `409` with the reason body - never the old
      blanket 500
- [x] write tests for every branch: `notify: false` duplicate (answered 500 both before this plan
      and after task 4) -> 200 with the duplicate body and no row; `notify: true` duplicate still
      downloading -> row written with `published_at` NULL and returned by `Pending()`; `notify: true`
      duplicate already complete -> row with `published_at` non-NULL, `status` and `completed_at`
      set, **not** returned by `Pending()`, and a body carrying `"completed": true` with **no**
      `download_id` and no `subject`; `notify: true` duplicate in state `error` -> **no row** and no
      subject; `notify: true` duplicate whose hash is absent from the lookup -> the
      `state: "unknown"` body, no row, no subject; unresolvable duplicate -> 409; a 415 add failure
      -> still 500, proving the sentinel is not over-matched
- [x] run `go test ./... -race` - must pass before task 7

### Task 7: The download sweep

**Files:**
- Create: `app/downloads/sweeper.go`
- Create: `app/downloads/sweeper_test.go`

- [x] implement `Sweeper` with `RunCycle(ctx) error`, built from a deps struct carrying the store,
      the torrent lookup (`TorrentStates`) and the notifier (`Publish`), each behind a
      consumer-side interface declared in this package
- [x] read `Pending()` first and **return without calling qBittorrent at all** when it is empty;
      otherwise make exactly one `TorrentStates(ctx, hashes)` call for the whole set and match in
      memory. Propagate `ctx` into that call - the lookup is context-aware for this reason
- [x] a failed `TorrentStates` call aborts the cycle: log, return the error, publish nothing, mark
      nothing, retry on the next tick. Proceeding with an empty map would read as "every torrent was
      deleted by hand" and publish a false `failed` for every pending row, then mark them published
      - losing every real completion permanently
- [x] classify each row with `downloads.Classify` from task 3 - the sweep does not re-derive the
      rule; build the payload from the fields listed under "Subjects and payloads"; publish both
      outcomes on `tuclaw.downloads.completed.<download_id>`, and only afterwards call
      `MarkPublished` - never the reverse
- [x] honour context cancellation between rows so shutdown mid-sweep publishes nothing by halves,
      and let a single row's publish failure leave that row unmarked for the next tick instead of
      aborting the cycle
- [x] write sweep-level tests over `knownTorrentStates` (the classifier's own 21-state table is
      tested in task 3): a state that classifies terminal publishes exactly one message and marks
      the row; one that does not publishes nothing and leaves the row pending
- [x] write tests for the ordering guarantee: a publisher error leaves `published_at` empty and the
      next cycle retries; a successful publish marks the row and the next cycle publishes nothing;
      a row seeded already-published (what task 6 writes for an already-complete duplicate) is never
      picked up; a lookup error publishes nothing, marks nothing, and leaves every row pending
- [x] assert the exact subject, message id and payload of both event kinds - including that the
      **failed** event goes to `tuclaw.downloads.completed.<download_id>` and not to any
      failure-specific subject, the three literal `reason` strings, and that `completed_at` equals
      `completion_on` converted to RFC3339 UTC on success while a failure carries the sweep clock
- [x] run `go test ./... -race` - must pass before task 8

### Task 8: `notify` flag on tracked files and the release-update event

**Files:**
- Modify: `app/tracker/parser.go` (this is where `FileMetadata` lives)
- Modify: `app/task-store/repository.go`
- Modify: `app/task-store/repository_test.go`
- Modify: `app/http/client.go`
- Modify: `app/http/client_test.go` (its `mockTaskCreator.CreateFromURL` has the old signature and
  will not compile; it is also where the `/api/files` handler behaviour is tested)
- Modify: `app/bot/download-tasks/client.go`
- Modify: `app/bot/download-tasks/client_test.go`
- Modify: `app/main.go`

- [x] add `Notify` to `tracker.FileMetadata`, to `CreateOrReplace`'s column list and values, and to
      the row scan; add `notify` to the store's required columns
- [x] carry `Notify` from the stored row into the freshly parsed metadata in `processFileMetadata`,
      immediately after the `if current.Location != ""` block and **outside** it (locate it by that
      code, not by a line number - earlier tasks have already edited this file). The assignment is
      unconditional: `notify` has no sentinel value, so guarding it the way `Location` is guarded
      would silently clear the flag for every tracked file with an empty location. Without the
      carry-over at all, the flag is wiped on the first sweep
- [x] widen `CreateFromURL(ctx, url, location string, notify bool)`; the Telegram path (`OnMessage`)
      passes `false` explicitly, since a message from a human must never arm the agent. A re-post of
      an already-tracked URL takes the request's flag verbatim - a create is a create - which is
      worth a test of its own so the behaviour is pinned rather than accidental
- [x] accept optional `notify` on `POST /api/files`, defaulting to false, and add `"notify"` to
      `FileMetadataResponse` - which `GET /api/files` shares through `toResponse`, and which the
      backward-compatibility bar allows as an additive key
- [x] `POST /api/files` with `notify: true` and the notifier disabled answers the same `503` as
      `/api/downloads`. Dry mode is **not** a refusal here, unlike `/api/downloads`: the tracked row
      outlives the dry run and the flag becomes live at the next real sweep, so nothing is promised
      that cannot be delivered
- [x] give `downloadTasks.ClientCtx` a `Notifier` field over a consumer-side interface
      `interface{ Publish(ctx context.Context, m notify.Message) error }` declared in
      `app/bot/download-tasks`; `main.go` passes the same `notify.Client` the watcher publisher and
      the sweeper use. A nil notifier is a no-op, never a panic
- [x] publish `tuclaw.releases.updated.<file_id>` from `processFileMetadata` under **all** of these
      conditions, and no others: `fromCron` is true (a human pressing refresh must not wake the
      agent - the same rule the breaker and the run state already follow), dry mode is off, the
      magnet actually changed, `CreateDownloadTask` returned nil (so the revert path publishes
      nothing), and the file id matches `^[A-Za-z0-9_-]+$`. A publish failure is logged and never
      aborts the sweep or the re-download
- [x] add `TestCreateOrReplacePreservesNotify` beside the existing counter-preservation test
- [x] write tests: magnet unchanged -> no event; magnet changed with the flag off -> no event; flag
      on via cron -> exactly one event with the documented payload; **flag on via `RefreshAll` or
      `CheckFileForUpdates` -> no event**; dry mode -> no event; re-download failed and metadata
      reverted -> no event; a file id containing a dot -> no publish and an error logged; a tracked
      file with an **empty location** and `notify` set still has `notify` after a sweep
- [x] write handler tests in `app/http/client_test.go`: `POST /api/files` with `notify: true` and a
      disabled notifier -> 503 with the fixed body and no `CreateFromURL` call; with an enabled
      notifier -> the flag reaches `CreateFromURL` and the decoded body carries `"notify": true`;
      without the flag -> `"notify": false`; `GET /api/files` carries the key through `toResponse`
- [x] run `go test ./... -race` - must pass before task 9

### Task 9: Configuration and wiring

**Files:**
- Modify: `app/config/config.go`
- Modify: `app/config/config_test.go`
- Modify: `app/main.go`
- Modify: `compose.yaml`

- [x] add `DOWNLOAD_CRON` with default `*/10 * * * *`, following how `WATCH_CRON` is declared and
      defaulted (an empty value falls back to the constant, rather than an `env-default` tag)
- [x] construct the sweeper in `main.go` from the store, the qBittorrent client and the
      `notify.Client` already built in earlier tasks, and register a third job named `downloads`
      beside `files` and `watcher`; a failed registration must remain a boot failure
- [x] add `DOWNLOAD_CRON` to `compose.yaml` alongside the other cron variables
- [x] write a config test asserting the default and an explicit override
- [x] run `go test ./... -race` - must pass before task 10

### Task 10: Health reporting

**Files:**
- Modify: `app/http/client.go`
- Modify: `app/http/client_test.go`

- [x] add exactly `"downloads": {"pending": <int>}` to `GET /api/health`, where `pending` is
      `CountPending()`. No other keys
- [x] follow the shape `watches` already uses: a `*downloadsHealth` field with `json:"...,omitempty"`,
      so a nil store omits the key instead of panicking. This is not cosmetic - `app/http/client_test.go`
      builds 25 ad-hoc `ClientCtx` literals, most of which will not set the new field. A
      `CountPending` error logs and omits the key
- [x] leave the existing `status` derivation untouched: a pending download is normal operation, not
      degradation
- [x] write tests asserting the full decoded `downloads` object for 0 and 2 pending rows, that the
      key is absent when the store is nil, and that the top-level `status` string is unchanged in
      every case
- [x] run `go test ./... -race` - must pass before task 11

### Task 11: Verify acceptance criteria

**Files:**
- Create: `app/downloads/acceptance_test.go`

- [ ] add `TestAcceptanceMagnetNotifyToCompletedEvent` in `app/downloads/acceptance_test.go`,
      declared as **`package downloads_test`** - an in-package test cannot import `app/http`, which
      itself imports `app/downloads`, and the cycle would not build. Post a magnet with
      `notify: true` through the handler, assert one `downloads` row and a 16-hex `download_id` in
      the response; serve the completed `torrents/info` fixture from Verified Facts; run one cycle
      and assert the recorder captured exactly one message, on
      `tuclaw.downloads.completed.<download_id>`, whose `content_path` equals the fixture path
- [ ] establish the baseline as the first of these that resolves:
      `git log --format=%H --diff-filter=A -- docs/plans/20260813-download-notifications.md | tail -1`,
      then `git merge-base origin/master HEAD`, then the oldest commit on this branch absent from
      `master`
- [ ] stage this task's own new file first (`git add -A`), then
      `git diff --cached <baseline> --name-only --diff-filter=A -- '*_test.go'` must list exactly:
      `app/notify/client_test.go`, `app/downloads/download_test.go`, `app/downloads/sweeper_test.go`,
      `app/download-store/repository_test.go`, `app/downloads/acceptance_test.go`. A plain
      `<baseline>..HEAD` diff cannot see the acceptance test this task just wrote, and would fail
      with four entries
- [ ] `git diff --cached <baseline> --name-only --diff-filter=M -- '*_test.go'` must list only these,
      for these reasons: `app/watcher/publisher_test.go` (task 1 moves three tests out, adds
      `TestPublisherForwardsDisabledTransport`), `app/main_test.go` (task 1 retargets
      `TestDegradedNoNATS`), `app/migrations/embed_test.go` (task 2),
      `app/download-client/qbittorrent/client_test.go` (task 4),
      `app/bot/download-tasks/client_test.go` (task 4's and task 5's signatures, task 8's events),
      `app/task-store/repository_test.go` (task 8's `TestCreateOrReplacePreservesNotify`),
      `app/http/client_test.go` (tasks 5, 6, 8 and 10), `app/config/config_test.go` (task 9's
      `DOWNLOAD_CRON` test). Any other modified existing test file is a defect
- [ ] assert the flagless behaviours directly rather than from memory: `POST /api/downloads` without
      `notify` -> `201 {"status":"ok"}` and no `downloads` row; `POST /api/files` without `notify`
      -> 201 with every pre-existing key byte-identical plus exactly one new key, `"notify": false`
- [ ] run the classifier's own table test - `go test ./app/downloads -run TestClassify` - which is
      where `len(knownTorrentStates) == 21` is asserted; it is unexported, so the external
      acceptance test cannot re-assert it
- [ ] run the visibility check from Code-Quality Rules over every identifier this plan exported;
      this is the one place it runs, because most of them get their first cross-package caller
      several tasks after they are created
- [ ] run the full suite: `go test ./... -race`
- [ ] run `go build ./...` and `gofmt -s -l app cmd` (must print nothing)
- [ ] run the three gate checks and the comment-density command from Code-Quality Rules over exactly
      these files: `app/notify/client.go`, `app/downloads/download.go`, `app/downloads/sweeper.go`,
      `app/download-store/repository.go`, `app/types/torrent.go`; record the ratios in the progress
      log

### Task 12: [Final] Update documentation

**Files:**
- Modify: `CLAUDE.md`
- Modify: `README.md`

- [ ] document `DOWNLOAD_CRON` in the environment variable list
- [ ] update the `/api/downloads` description: it is still fire-and-forget by default, with
      `notify: true` as the explicit exception that persists a row and publishes one terminal event
- [ ] document the two new subjects, the fact that a failed download publishes on the *same* subject
      as a completed one and is told apart by `status`, the completion criterion and the
      publish-before-mark ordering, beside the existing watcher notes
- [ ] document `files.notify` and the `CreateOrReplace` column-reset trap it shares with the failure
      counters
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification:**

- Deploy, then exercise the real scenario: search a release, post its magnet to `/api/downloads`
  with `notify: true` and `location: /downloads/cinema-prep`, arm a `schedule_type: event` task on
  the returned subject, and confirm the agent wakes with the on-disk path once the download lands.
- Confirm `GET /api/health` shows the pending count dropping back to zero after the event.
- Confirm a deliberately broken download (a magnet with no seeds, then deleted from qBittorrent by
  hand) produces a `failed` event rather than silence.

**External system updates:**

- The agent-facing documentation lives in the separate tuclaw-plugin repository, in the
  `allspeak-sources` skill. It needs a section covering the `notify` flag on both endpoints, the two
  subjects, the payload shapes, and the rule that an event task fires once and must be re-armed.
  Without it the agent will not know the feature exists. **That repository is not checked out in
  this run and must not be searched for; no task in this plan depends on it, and its absence is not
  a blocker for any checkbox.**

**Known limitation to record when documenting:**

- A `.torrent` URL that this service has never seen before, for a torrent qBittorrent already holds,
  cannot be identified and answers 409. The known remedy is `POST /api/v2/torrents/parseMetadata`,
  deliberately not implemented here.
