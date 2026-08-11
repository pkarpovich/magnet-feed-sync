# Apply migrations automatically at deploy time

## Overview

The service shipped a schema change that never reached production. A recent change added three columns to
`files` via `migrations/20260810144014-add-failure-tracking.sql`, the deploy pulled the new image, and
every request then failed with:

```
SQL logic error: no such column: consecutive_failures (1)
```

`/api/health` returned HTTP 500 and the container had to be stopped by hand.

**Root cause — two independent gaps, both required:**

1. **Nothing applies migrations.** The deploy is `git pull && curl stash > .env && docker compose pull &&
   docker compose up -d`. There is no migration step anywhere, and the host has neither `sql-migrate` nor a
   Go toolchain. The three migrations recorded in production's `gorp_migrations` were applied by hand in
   2024.
2. **The schema is declared twice, so the gap was invisible.** `NewRepository` runs
   `CREATE TABLE IF NOT EXISTS files (...)` with the full current column list. On a fresh database that
   creates everything and every test passes; on an existing table the statement is a **no-op** and the new
   columns silently never appear. The code therefore looked self-sufficient while depending on a manual
   step nobody ran.

This plan makes migrations a deploy-time step owned by a **separate one-shot container**, and removes the
duplicated schema declaration so the two can never drift again.

### Non-goals

- **Do NOT run migrations from inside the server process.** Explicitly chosen: the server binary must not
  gain a migration dependency, and the app image must stay exactly what it is today.
- **Do NOT rename or edit any existing migration file.** Their names are the ids stored in
  `gorp_migrations`, and production already holds three of them. Adding the one new baseline migration
  specified below is the only permitted change to the migration set.
- **Do NOT fix the production database by hand.** Once this ships, the first deploy applies the pending
  migrations by itself. Manual `ALTER TABLE` is not part of this plan.
- **Do NOT touch the release-watcher plan or its tables.** This lands first and independently.
- **Do NOT add behaviour to the migrate binary beyond "apply pending migrations up".** No `down`, no
  `status`, no flags — the `sql-migrate` CLI stays available for those.
- **Do NOT reformat unrelated files.**

### Rejected alternatives

- **Second binary inside the existing app image (same image, different entrypoint).** Cheapest option and
  version skew is impossible, but it grows the app image from 41.9 MB to ~50 MB and puts the migration
  concern inside the artifact that serves traffic. Rejected by the user in favour of separation; the
  measured cost of separating turned out to be small.
- **Migrations at server startup.** Same objection, plus it makes a failed migration a crash-looping
  service rather than a deploy that stops before starting.
- **A shell script over the `sqlite3` CLI in a throwaway image.** No Go dependency at all, but it has to
  re-implement what `gorp_migrations` already does: read applied ids, split `-- +migrate Up` from `Down`,
  and honour `StatementBegin`/`StatementEnd` blocks. Twenty lines today, wrong the first time a migration
  contains a trigger with semicolons in it.
- **`golang-migrate` and its off-the-shelf image.** A different tool with a different file naming
  convention and its own history table, so the already-applied migrations would not be recognised.
- **Running migrations from the deploy command in `home-environment`.** Splits the schema contract across
  two repositories, gives local development and CI nothing, and needs tooling installed on the host.

## Skills to invoke

The `Code-Quality Rules` section below is the **complete and sole** acceptance bar for style. If the `go`
skill is available, loading it is optional context; if not, proceed — it is not a blocker, and a reviewer
MUST NOT fail a task on any rule not written in that section.

## Context (from discovery)

**Repo rows below are re-checkable and SHOULD be spot-checked against the checkout; if one disagrees with
the repo, trust the repo and mark the plan `⚠️`. Production and toolchain rows are NOT observable from a
ralphex session — treat them as given, and no task may depend on them.**

### Base precondition (check first, before Task 1)

This plan builds on the failure-tracking change. Run:

```bash
ls migrations/*.sql
```

It MUST list five files: the three 2024 ones and `20260810144014-add-failure-tracking.sql` +
`20260810145500-add-app-state.sql`. If the 2026 files are absent the base branch predates that change:
**stop and mark the plan `⚠️ blocked — base branch predates the failure-tracking change`** rather than
inventing the missing migrations.

### The migration set does not build a database from scratch

**This is the single most important fact in this plan.** No migration creates the `files` table:

