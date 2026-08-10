package download_tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"magnet-feed-sync/app/bot"
	taskStore "magnet-feed-sync/app/task-store"
	"magnet-feed-sync/app/tracker"
	"magnet-feed-sync/app/tracker/providers"
	"magnet-feed-sync/app/utils"
)

// FailureThreshold is the consecutive failure count at which a task counts as failing.
const FailureThreshold = 3

const deadTaskInterval = 24 * time.Hour

type FileParser interface {
	Parse(ctx context.Context, url, location string) (*tracker.FileMetadata, error)
	ProviderName(url string) string
}

type ProviderBreaker interface {
	Allow(name string) bool
	BeginRun()
	RecordFailure(name string, kind providers.ErrorKind)
	RecordSuccess(name string)
	Snapshot() map[string]tracker.State
}

type FileStore interface {
	GetById(id string) (*tracker.FileMetadata, error)
	CreateOrReplace(metadata *tracker.FileMetadata) error
	GetAll() ([]*tracker.FileMetadata, error)
	Remove(id string) error
	RecordSyncSuccess(id string, syncedAt time.Time) error
	RecordSyncFailure(id string, failure taskStore.SyncFailure) error
	SetLastRun(at time.Time, ok bool) error
}

type DownloadClient interface {
	CreateDownloadTask(url, destination string) error
}

type Client struct {
	mu              sync.Mutex
	messagesForSend chan string
	tracker         FileParser
	dClient         DownloadClient
	store           FileStore
	breaker         ProviderBreaker
	dryMode         bool

	// notifyMu guards failingNotified, the set of tasks the failing alert already went out
	// for; it is separate from mu because the alert is decided outside the store critical section
	notifyMu        sync.Mutex
	failingNotified map[string]struct{}
}

type ClientCtx struct {
	MessagesForSend chan string
	Tracker         FileParser
	DClient         DownloadClient
	Store           FileStore
	Breaker         ProviderBreaker
	DryMode         bool
}

func NewClient(ctx *ClientCtx) *Client {
	breaker := ctx.Breaker
	if breaker == nil {
		breaker = noopBreaker{}
	}

	return &Client{
		messagesForSend: ctx.MessagesForSend,
		tracker:         ctx.Tracker,
		dClient:         ctx.DClient,
		dryMode:         ctx.DryMode,
		store:           ctx.Store,
		breaker:         breaker,
		failingNotified: make(map[string]struct{}),
	}
}

type noopBreaker struct{}

func (noopBreaker) Allow(string) bool                         { return true }
func (noopBreaker) BeginRun()                                 {}
func (noopBreaker) RecordFailure(string, providers.ErrorKind) {}
func (noopBreaker) RecordSuccess(string)                      {}
func (noopBreaker) Snapshot() map[string]tracker.State        { return nil }

func (c *Client) OnMessage(ctx context.Context, msg bot.Message, location string) (bool, string, error) {
	metadata, err := c.CreateFromURL(ctx, msg.Text, location)
	if err != nil {
		return false, "", err
	}

	formatedMsg, err := MetadataToMsg(metadata)
	if err != nil {
		return false, "", err
	}

	replyMsg := fmt.Sprintf("✅ Download task created:\n\n%s", formatedMsg)
	return true, replyMsg, nil
}

func (c *Client) CreateFromURL(ctx context.Context, url, location string) (*tracker.FileMetadata, error) {
	metadata, err := c.tracker.Parse(ctx, url, location)
	if err != nil {
		return nil, err
	}

	slog.DebugContext(ctx, "metadata", "metadata", metadata)

	return c.createWithLock(ctx, metadata)
}

func (c *Client) DownloadNow(ctx context.Context, source, location string) error {
	if c.dryMode {
		slog.InfoContext(ctx, "dry mode is enabled, skipping one-shot download", "location", location)
		return nil
	}

	return c.dClient.CreateDownloadTask(source, location)
}

func (c *Client) createWithLock(ctx context.Context, metadata *tracker.FileMetadata) (*tracker.FileMetadata, error) {
	c.mu.Lock()

	existing, getErr := c.store.GetById(metadata.ID)
	if getErr != nil && !errors.Is(getErr, sql.ErrNoRows) {
		c.mu.Unlock()
		return nil, fmt.Errorf("check existing task: %w", getErr)
	}
	hadActiveRow := existing != nil && !existing.DeleteAt.Valid

	err := c.store.CreateOrReplace(metadata)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}

	c.mu.Unlock()

	if c.dryMode {
		return metadata, nil
	}

	err = c.dClient.CreateDownloadTask(metadata.Magnet, metadata.Location)
	if err != nil {
		c.rollbackCreate(ctx, metadata.ID, existing, hadActiveRow)
		return nil, err
	}

	slog.InfoContext(ctx, "download task created", "name", metadata.Name)

	return metadata, nil
}

