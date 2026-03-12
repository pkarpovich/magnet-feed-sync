# Jackett Provider + POST /api/files Endpoint

## Overview
- Add Jackett as new tracker provider — парсит Torznab API для первичного добавления, сохраняет URL исходного трекера для мониторинга обновлений через HTML-провайдеры
- Add `POST /api/files` HTTP endpoint — создаёт tracking-задачу (то же что Telegram flow), принимает URL + location
- Рефакторинг Provider interface — убрать зависимость от goquery, каждый провайдер сам отвечает за fetch + parse

## Context (from discovery)
- Providers: `app/tracker/providers/` — `base.go` (Service interface), `rutracker.go`, `nnm.go`
- Parser: `app/tracker/parser.go` — `Parse()` fetches HTML, creates goquery doc, calls provider methods
- HTTP server: `app/http/client.go` — существующие endpoints (GET/PATCH/DELETE /api/files)
- Download tasks: `app/bot/download-tasks/client.go` — `processFileMetadata()`, `CheckForUpdates()`
- Events: `app/events/events.go` — Telegram message processing, folder commands
- Task store: `app/task-store/repository.go` — SQLite CRUD, files table
- Config: `app/config/config.go` — cleanenv-based config from env vars
- Tests: `app/tracker/providers/rutracker_test.go`, `app/events/events_test.go`, `app/tracker/parser_test.go`
- go.mod: Go 1.25, goquery, gofeed, cleanenv, testify

## Development Approach
- **Testing approach**: Regular (code first, then tests)
- Complete each task fully before moving to the next
- **CRITICAL: every task MUST include new/updated tests**
- **CRITICAL: all tests must pass before starting next task**
- **CRITICAL: update this plan file when scope changes during implementation**
- Maintain backward compatibility — existing RuTracker/NNM flows must keep working

## Testing Strategy
- **Unit tests**: mock-based testing through interfaces, testify/assert + require
- Table-driven tests for URL matching, parsing edge cases
- Testdata fixtures for Jackett XML responses (like existing rutracker HTML fixtures)

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix

## Implementation Steps

### Task 1: Refactor Provider interface
- [x] define new `Provider` interface in `app/tracker/providers/base.go`: `Parse(ctx, url) (*Result, error)`, `CanHandle(url) bool`
- [x] define `Result` struct: `ID string`, `Title string`, `Magnet string`, `UpdatedAt time.Time`, `Comment string`, `TrackerURL string` (for Jackett → original tracker URL)
- [x] refactor `RuTracker` to implement new interface — move HTML fetch + goquery parsing inside `Parse()` method
- [x] refactor `NNMClub` to implement new interface — same approach
- [x] update `app/tracker/parser.go` to use new interface: iterate providers via `CanHandle(url)`, call `Parse(ctx, url)`
- [x] remove old `getProviderByUrl()` and goquery fetch logic from parser
- [x] update existing tests in `rutracker_test.go` to match new interface (use httptest for HTML fixture serving)
- [x] update existing tests in `nnm_test.go` if present
- [x] update `parser_test.go` for new flow
- [x] run tests — must pass before next task

### Task 2: Add Jackett provider
- [x] add `JACKETT_URL` and `JACKETT_API_KEY` to config struct in `app/config/config.go`
- [x] create `app/tracker/providers/jackett.go` implementing new `Provider` interface
- [x] `CanHandle(url)` — match by Jackett base URL prefix
- [x] `Parse(ctx, url)` — HTTP GET to Jackett Torznab API, parse XML response
- [x] extract from Torznab XML: title, magnet (from `<link>` or `<enclosure>`), size, seeders, tracker name
- [x] extract `TrackerURL` from `<comments>` or `<guid>` field (URL исходного трекера для мониторинга)
- [x] return `Result` with `TrackerURL` populated — parser will save this as `original_url` for future update checks
- [x] add testdata XML fixtures with sample Jackett Torznab responses
- [x] write tests for XML parsing: valid response, empty results, missing fields
- [x] write tests for CanHandle: Jackett URL match, non-Jackett URL reject
- [x] write tests for TrackerURL extraction from different indexer formats
- [x] run tests — must pass before next task

