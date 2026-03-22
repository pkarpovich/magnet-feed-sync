# Fix false torrent update detection

## Overview
- `processFileMetadata` compares `TorrentUpdatedAt` (the "ред." edit date from RuTracker) to detect torrent updates
- The "ред." date changes on ANY first-post edit (typos, description, stats), not only when the actual torrent changes
- This causes false update notifications and re-downloads every time the post is edited
- Fix: compare magnet links instead of edit dates — magnet changes only when the torrent is actually re-uploaded
- Discovered on page https://rutracker.org/forum/viewtopic.php?t=3304959 where frequent post edits trigger hourly false positives

## Context (from discovery)
- Main logic: `app/bot/download-tasks/client.go` — `processFileMetadata()` method, lines 141-219
- Previous similar fix: commit `d24f427` fixed `time.Now()` fallback that caused false detection
- Test fixture already saved: `app/tracker/providers/testdata/rutracker_3304959.html`
- No tests exist for `download-tasks/client.go` — needs interfaces for mockable dependencies
- Existing mock pattern: `app/http/client_test.go` uses interface mocks for `TaskCreator`, `FileStore`
- Dependencies of `Client`: `*tracker.Parser` (concrete), `downloadClient.Client` (interface), `*taskStore.Repository` (concrete)

## Development Approach
- **Testing approach**: Regular (code first, then tests)
- Complete each task fully before moving to the next
- **CRITICAL: every task MUST include new/updated tests**
- **CRITICAL: all tests must pass before starting next task**
- **CRITICAL: update this plan file when scope changes during implementation**
- Maintain backward compatibility — existing update detection flow must keep working for actual torrent changes

## Testing Strategy
- **Unit tests**: mock-based testing through interfaces, testify/assert + require
- Table-driven tests for processFileMetadata scenarios
- Testdata fixture for RuTracker page t=3304959

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix

## Implementation Steps

### Task 1: Add RuTracker parser test for page t=3304959
- [ ] write test `TestRutrackerProvider_Parse_3304959` using existing fixture `testdata/rutracker_3304959.html`
- [ ] verify correct ID extraction ("3304959")
- [ ] verify title, magnet link parsed
- [ ] verify "ред." date parsed correctly (22-Мар-26 12:59 → stable time)
- [ ] write `TestRutrackerProvider_Parse_3304959_StableDate` — two consecutive parses return same date
- [ ] run tests — must pass before next task

### Task 2: Extract interfaces in download-tasks package for testability
- [ ] define `FileParser` interface: `Parse(url, location string) (*tracker.FileMetadata, error)`
- [ ] define `FileStore` interface: `GetById(id string) (*tracker.FileMetadata, error)`, `CreateOrReplace(metadata *tracker.FileMetadata) error`, `GetAll() ([]*tracker.FileMetadata, error)`, `Remove(id string) error`
- [ ] update `Client` struct to use interfaces instead of concrete `*tracker.Parser` and `*taskStore.Repository`
- [ ] update `ClientCtx` struct accordingly
- [ ] update `NewClient` constructor
- [ ] update `app/main.go` and `app/http/client.go` if needed (should be compatible since concrete types implement interfaces)
- [ ] run tests — must pass before next task

### Task 3: Fix processFileMetadata — compare magnet links
- [ ] in `processFileMetadata`: after parsing updated metadata, compare `current.Magnet == updatedMetadata.Magnet`
- [ ] if magnets match: update metadata in DB (date, name, comment, last_sync_at) without re-download or notification
- [ ] if magnets differ: keep existing behavior (re-download + Telegram notification)
- [ ] write tests for `processFileMetadata`:
  - same magnet, different date → no re-download, no notification, metadata updated in DB
  - different magnet → re-download triggered, notification sent
  - same magnet, same date → metadata updated (last_sync_at), no re-download
  - parse error → no crash, logged
- [ ] run tests — must pass before next task

### Task 4: Verify acceptance criteria
- [ ] verify page t=3304959 fixture parses with stable date
- [ ] verify same-magnet edits don't trigger re-download
- [ ] verify actual torrent changes (different magnet) still trigger re-download + notification
- [ ] verify existing RuTracker/NNM/Jackett flows unaffected
- [ ] run full test suite
- [ ] run linter

## Technical Details

### Current comparison (broken for post-edit-only changes):
```go
if current.TorrentUpdatedAt.Equal(updatedMetadata.TorrentUpdatedAt) {
    // "up to date" — only updates last_sync_at
    return
}
// "outdated" — re-downloads + notifies
```

### New comparison (magnet-based):
```go
if current.Magnet == updatedMetadata.Magnet {
    // torrent unchanged — update metadata silently (date, name, comment)
    return
}
// torrent actually changed — re-download + notify
```

### Interfaces for download-tasks testability:
```go
type FileParser interface {
    Parse(url, location string) (*tracker.FileMetadata, error)
}

type FileStore interface {
    GetById(id string) (*tracker.FileMetadata, error)
    CreateOrReplace(metadata *tracker.FileMetadata) error
    GetAll() ([]*tracker.FileMetadata, error)
    Remove(id string) error
}
```

## Post-Completion

**Manual verification:**
- Add page t=3304959 to tracking → verify no false "Metadata updated" notifications on hourly checks
- Edit-only changes on a tracked page should NOT trigger re-download
- Actual torrent re-uploads (new magnet) should still trigger re-download + notification