func (c *Client) rollbackCreate(ctx context.Context, id string, existing *tracker.FileMetadata, hadActiveRow bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	current, err := c.store.GetById(id)
	if err != nil {
		slog.ErrorContext(ctx, "failed to read task for rollback", "error", err)
		return
	}

	if current.DeleteAt.Valid {
		return
	}

	if hadActiveRow {
		if restoreErr := c.store.CreateOrReplace(existing); restoreErr != nil {
			slog.ErrorContext(ctx, "failed to restore previous file after download error", "error", restoreErr)
		}
	} else {
		if removeErr := c.store.Remove(id); removeErr != nil {
			slog.ErrorContext(ctx, "failed to remove file after download error", "error", removeErr)
		}
	}
}

func (c *Client) processFileMetadata(ctx context.Context, fileMetadata *tracker.FileMetadata, fromCron bool) {
	ctx, span := otel.Tracer("download-tasks").Start(ctx, "processFileMetadata")
	defer span.End()

	if fileMetadata.OriginalUrl == "" {
		return
	}

	updatedMetadata, err := c.tracker.Parse(ctx, fileMetadata.OriginalUrl, "")
	if err != nil {
		// an aborted request says nothing about the tracker, so it must not count as a
		// failure for the task, the breaker or the notifications
		if ctx.Err() != nil {
			slog.InfoContext(ctx, "parse aborted", "error", ctx.Err(), "id", fileMetadata.ID)
			return
		}

		text := c.recordSyncFailure(ctx, fileMetadata.ID, err)
		if fromCron {
			c.recordParseFailure(fileMetadata.OriginalUrl, err)
			c.notifyFailing(fileMetadata, text)
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "error parsing metadata", "error", err, "id", fileMetadata.ID, "url", fileMetadata.OriginalUrl)
		return
	}

	c.recordSyncSuccess(ctx, fileMetadata.ID)

	if fromCron {
		if name := c.tracker.ProviderName(fileMetadata.OriginalUrl); name != "" {
			c.breaker.RecordSuccess(name)
		}
		c.notifyRecovered(fileMetadata)
	}

	c.mu.Lock()

	current, err := c.store.GetById(fileMetadata.ID)
	if err != nil {
		c.mu.Unlock()
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "error re-reading metadata", "error", err, "id", fileMetadata.ID, "url", fileMetadata.OriginalUrl)
		return
	}

	if current.DeleteAt.Valid {
		c.mu.Unlock()
		return
	}

	if current.Location != "" {
		updatedMetadata.Location = current.Location
	}

	updatedMetadata.LastSyncAt = time.Now()
	if magnetsEqual(current.Magnet, updatedMetadata.Magnet) {
		slog.InfoContext(ctx, "magnet unchanged, updating metadata silently", "id", fileMetadata.ID)

		if err := c.store.CreateOrReplace(updatedMetadata); err != nil {
			slog.ErrorContext(ctx, "error updating metadata", "error", err, "id", fileMetadata.ID, "url", fileMetadata.OriginalUrl)
		}

		c.mu.Unlock()
		return
	}
	slog.InfoContext(ctx, "magnet changed, re-downloading", "id", fileMetadata.ID)

	if err := c.store.CreateOrReplace(updatedMetadata); err != nil {
		slog.ErrorContext(ctx, "error updating metadata", "error", err, "id", fileMetadata.ID, "url", fileMetadata.OriginalUrl)
		c.mu.Unlock()
		return
	}

	c.mu.Unlock()

	if c.dryMode {
		slog.InfoContext(ctx, "dry mode is enabled, skipping download")
		c.sendUpdateNotification(updatedMetadata)
		return
	}

	if err := c.dClient.CreateDownloadTask(updatedMetadata.Magnet, updatedMetadata.Location); err != nil {
		slog.ErrorContext(ctx, "error creating download task", "error", err, "id", fileMetadata.ID, "url", fileMetadata.OriginalUrl)

		c.mu.Lock()
		updatedMetadata.Magnet = current.Magnet
		updatedMetadata.TorrentUpdatedAt = current.TorrentUpdatedAt
		if storeErr := c.store.CreateOrReplace(updatedMetadata); storeErr != nil {
			slog.ErrorContext(ctx, "error reverting metadata after download failure", "error", storeErr, "id", fileMetadata.ID, "url", fileMetadata.OriginalUrl)
		}
		c.mu.Unlock()
		return
	}

	slog.InfoContext(ctx, "download task created", "name", updatedMetadata.Name)
	c.sendUpdateNotification(updatedMetadata)
}

func (c *Client) recordSyncSuccess(ctx context.Context, id string) {
	if err := c.store.RecordSyncSuccess(id, time.Now()); err != nil {
		slog.ErrorContext(ctx, "error recording sync success", "error", err, "id", id)
		return
	}

	// the streak is over, so the next one has to be able to alert again — including when
	// this success came from a manual refresh, which never alerts itself
	c.clearFailingNotified(id)
}