| file | what it does |
|---|---|
| `20240511212753-remove-rss-field.sql` | `ALTER TABLE files DROP COLUMN rss_url` |
| `20240803112540-add-last-comment-column.sql` | `ALTER TABLE files ADD COLUMN last_comment TEXT NOT NULL DEFAULT ''` |
| `20240805004743-add-location-column.sql` | `ALTER TABLE files ADD COLUMN location TEXT NOT NULL DEFAULT '/downloads/tv shows'` |
| `20260810144014-add-failure-tracking.sql` | adds `consecutive_failures`, `last_error`, `last_error_at` |
| `20260810145500-add-app-state.sql` | `CREATE TABLE IF NOT EXISTS app_state (...)` |

`files` has only ever existed because `NewRepository` created it — the very statement Task 3 deletes. So
applying this set to an empty database fails immediately with `no such table: files`. **Task 1 therefore
adds a baseline migration**, specified verbatim in Technical Details. Without it nothing in this plan works.

### Production state (given; no task may depend on it)

| fact | value |
|---|---|
| `gorp_migrations` in production | three records, all 2024: `20240511212753-remove-rss-field.sql`, `20240803112540-add-last-comment-column.sql`, `20240805004743-add-location-column.sql` |
| pending | the two 2026 migrations, plus the new baseline (a no-op there, see below) |
| `files` columns in production | 10; `consecutive_failures`, `last_error`, `last_error_at` are **missing** |
| `app_state` | **exists** — new table, so `CREATE TABLE IF NOT EXISTS` in `NewRepository` did create it |
| rows in `files` | 160 — live data, must survive |
| container | stopped by hand; `restart: unless-stopped`, so it stays down until started |

### Toolchain and sizes (measured elsewhere; given)

| fact | value |
|---|---|
| host | no `sql-migrate`, no Go toolchain |
| app image | 41.9 MB, `/bin/server` 28.6 MB |
| migrate binary (`CGO_ENABLED=0`, `-ldflags="-s -w"`) | 7.7 MB |
| docker compose on the host | v5.3.1, so `depends_on: condition: service_completed_successfully` is supported |

### sql-migrate v1.8.1 (pin to this version; the facts below are stated for it)

| fact | value |
|---|---|
| dependency graph | exactly `sql-migrate`, `sql-migrate/sqlparse`, `go-gorp/gorp/v3` — **no `mattn/go-sqlite3`, no CGO**; builds with `CGO_ENABLED=0` |
| apply-all | `migrate.Exec(db *sql.DB, dialect string, m MigrationSource, dir MigrationDirection) (int, error)` |
| apply-N | `migrate.ExecMax(db, dialect, m, dir, max int) (int, error)` — needed by the regression test |
| embedded source | `migrate.EmbedFileSystemMigrationSource{FileSystem: <embed.FS>, Root: "."}` |
| direction | `migrate.Up` / `migrate.Down` (`MigrationDirection` constants) |
| dialect | `"sqlite3"` — selects SQL generation only; the driver comes from the `*sql.DB` passed in |
| history table | default is `gorp_migrations` — do **not** call `SetTable` |

### Repo facts (re-checkable)

| fact | value |
|---|---|
| Dockerfile stages | `build` (golang), `base` / `deps` / `frontend-build` (node), `final` (`alpine:latest`) — `final` is currently the **last** stage |
| `build` stage | `FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}`, declares `ARG TARGETARCH`, builds via `--mount=type=cache,target=/go/pkg/mod/ --mount=type=bind,target=.` with `CGO_ENABLED=0 GOARCH=$TARGETARCH` |
| `final` stage | copies `/bin/server` and the frontend `dist`; sets no `WORKDIR`, so CWD is `/` |
| `openDB` | resolves `.db/<filename>` relative to the process CWD, creating the folder; compose mounts `.db:/.db`, so the file is `/.db/tasks.db` |
| `database.Client` | wraps `*sql.DB` unexported with **no accessor** — sql-migrate needs the `*sql.DB`, so one must be added |
| `app/task-store/repository_test.go:15` | `newTestRepo(t)` does `t.Chdir(t.TempDir())`, `database.NewClient("test.db")`, then `NewRepository(db)` — it relies on the constructor creating the tables |
| import alias | `app/main.go` imports the package as `taskStore "magnet-feed-sync/app/task-store"`; the Go package name is `task_store` |
| release pipeline | `.gitea/workflows/release.yml`, push to `master`: **one** `docker/build-push-action` step **with no `target:`**, so it builds the Dockerfile's last stage; tags `:latest` and `:${{ steps.version.outputs.version }}` from `.semver.yaml` (`release: v1.1.0`); then the updater webhook, then Telegram |
| `compose.override.yml` | overrides both `build: {context: ., target: final}` **and** `image: magnet-feed-sync:local` |
| `Makefile` | drives `sql-migrate up`, reading `dbconfig.yml` — so updating `dir:` is enough |
| `gofmt -s -l .` | currently clean — keep it that way |