### Task 3: Update parser to handle TrackerURL swap
- [x] in `parser.go`, after Jackett provider returns `Result` with `TrackerURL`: save `TrackerURL` as `original_url` in database (not the Jackett URL)
- [x] this means future update checks via `CheckForUpdates()` will use the tracker URL → routed to RuTracker/NNM HTML provider
- [x] if `TrackerURL` is empty (indexer didn't provide it) — save Jackett URL as fallback, skip update monitoring for this file
- [x] write tests for TrackerURL swap logic
- [x] write tests for empty TrackerURL fallback
- [x] run tests — must pass before next task

### Task 4: Add POST /api/files endpoint
- [x] add `POST /api/files` handler in `app/http/client.go`
- [x] request body: `{ "url": "string", "location": "string" (optional), "magnet": "string" (optional) }`
- [x] if `url` provided: parse through tracker provider → save to DB → send to download client
- [x] if `magnet` provided (without url): save directly with name from request, no monitoring (no original_url for updates)
- [x] use same `processFileMetadata()` or equivalent logic from download-tasks
- [x] return created file as JSON response (same format as GET /api/files items)
- [x] return 400 for invalid input, 500 for processing errors
- [x] write tests for POST handler: valid URL, valid magnet, missing both, invalid URL
- [x] write tests for location fallback to default
- [x] run tests — must pass before next task

### Task 5: Wire Jackett config and provider registration
- [ ] pass Jackett config to parser/provider registration in `app/main.go`
- [ ] register Jackett provider in the providers list (only if JACKETT_URL is configured)
- [ ] pass download-tasks client to HTTP server for POST endpoint (or extract shared logic)
- [ ] write integration-style test: Jackett URL → parsed → TrackerURL saved → update check uses HTML provider
- [ ] run tests — must pass before next task

### Task 6: Bump version
- [ ] bump minor version in `.semver.yaml` (new feature)

### Task 7: Verify acceptance criteria
- [ ] verify Jackett URL is recognized and creates a task with tracker's original_url
- [ ] verify RuTracker/NNM URLs still work as before (backward compatibility)
- [ ] verify `POST /api/files` with URL creates a task and returns result
- [ ] verify `POST /api/files` with magnet creates a task without monitoring
- [ ] verify hourly update check works for all provider types
- [ ] verify existing tasks (RuTracker/NNM) are not broken
- [ ] run full test suite
- [ ] run linter

### Task 8: [Final] Update compose and documentation
- [ ] add `JACKETT_URL` and `JACKETT_API_KEY` env vars to docker-compose.yaml
- [ ] update CLAUDE.md if new patterns discovered

## Technical Details

### New Provider Interface
```go
type Provider interface {
    Parse(ctx context.Context, url string) (*Result, error)
    CanHandle(url string) bool
}

type Result struct {
    ID         string
    Title      string
    Magnet     string
    UpdatedAt  time.Time
    Comment    string
    TrackerURL string // populated by Jackett: original tracker page URL for monitoring
}
```

### Jackett Torznab Response (XML)
```xml
<rss>
  <channel>
    <item>
      <title>Severance S02 2160p</title>
      <guid>https://rutracker.org/forum/viewtopic.php?t=6810475</guid>
      <comments>https://rutracker.org/forum/viewtopic.php?t=6810475</comments>
      <link>magnet:?xt=urn:btih:...</link>
      <size>52428800000</size>
      <pubDate>Mon, 10 Mar 2026 14:00:00 +0000</pubDate>
      <torznab:attr name="seeders" value="150"/>
      <jackettindexer id="rutracker">RuTracker</jackettindexer>
    </item>
  </channel>
</rss>
```

### TrackerURL Swap Flow
```
User sends Jackett URL
  → Jackett provider: Parse(ctx, jackettURL)
  → returns Result{
      TrackerURL: "https://rutracker.org/forum/viewtopic.php?t=6810475",
      Magnet: "magnet:?xt=...",
      Title: "Severance S02 2160p",
    }
  → Parser saves original_url = Result.TrackerURL (not Jackett URL)
  → Future update checks: original_url → RuTracker HTML provider
```

### POST /api/files
```
POST /api/files
Content-Type: application/json

// Option 1: URL tracking
{
  "url": "https://rutracker.org/forum/viewtopic.php?t=6810475",
  "location": "/downloads/tv shows"
}

// Option 2: Jackett URL tracking (saved as tracker URL)
{
  "url": "http://nas:9117/api/v2.0/indexers/rutracker/results/torznab?apikey=KEY&t=details&id=6810475"
}

// Option 3: Direct magnet (no monitoring)
{
  "magnet": "magnet:?xt=urn:btih:...",
  "name": "Severance S02",
  "location": "/downloads/tv shows"
}

Response: 201 Created
{
  "id": "6810475",
  "originalUrl": "https://rutracker.org/...",
  "name": "Severance S02 2160p",
  "magnet": "magnet:?xt=...",
  "location": "/downloads/tv shows"
}
```

### Config (new env vars)
| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| JACKETT_URL | no | empty | Jackett instance base URL |
| JACKETT_API_KEY | no | empty | Jackett API key |

## Post-Completion

**Manual verification:**
- Отправить Jackett URL в Telegram → проверить что задача создана с original_url трекера
- Отправить RuTracker URL → проверить что работает как раньше
- `POST /api/files` с URL → проверить создание задачи
- `POST /api/files` с magnet → проверить создание без мониторинга
- Подождать hourly check → убедиться что обновления работают для всех типов
- Проверить что JARVIS может создавать задачи через POST endpoint

**Deployment:**
- Добавить `JACKETT_URL` и `JACKETT_API_KEY` в secrets на сервере
- Убедиться что magnet-feed-sync имеет сетевой доступ к Jackett