func (c *Client) recordSyncFailure(ctx context.Context, id string, cause error) string {
	text := cause.Error()

	var providerErr *providers.ProviderError
	if errors.As(cause, &providerErr) {
		text = providerErr.Error()
	}

	failure := taskStore.SyncFailure{Text: text, At: time.Now()}
	if err := c.store.RecordSyncFailure(id, failure); err != nil {
		slog.ErrorContext(ctx, "error recording sync failure", "error", err, "id", id)
	}

	return text
}

// notifyFailing fires once per failing streak, on the first cron run that observes the task
// at or above the threshold. It cannot key off the exact 2→3 transition: a manual refresh
// increments the counter without notifying, so the crossing run may not be a cron run at all,
// and the alert would then be lost for good.
func (c *Client) notifyFailing(metadata *tracker.FileMetadata, lastError string) {
	// ConsecutiveFailures is the count read before this run's own increment
	failures := metadata.ConsecutiveFailures + 1
	if failures < FailureThreshold {
		return
	}

	if !c.markFailingNotified(metadata.ID) {
		return
	}

	c.messagesForSend <- escapeMarkdown(fmt.Sprintf(
		"⚠️ Task is failing after %d attempts:\n\n%s (%s)\n\n%s",
		failures, metadata.Name, metadata.ID, lastError,
	))
}

// markFailingNotified claims the alert for id, reporting whether this caller won it.
func (c *Client) markFailingNotified(id string) bool {
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()

	if _, done := c.failingNotified[id]; done {
		return false
	}
	c.failingNotified[id] = struct{}{}

	return true
}

func (c *Client) clearFailingNotified(id string) {
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()
	delete(c.failingNotified, id)
}

func (c *Client) notifyRecovered(metadata *tracker.FileMetadata) {
	if metadata.ConsecutiveFailures < FailureThreshold {
		return
	}

	c.messagesForSend <- escapeMarkdown(fmt.Sprintf(
		"✅ Task recovered after %d failures:\n\n%s (%s)\n\nlast error: %s",
		metadata.ConsecutiveFailures, metadata.Name, metadata.ID, metadata.LastError,
	))
}

// escapeMarkdown makes plain text safe for the MarkdownV2 parse mode every admin
// message is sent with (see events.NewMarkdownMessage) — telegram rejects the whole
// message when a reserved char such as '(' or '-' is left unescaped.
//
// '\' has to be doubled up first, and by hand: tbapi.EscapeText leaves it alone, so a
// task name like `a\*b` would reach telegram as `a\\*b` — a literal backslash followed
// by an entity-opening '*' instead of the escaped literals we meant.
func escapeMarkdown(text string) string {
	return tbapi.EscapeText(tbapi.ModeMarkdownV2, strings.ReplaceAll(text, "\\", "\\\\"))
}

func (c *Client) recordParseFailure(url string, err error) {
	name := c.tracker.ProviderName(url)
	if name == "" {
		return
	}

	var providerErr *providers.ProviderError
	if !errors.As(err, &providerErr) {
		return
	}

	c.breaker.RecordFailure(name, providerErr.Kind)
}

func (c *Client) sendUpdateNotification(metadata *tracker.FileMetadata) {
	formatedMsg, err := MetadataToMsg(metadata)
	if err != nil {
		slog.Error("error formatting metadata", "error", err)
		return
	}
	c.messagesForSend <- fmt.Sprintf("✅ Metadata updated:\n\n%s", formatedMsg)
}

func magnetsEqual(a, b string) bool {
	hashA := utils.ExtractBtihHash(a)
	hashB := utils.ExtractBtihHash(b)
	if hashA != "" && hashB != "" {
		return hashA == hashB
	}
	xtA := utils.ExtractXtParam(a)
	xtB := utils.ExtractXtParam(b)
	if xtA != "" && xtB != "" {
		return xtA == xtB
	}
	return a == b
}

