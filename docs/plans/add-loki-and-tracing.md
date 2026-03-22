# Add Loki logging and OpenTelemetry tracing

## Overview
- Migrate from `log.Printf` to structured `slog` logging with Loki backend
- Add OpenTelemetry tracing with OTLP HTTP exporter
- Copy proven observability package from `jackett-mcp` project
- Full migration: 81 `log.Printf` calls across 12 files → `slog`

## Context (from discovery)
- Reference implementation: `/Users/pavel.karpovich/Projects/turtle-hub/services/jackett-mcp/observability/`
  - `loki.go` — custom `slog.Handler` with batching, level labels, trace_id correlation, multiHandler
  - `tracing.go` — OTLP HTTP tracing setup with noop fallback
  - Both have comprehensive tests
- Current logging: `log.Printf` with manual `[INFO]`/`[ERROR]`/`[WARN]`/`[DEBUG]` prefixes
- Files to migrate (by call count): `http/client.go` (19), `download-tasks/client.go` (19), `events/events.go` (14), `main.go` (9), `rutracker.go` (7), `nnm.go` (5), `download-station/client.go` (3), remaining 5 files (1 each)
- Context is already threaded in providers (`Parse(ctx, url)`) and HTTP handlers
- go.mod: Go 1.25, needs otel dependencies added

## Development Approach
- **Testing approach**: Regular (code first, then tests)
- Complete each task fully before moving to the next
- **CRITICAL: every task MUST include new/updated tests**
- **CRITICAL: all tests must pass before starting next task**
- **CRITICAL: update this plan file when scope changes during implementation**
- Use `slog.SetDefault(logger)` pattern (same as jackett-mcp) — no need to thread logger through dependencies

## Testing Strategy
- **Unit tests**: copy tests from jackett-mcp for observability package (already comprehensive)
- Verify existing tests still pass after each slog migration batch
- Tracing tests: verify noop when endpoint empty, verify spans created when enabled

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix

## Implementation Steps

### Task 1: Copy observability package and add dependencies
- [ ] create `app/observability/` directory
- [ ] copy `loki.go` from jackett-mcp, update package name and module path
- [ ] copy `tracing.go` from jackett-mcp, update package name and module path
- [ ] copy `loki_test.go` and `tracing_test.go`, update package name and module path
- [ ] add otel dependencies to go.mod: `go.opentelemetry.io/otel`, `otel/trace`, `otel/sdk`, `otel/exporters/otlp/otlptrace/otlptracehttp`
- [ ] run `go mod tidy`
- [ ] run observability tests — must pass before next task

### Task 2: Add config and wire observability in main.go
- [ ] add `OtelServiceName`, `OtelEndpoint`, `LokiURL` fields to `config.Config` in `app/config/config.go`
- [ ] wire `observability.SetupLogging()` and `observability.SetupTracing()` in `app/main.go`
- [ ] replace all `log.Printf`/`log.Fatalf` in `main.go` with `slog.Info`/`slog.Error`
- [ ] replace `log.Printf` in `config/config.go` with `slog.Warn`
- [ ] write test for new config fields (env parsing)
- [ ] run tests — must pass before next task

### Task 3: Migrate core business logic logging to slog
- [ ] migrate `app/bot/download-tasks/client.go` (19 calls): replace `log.Printf("[LEVEL]...")` → `slog.Level("msg", attrs...)`
- [ ] migrate `app/http/client.go` (19 calls): same pattern, use `r.Context()` where available for trace correlation
- [ ] migrate `app/events/events.go` (14 calls): same pattern
- [ ] verify existing tests pass after migration
- [ ] run tests — must pass before next task

### Task 4: Migrate tracker providers logging to slog
- [ ] migrate `app/tracker/providers/rutracker.go` (7 calls)
- [ ] migrate `app/tracker/providers/nnm.go` (5 calls)
- [ ] migrate `app/tracker/providers/base.go` (1 call)
- [ ] verify existing provider tests pass
- [ ] run tests — must pass before next task

### Task 5: Migrate infrastructure logging to slog
- [ ] migrate `app/database/client.go` (0 direct log calls, but verify)
- [ ] migrate `app/download-client/download-station/client.go` (3 calls)
- [ ] migrate `app/download-client/qbittorrent/client.go` (1 call)
- [ ] migrate `app/schedular/schedular.go` (1 call)
- [ ] migrate `app/task-store/repository.go` (1 call)
- [ ] remove `"log"` imports from all migrated files
- [ ] run tests — must pass before next task

### Task 6: Add tracing spans to key operations
- [ ] add tracing spans to HTTP handlers in `app/http/client.go` (use `r.Context()`)
- [ ] add tracing spans to tracker `Parse()` methods (already receive `ctx`)
- [ ] add tracing span to `processFileMetadata` and `CheckForUpdates` in download-tasks
- [ ] write tests for span creation (verify span names, check noop when tracing disabled)
- [ ] run tests — must pass before next task

### Task 7: Update compose and verify
- [ ] add `OTEL_SERVICE_NAME`, `OTEL_EXPORTER_OTLP_ENDPOINT`, `LOKI_URL` env vars to `compose.yaml`
- [ ] verify all `log.Printf` calls are gone: `grep -rn "log\.Printf\|log\.Fatalf\|log\.Println" app/`
- [ ] run full test suite
- [ ] run linter

## Technical Details

### slog migration pattern
```
// Before:
log.Printf("[INFO] Starting app")
log.Printf("[ERROR] Error parsing metadata: %s", err)
log.Printf("[DEBUG] Metadata: %+v", metadata)

// After:
slog.Info("starting app")
slog.Error("error parsing metadata", "error", err)
slog.Debug("metadata", "metadata", metadata)
```

### Config additions
```go
type Config struct {
    // ... existing fields ...
    OtelServiceName string `env:"OTEL_SERVICE_NAME" env-default:"magnet-feed-sync"`
    OtelEndpoint    string `env:"OTEL_EXPORTER_OTLP_ENDPOINT"`
    LokiURL         string `env:"LOKI_URL"`
}
```

### main.go wiring
```go
logger, cleanupLog := observability.SetupLogging(cfg.OtelServiceName, cfg.LokiURL)
defer cleanupLog()
slog.SetDefault(logger)

shutdownTracing, err := observability.SetupTracing(ctx, cfg.OtelServiceName, cfg.OtelEndpoint)
if err != nil { ... }
defer shutdownTracing(ctx)
```

### New dependencies
```
go.opentelemetry.io/otel v1.42.0
go.opentelemetry.io/otel/trace v1.42.0
go.opentelemetry.io/otel/sdk v1.42.0
go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.42.0
```

## Post-Completion

**Manual verification:**
- Deploy with `LOKI_URL` set → verify logs appear in Grafana Loki with service/level labels
- Deploy with `OTEL_EXPORTER_OTLP_ENDPOINT` set → verify traces appear in Tempo/Jaeger
- Verify trace_id correlation between logs and traces
- Verify graceful shutdown flushes pending logs and trace spans

**External system updates:**
- Add `LOKI_URL` and `OTEL_EXPORTER_OTLP_ENDPOINT` to deployment secrets
- Ensure network access from magnet-feed-sync container to Loki and OTLP collector