## Development Approach

- **Testing approach:** Regular (code first, tests in the same task), matching the repo's existing style.
- Complete each task fully before the next; keep the build green at every task boundary.
- **Every task that changes Go code under `app/` or `cmd/` MUST include new/updated tests** as separate
  checklist items. **Exempt:** Task 4 (Dockerfile), Task 5 (compose), Task 6 (pipeline), Task 7
  (verification) and Task 8 (documentation) change no Go code.
- **All tests must pass before starting the next task.**
- **No test may reach the network.** Everything is local SQLite under `t.TempDir()`.
- Update this plan when scope changes (`➕` new task, `⚠️` blocker).

## Code-Quality Rules (verify before marking each task complete)

This section is the complete style bar for this plan.

**Signatures:** no function or method has 4+ parameters (`ctx context.Context` does not count) or 4+ return
values; adjacent same-type parameters go on a struct. Signatures written verbatim in Technical Details are
pre-approved.

**Methods vs standalone helpers:** if a function is called only from methods of a single struct, it MUST be
a method on that struct.

**Visibility:** lowercase by default. **This plan adds exactly three exported identifiers, and they are all
pre-approved:** `migrations.Apply`, `database.Client.DB` (a method), and
`task_store.ErrSchemaNotInitialised` (deliberately part of the package's error API; no external caller is
required). Any *other* new exported top-level identifier must be justified or unexported.

**Errors:** wrap with context `fmt.Errorf("apply migrations: %w", err)` — lowercase, no trailing
punctuation, wrapped once at the boundary. Never panic for expected failures, never discard with `_`.

**Comments:** default none; add one only when the WHY is non-obvious.

**The numbered checks below are the only failable style criteria.** The prose above is authoring guidance;
a reviewer MUST NOT report a finding against it unless a numbered check fails.

1. Build the changed-file list covering uncommitted work, because this gate runs before the task's commit:
   `CHANGED=$(git status --porcelain -- '*.go' | awk '{print $NF}'; git diff --name-only $(git merge-base HEAD origin/HEAD) -- '*.go')`.
   If that fails, fall back to the union of the `Files:` lists of the tasks completed so far.
2. `gofmt -s -l .` prints nothing. **Always run this one**, it has no stdin hazard.
3. `gofmt -s -l $CHANGED` prints nothing — **skip only if `$CHANGED` is empty**, because `gofmt` with no
   file arguments reads stdin and reports a false pass.
4. `go vet ./...`, `go build ./...`, `go test ./... -race` all exit 0.
5. `grep -nE '^func.*\(.*,.*,.*,.*\)' $CHANGED` — skip if `$CHANGED` is empty. A hit is a violation **only
   if** all three hold: the parameter list excluding `ctx context.Context` has 4+ entries, the signature is
   not prescribed in Technical Details, **and the line was added or modified by this plan**. The regex also
   counts commas in return tuples, and pre-existing signatures in a touched file (for example
   `ExecWithRetry`/`QueryWithRetry` in `app/database/client.go`) are expected non-violations.

## Solution Overview

Migrations become an artifact of their own: a second, tiny image built from this repository, containing one
static binary that opens the same database file and applies whatever is pending. Compose runs it to
completion before the app starts, so the app can only ever see a migrated schema.

The app image, the server binary and its dependency graph are untouched — `sql-migrate` never enters them.

The second half of the fix is subtractive: `NewRepository` stops declaring the schema. Migrations become
the single source of truth — which is only possible once the baseline migration below exists.

## Technical Details

### Baseline migration (new file — the fix for the "no such table: files" hole)

Create `app/migrations/20240101000000-create-files.sql`. The id must sort **before** `20240511212753` so a
fresh database is built in the right order.