func (c *Client) CheckForUpdates(ctx context.Context) {
	ctx, span := otel.Tracer("download-tasks").Start(ctx, "CheckForUpdates")
	defer span.End()

	slog.InfoContext(ctx, "checking for updates")

	runOk := true
	defer func() {
		// a sweep cut short by shutdown never finished, so it must not overwrite the
		// last run state the health endpoint reads
		if ctx.Err() != nil {
			return
		}
		c.recordRun(ctx, runOk)
	}()

	c.breaker.BeginRun()
	before := c.breaker.Snapshot()

	filesMetadata, err := c.store.GetAll()
	if err != nil {
		runOk = false
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "error getting files metadata", "error", err)
		return
	}

	skipped := make(map[string]int)
	for _, metadata := range filesMetadata {
		// shutdown cancels the sweep context: stop instead of failing every remaining
		// task, which would trip the breaker and notify on each restart
		if ctx.Err() != nil {
			slog.InfoContext(ctx, "update sweep interrupted", "error", ctx.Err())
			return
		}

		if c.isStretched(metadata) {
			slog.DebugContext(ctx, "task is failing, retry postponed", "id", metadata.ID, "failures", metadata.ConsecutiveFailures)
			continue
		}

		name := c.tracker.ProviderName(metadata.OriginalUrl)
		if name != "" && !c.breaker.Allow(name) {
			skipped[name]++
			continue
		}

		c.processFileMetadata(ctx, metadata, true)
	}

	for name, count := range skipped {
		slog.InfoContext(ctx, "provider is blocked, tasks skipped", "provider", name, "skipped", count)
	}

	c.notifyBreakerTransitions(breakerDelta{before: before, after: c.breaker.Snapshot(), skipped: skipped})
}

// RefreshAll re-checks every task on demand. It records store outcomes so the counters stay
// truthful, but leaves the breaker, the alerts and the cron run state alone: a human pressing
// refresh must not trip a provider, fire alerts, or hide a dead cron from the health endpoint.
func (c *Client) RefreshAll(ctx context.Context) {
	ctx, span := otel.Tracer("download-tasks").Start(ctx, "RefreshAll")
	defer span.End()

	slog.InfoContext(ctx, "refreshing all tasks")

	filesMetadata, err := c.store.GetAll()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "error getting files metadata", "error", err)
		return
	}

	for _, metadata := range filesMetadata {
		if ctx.Err() != nil {
			slog.InfoContext(ctx, "manual refresh interrupted", "error", ctx.Err())
			return
		}

		c.processFileMetadata(ctx, metadata, false)
	}
}

type breakerDelta struct {
	before  map[string]tracker.State
	after   map[string]tracker.State
	skipped map[string]int
}

func (c *Client) notifyBreakerTransitions(delta breakerDelta) {
	for name, state := range delta.after {
		was := delta.before[name]
		switch {
		case state.Tripped && !was.Tripped:
			c.messagesForSend <- escapeMarkdown(fmt.Sprintf(
				"🚫 Provider %s is blocked, %d task(s) skipped this run, next probe at %s",
				name, delta.skipped[name], state.NextProbeAt.Format(time.RFC3339),
			))
		case was.Tripped && !state.Tripped:
			c.messagesForSend <- escapeMarkdown(fmt.Sprintf("✅ Provider %s is reachable again", name))
		}
	}
}

func (c *Client) recordRun(ctx context.Context, ok bool) {
	if err := c.store.SetLastRun(time.Now(), ok); err != nil {
		slog.ErrorContext(ctx, "error recording run state", "error", err)
	}
}

func (c *Client) isStretched(metadata *tracker.FileMetadata) bool {
	return metadata.ConsecutiveFailures >= FailureThreshold &&
		time.Since(metadata.LastErrorAt.Time) < deadTaskInterval
}

func (c *Client) RemoveTask(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.store.Remove(id); err != nil {
		return err
	}
	c.clearFailingNotified(id)

	return nil
}

func (c *Client) UpdateTaskLocation(id, location string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	file, err := c.store.GetById(id)
	if err != nil {
		return fmt.Errorf("get task: %w", err)
	}

	if file.DeleteAt.Valid {
		return fmt.Errorf("task %s has been deleted", id)
	}

	file.Location = location
	return c.store.CreateOrReplace(file)
}

func (c *Client) CheckFileForUpdates(ctx context.Context, fileId string) {
	metadata, err := c.store.GetById(fileId)
	if err != nil {
		slog.ErrorContext(ctx, "error getting metadata", "error", err, "id", fileId)
		return
	}

	// a deleted row must not accrue sync counters, which would make the health endpoint
	// count a task nobody tracks any more
	if metadata.DeleteAt.Valid {
		slog.InfoContext(ctx, "task is deleted, refresh skipped", "id", fileId)
		return
	}

	c.processFileMetadata(ctx, metadata, false)
}

func MetadataToMsg(metadata *tracker.FileMetadata) (string, error) {
	comment := metadata.LastComment
	runes := []rune(comment)
	if len(runes) > 100 {
		comment = string(runes[:100]) + "..."
	}

	display := *metadata
	display.LastComment = comment

	jsonData, err := json.MarshalIndent(&display, "", "  ")
	if err != nil {
		return "", err
	}

	// inside a MarkdownV2 code block only backticks and backslashes are reserved, and a
	// single unescaped one from a task name or magnet makes telegram reject the message
	body := strings.NewReplacer("\\", "\\\\", "`", "\\`").Replace(string(jsonData))

	return fmt.Sprintf("```json\n%s\n```", body), nil
}