```sql
-- +migrate Up
CREATE TABLE IF NOT EXISTS files (
    id TEXT PRIMARY KEY,
    original_url TEXT,
    rss_url TEXT,
    magnet TEXT,
    name TEXT,
    last_sync_at TIMESTAMP,
    torrent_updated_at TIMESTAMP,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    delete_at TIMESTAMP DEFAULT NULL
);

-- +migrate Down
DROP TABLE files;
```

Three properties are load-bearing and must not be "tidied":

- **`rss_url` must be present**, because `20240511212753` drops it. This is the historical shape of the
  table, recovered from the commit that introduced that migration — not the current shape.
- **`last_comment` and `location` must be absent**, because `20240803112540` and `20240805004743` add them;
  including them here makes those migrations fail with `duplicate column name`.
- **`IF NOT EXISTS` is mandatory.** This migration is not recorded in production's `gorp_migrations`, so it
  *will* run there against the live 160-row table, where it must be a silent no-op.

Consequence to remember: production ends with **six** records in `gorp_migrations`, not five.

### Package layout

Migrations move from `migrations/` into a Go package, because `go:embed` cannot reach files above the
directory of the file that declares it:

```
app/migrations/
    embed.go
    20240101000000-create-files.sql          <- new baseline
    20240511212753-remove-rss-field.sql
    20240803112540-add-last-comment-column.sql
    20240805004743-add-location-column.sql
    20260810144014-add-failure-tracking.sql
    20260810145500-add-app-state.sql
cmd/migrate/
    main.go
```

`dbconfig.yml` gets `dir: app/migrations` so the `sql-migrate` CLI and the `Makefile` keep working.

### The migrations package

```go
//go:embed *.sql
var files embed.FS

func Apply(db *sql.DB) (int, error)
```

`Apply` calls `migrate.Exec` with dialect `"sqlite3"`, direction `migrate.Up` and
`migrate.EmbedFileSystemMigrationSource{FileSystem: files, Root: "."}`, returning the number applied. It
does not call `SetTable`: the default is already `gorp_migrations`.

The package is imported by `cmd/migrate` and by the `task-store` test helper, and **never by
`app/main.go`**, so the server binary keeps a `sql-migrate`-free dependency graph.

The subset regression test lives **inside** `package migrations` so it can reach the unexported `files`
variable and call `migrate.ExecMax(db, "sqlite3", migrate.EmbedFileSystemMigrationSource{FileSystem: files,
Root: "."}, migrate.Up, 4)` — 4 = baseline + the three 2024 migrations, i.e. everything before the failure
columns.

### Database accessor

```go
func (c *Client) DB() *sql.DB
```

### The migrate binary

`cmd/migrate` opens the database exactly the way the server does — `database.NewClient("tasks.db")`, so it
inherits the same `.db/<file>` path resolution and the same pragmas — then calls `migrations.Apply`.

Two things a fresh session would otherwise get wrong:

- **Close the database on every path.** `log.Fatal` and `os.Exit` skip `defer`, and on a WAL database a
  skipped `Close` leaves `-wal`/`-shm` unrolled for the app container that starts seconds later. Structure
  it as a `run() error` helper called from a two-line `main` that closes explicitly before exiting.
- **No observability wiring.** Plain `slog` to stderr, no Loki, no config loading, no tracing — the second
  image must not pull the observability stack.

Exit codes are the whole contract with compose: **0 on success including "nothing to apply", non-zero on
any failure.** No flags, no arguments.

### The migrate image

The new Dockerfile stage is named **`migrate-final`** and every other file refers to it by that name.

Build it in the existing `build` stage, mirroring the server's RUN so cross-compilation still works:

```dockerfile
RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=bind,target=. \
    CGO_ENABLED=0 GOARCH=$TARGETARCH go build -ldflags="-s -w" -o /bin/migrate ./cmd/migrate
```

Then:

```dockerfile
FROM alpine:latest AS migrate-final
COPY --from=build /bin/migrate /bin/
ENTRYPOINT [ "/bin/migrate" ]
```

Decisions behind this:

- **`GOARCH=$TARGETARCH` is not optional.** Without it the migrate binary is built for the builder's
  architecture while the app is cross-compiled correctly, and the failure appears only at deploy time as
  `exec format error`.
- Base is `alpine:latest`, the same base as `final`, so the host has already pulled that layer. `scratch`
  would save ~8 MB but leaves no writable `/tmp` for SQLite — not worth it for a deploy-time job.
- No `WORKDIR`, so CWD is `/` and `.db/tasks.db` resolves to `/.db/tasks.db`, matching the server.

**Stage order matters, and it is a trap.** The release workflow's build step has no `target:`, so it builds
the Dockerfile's **last** stage. Placing `migrate-final` after `final` would publish the migration binary
under the app's tags on the next release. Task 6 therefore adds `target: final` to the existing step, which
makes the app image independent of stage order permanently.

### Compose wiring

```yaml
  migrate:
    container_name: magnet-feed-sync-migrate
    image: git.pkarpovich.space/pkarpovich/magnet-feed-sync-migrate:latest
    restart: "no"
    volumes:
      - .db:/.db

  magnet-feed-sync:
    depends_on:
      migrate:
        condition: service_completed_successfully
```

- **`restart: "no"` is mandatory** — inherited or copied `unless-stopped` turns a one-shot into a loop.
- The migrate service takes **only** the `.db` volume: no environment, no `proxy` network, no traefik
  labels.
- Re-running is free: `gorp_migrations` makes a no-op run exit 0 immediately.

`compose.override.yml` mirrors what it already does for the app — **both** a build section and an image
override, so a local build never stamps itself onto the registry tag:

```yaml
  migrate:
    build:
      context: .
      target: migrate-final
    image: magnet-feed-sync-migrate:local
```

### Release pipeline

Two edits to `.gitea/workflows/release.yml`:

1. Add `target: final` to the **existing** build-push step (see the stage-order trap above).
2. Add a second build-push step after it with `target: migrate-final` and these values verbatim:
   - tags `${{ secrets.DOCKER_REGISTRY }}/pkarpovich/magnet-feed-sync-migrate:latest` and
     `${{ secrets.DOCKER_REGISTRY }}/pkarpovich/magnet-feed-sync-migrate:${{ steps.version.outputs.version }}`
   - `cache-from: type=registry,ref=${{ secrets.DOCKER_REGISTRY }}/pkarpovich/magnet-feed-sync-migrate:cache`
   - `cache-to: type=registry,ref=${{ secrets.DOCKER_REGISTRY }}/pkarpovich/magnet-feed-sync-migrate:cache,mode=max`

The updater webhook and the Telegram notification stay the last two steps, in that order.

### Removing the duplicated schema

`NewRepository` stops executing `CREATE TABLE IF NOT EXISTS` for `files` and for `app_state`, and instead
verifies the schema:

```go
var ErrSchemaNotInitialised = errors.New("database schema not initialised: run the migrate binary or `sql-migrate up`")
```

**The check is on columns, not on table existence.** Table-existence would not have caught the incident at
all: `files` existed, the columns did not. `NewRepository` runs `PRAGMA table_info(files)` and requires
`consecutive_failures`, `last_error` and `last_error_at` to be present, and checks `sqlite_master` for
`app_state`. Anything missing ⇒ return `ErrSchemaNotInitialised` (wrapped with which check failed).

`newTestRepo(t)` calls `migrations.Apply` on the fresh temp database before constructing the repository, so
tests and production reach their schema by the same path.

## Testing Strategy

- Unit tests are required in every Go task, as separate checklist items.
- The migrations package is tested against a real temporary SQLite database.
- The regression test for this incident applies only the pre-2026 subset via `ExecMax`, inserts a row, then
  applies everything and asserts the failure columns exist and the row survived — the case that
  `CREATE TABLE IF NOT EXISTS` silently skipped.
- `go test ./... -race`.

## Progress Tracking

- Mark completed items `[x]` immediately when done.
- New tasks get a `➕` prefix; blockers get `⚠️`.

## What Goes Where

- **Implementation Steps** (`[ ]`): everything inside this repository.
- **Post-Completion** (no checkboxes): deploying and confirming production recovers.

## Implementation Steps

### Task 1: Migrations package, baseline migration and runner

**Files:**
- Create: `app/migrations/embed.go`
- Create: `app/migrations/embed_test.go`
- Create: `app/migrations/20240101000000-create-files.sql`
- Move: `migrations/*.sql` → `app/migrations/` (names unchanged)
- Modify: `dbconfig.yml`
- Modify: `app/database/client.go`
- Modify: `go.mod`, `go.sum`

- [x] run the **Base precondition** check from Context; if the two 2026 migrations are absent, stop and
      mark the plan `⚠️ blocked`
- [x] `go get github.com/rubenv/sql-migrate@v1.8.1` — pinned, because the API, dependency-graph and size
      facts in Context are stated for that version
- [x] move every `*.sql` from `migrations/` into `app/migrations/` **keeping names byte-for-byte** — they
      are the ids already stored in `gorp_migrations`
- [x] add `app/migrations/20240101000000-create-files.sql` with the SQL given verbatim in Technical
      Details, including `rss_url`, excluding `last_comment`/`location`, and with `IF NOT EXISTS`
- [x] add `app/migrations/embed.go` with the `//go:embed *.sql` variable and
      `Apply(db *sql.DB) (int, error)` as prescribed
- [x] add the `DB() *sql.DB` accessor to `database.Client`
- [x] point `dbconfig.yml` at `dir: app/migrations`
- [x] write a test applying to an **empty** temporary database and asserting `files` ends up with
      `consecutive_failures`, `last_error`, `last_error_at`, `last_comment`, `location`, and **without**
      `rss_url`, and that `app_state` exists
- [x] write a test asserting a second `Apply` on the same database returns 0 applied
- [x] write the incident regression test using `migrate.ExecMax(..., 4)` as prescribed: apply the first
      four, insert a row into `files`, apply everything, assert the three failure columns exist and the row
      survived
- [x] run tests — must pass before Task 2

### Task 2: The migrate binary

**Files:**
- Create: `cmd/migrate/main.go`
- Create: `cmd/migrate/main_test.go`

- [x] implement `run() error` opening the database with `database.NewClient("tasks.db")`, calling
      `migrations.Apply`, logging the number applied via plain `slog` to stderr, and closing the database
- [x] make `main` a two-line wrapper that closes explicitly and exits non-zero on error — no `defer` on the
      exit path, because `os.Exit`/`log.Fatal` skip it and a WAL database would be left unrolled
- [x] wire no observability: no Loki, no tracing, no config loading
- [x] keep it flagless: no `down`, no `status`, no arguments
- [x] write a test for `run()` succeeding against a temp database (use `t.Chdir(t.TempDir())` as
      `newTestRepo` does) and asserting a second call applies zero
- [x] write a test for `run()` returning an error when the database path is unusable
- [x] run tests — must pass before Task 3

### Task 3: Make migrations the single source of schema

**Files:**
- Modify: `app/task-store/repository.go`
- Modify: `app/task-store/repository_test.go`

- [ ] delete both `CREATE TABLE IF NOT EXISTS` statements from `NewRepository` — this duplicated
      declaration is the reason the missing columns went unnoticed
- [ ] add `ErrSchemaNotInitialised` and make `NewRepository` verify **columns**, not just tables:
      `PRAGMA table_info(files)` must contain `consecutive_failures`, `last_error`, `last_error_at`, and
      `sqlite_master` must contain `app_state`; anything missing returns the error wrapped with which check
      failed
- [ ] make `newTestRepo(t)` run `migrations.Apply` on the temporary database before constructing the
      repository
- [ ] write a test asserting `NewRepository` returns `ErrSchemaNotInitialised` on an empty database
- [ ] write a test asserting it also returns the error when `files` exists but the failure columns do not —
      this is the exact production case, and a table-existence check would pass it
- [ ] write a test asserting `NewRepository` succeeds after `migrations.Apply`
- [ ] run tests — must pass before Task 4

### Task 4: Dockerfile stage for the migrate image

**Files:**
- Modify: `Dockerfile`

- [ ] add the migrate build RUN to the existing `build` stage exactly as given in Technical Details,
      including `GOARCH=$TARGETARCH`, the cache and bind mounts, and `-ldflags="-s -w"`
- [ ] add the `migrate-final` stage exactly as given, **placed before the existing `final` stage** so that
      `final` remains the last stage in the file
- [ ] leave the `final` stage byte-identical — verify with `git diff -- Dockerfile` that no line inside the
      `FROM alpine:latest AS final` block changed
- [ ] confirm `grep -c 'AS migrate-final' Dockerfile` prints 1
- [ ] **if `docker info` succeeds:** `docker build --target migrate-final -t mfs-migrate:check .` then
      `docker image inspect mfs-migrate:check --format '{{.Size}}'` prints a value below 20000000; also
      `docker build -t mfs-app:check .` (no `--target`) and confirm its entrypoint is `/bin/server`
- [ ] **if `docker info` fails:** record "docker unavailable — skipped" in the progress log and treat the
      two Docker items as satisfied; the `git diff` and `grep` checks above are the binding ones

### Task 5: Compose wiring

**Files:**
- Modify: `compose.yaml`
- Modify: `compose.override.yml`

- [ ] add the one-shot `migrate` service to `compose.yaml` exactly as prescribed: new image,
      `restart: "no"`, only the `.db` volume, no environment, no networks, no traefik labels
- [ ] add `depends_on` with `condition: service_completed_successfully` to `magnet-feed-sync`
- [ ] add the `migrate` override to `compose.override.yml` with **both** `build.target: migrate-final` and
      `image: magnet-feed-sync-migrate:local`, mirroring the app service — without the image override a
      local build stamps itself onto the registry tag
- [ ] **if `docker info` succeeds:**
      `docker compose config --format json | jq -e '.services["magnet-feed-sync"].depends_on.migrate.condition == "service_completed_successfully"'`
      exits 0. `variable is not set` warnings on stderr are expected, because `.env` is not committed
- [ ] **if `docker info` fails:** confirm instead that `compose.yaml` contains
      `service_completed_successfully` and a `migrate:` service with `restart: "no"`

### Task 6: Release pipeline builds both images

**Files:**
- Modify: `.gitea/workflows/release.yml`

- [ ] add `target: final` to the **existing** build-push step — without it the workflow builds whatever
      stage happens to be last, which is how the migrate image would end up published under the app's tags
- [ ] add a second build-push step after it with `target: migrate-final` and the tags and cache refs given
      verbatim in Technical Details
- [ ] keep the updater webhook and the Telegram notification as the final two steps, in that order
- [ ] confirm `grep -c 'id: version' .gitea/workflows/release.yml` prints 1 and
      `grep -c 'target:' .gitea/workflows/release.yml` prints 2

### Task 7: Verify the implementation

- [ ] confirm the server binary never links the migration library:
      `go list -deps ./app | grep -q sql-migrate` **exits 1** (no match). Note `grep -c` would print `0`
      and also exit 1 — use `-q` and judge by exit status only
- [ ] confirm `go list -deps ./cmd/migrate | grep -q sql-migrate` **exits 0**
- [ ] confirm no existing migration was renamed: every `.sql` name under `app/migrations/` other than
      `20240101000000-create-files.sql` also existed under `migrations/` at the base commit
      (`git show $(git merge-base HEAD origin/HEAD):migrations` vs `ls app/migrations`). Additional
      migrations introduced by unrelated work are allowed; renames of the existing ones are not
- [ ] confirm the only new exported identifiers are the three pre-approved in Code-Quality Rules
- [ ] run `go test ./... -race`, `go vet ./...`, `go build ./...`
- [ ] confirm `gofmt -s -l .` prints nothing

### Task 8: Update documentation and close out

**Files:**
- Modify: `CLAUDE.md`
- Modify: `README.md`

- [ ] update `CLAUDE.md`: migrations live in `app/migrations/` and are applied by a separate one-shot
      container before the app starts; the schema is declared **once**, in migrations; remove the note
      describing the old double declaration
- [ ] update `README.md`: the new migration flow, the second image, the updated `sql-migrate` paths, and
      the fact that a bare `go run ./app` now requires migrations to have been applied first
- [ ] move this plan to `docs/plans/completed/` — **do this only after the final review reports clean.** A
      reviewer that cannot find the plan at `docs/plans/20260811-migrations-on-deploy.md` should look in
      `docs/plans/completed/`; that is expected and is not a finding

## Post-Completion

*Items requiring external systems — informational only.*

- Merging to `master` triggers the release workflow, which builds both images and calls the updater. The
  first deploy after this lands applies the baseline (a no-op against the existing table) plus the two 2026
  migrations on its own — **no manual `ALTER TABLE` is needed**, and the 160 existing rows are preserved.
- The production container is currently **stopped by hand**; `docker compose up -d` during the deploy
  starts it again after the migrate container exits 0.
- After the deploy, confirm `GET /api/health` returns 200 with a real status body instead of the SQL error,
  and that `gorp_migrations` holds **six** records — five pre-existing ids plus the new baseline.
- Gatus recovers on its own once health returns 200.
