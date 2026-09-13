package download_tasks

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"magnet-feed-sync/app/bot"
	"magnet-feed-sync/app/notify"
	taskStore "magnet-feed-sync/app/task-store"
	"magnet-feed-sync/app/tracker"
	"magnet-feed-sync/app/tracker/providers"
	"magnet-feed-sync/app/types"
	"magnet-feed-sync/app/utils"
)

const (
	// A torrent that changed within freshFor is checked every sweep; one quiet for longer is
	// checked every settledEvery, and past settledFor once a day. Most tracked pages are years
	// old, and asking Cloudflare about them hourly was the bulk of the solver timeouts.
	freshFor     = 14 * 24 * time.Hour
	settledFor   = 90 * 24 * time.Hour
	settledEvery = 6 * time.Hour
	dormantEvery = 24 * time.Hour
	// dueSlack absorbs sweep jitter: an interval measured against sweeps an hour apart would
	// otherwise slip by a whole sweep whenever the previous one finished a minute late.
	dueSlack = 5 * time.Minute
)

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
	UpdateSettings(id string, notify bool, location string) error
	GetAll() ([]*tracker.FileMetadata, error)
	Remove(id string) error
	RecordSyncSuccess(id string, syncedAt time.Time) error
	RecordSyncFailure(id string, failure taskStore.SyncFailure) error
	SetLastRun(at time.Time, ok bool) error
}

type DownloadClient interface {
	CreateDownloadTask(url, destination string) (string, error)
}

type notifier interface {
	Publish(ctx context.Context, m notify.Message) error
}

type Client struct {
	mu              sync.Mutex
	messagesForSend chan string
	tracker         FileParser
	dClient         DownloadClient
	store           FileStore
	breaker         ProviderBreaker
	notifier        notifier
	dryMode         bool

	// notifyMu guards staleNotified, the set of tasks the stale message already went out for;
	// it is separate from mu because the message is decided outside the store critical section
	notifyMu      sync.Mutex
	staleNotified map[string]struct{}
}

type ClientCtx struct {
	MessagesForSend chan string
	Tracker         FileParser
	DClient         DownloadClient
	Store           FileStore
	Breaker         ProviderBreaker
	Notifier        notifier
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
		notifier:        ctx.Notifier,
		staleNotified:   make(map[string]struct{}),
	}
}

type noopBreaker struct{}

func (noopBreaker) Allow(string) bool                         { return true }
func (noopBreaker) BeginRun()                                 {}
func (noopBreaker) RecordFailure(string, providers.ErrorKind) {}
func (noopBreaker) RecordSuccess(string)                      {}
func (noopBreaker) Snapshot() map[string]tracker.State        { return nil }

func (c *Client) OnMessage(ctx context.Context, msg bot.Message, location string) (bool, string, error) {
	// a message from a human must never arm the agent
	metadata, err := c.CreateFromURL(ctx, msg.Text, location, false)
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

func (c *Client) CreateFromURL(ctx context.Context, url, location string, notify bool) (*tracker.FileMetadata, error) {
	metadata, err := c.tracker.Parse(ctx, url, location)
	if err != nil {
		return nil, err
	}
	metadata.Notify = notify

	slog.DebugContext(ctx, "metadata", "metadata", metadata)

	return c.createWithLock(ctx, metadata)
}

func (c *Client) DownloadNow(ctx context.Context, source, location string) (string, error) {
	if c.dryMode {
		slog.InfoContext(ctx, "dry mode is enabled, skipping one-shot download", "location", location)
		return "", nil
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
	if existing != nil && !existing.DeleteAt.Valid {
		c.mu.Unlock()
		return nil, types.ErrFileAlreadyTracked
	}

	err := c.store.CreateOrReplace(metadata)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}

	c.mu.Unlock()

	// CreateOrReplace wrote the freshly parsed metadata, so the failure counters are back to
	// zero; leaving the id in the set would swallow the message for the next stale streak
	c.clearStaleNotified(metadata.ID)

	if c.dryMode {
		return metadata, nil
	}

	_, err = c.dClient.CreateDownloadTask(metadata.Magnet, metadata.Location)
	if err != nil {
		c.rollbackCreate(ctx, metadata.ID)
		return nil, err
	}

	slog.InfoContext(ctx, "download task created", "name", metadata.Name)

	return metadata, nil
}

func (c *Client) rollbackCreate(ctx context.Context, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.store.Remove(id); err != nil {
		slog.ErrorContext(ctx, "failed to remove file after download error", "error", err)
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

		text, kind, recorded := c.recordSyncFailure(ctx, fileMetadata.ID, err)
		if fromCron {
			c.recordParseFailure(fileMetadata.OriginalUrl, err)
			// the store did not move, so a message would describe a state it does not hold
			if recorded {
				c.notifyFailure(ctx, fileMetadata, kind, text)
			}
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "error parsing metadata", "error", err, "id", fileMetadata.ID, "url", utils.RedactURL(fileMetadata.OriginalUrl))
		return
	}

	c.recordSyncSuccess(ctx, fileMetadata.ID)

	if fromCron {
		if name := c.tracker.ProviderName(fileMetadata.OriginalUrl); name != "" {
			c.breaker.RecordSuccess(name)
		}
	}

	c.mu.Lock()

	current, err := c.store.GetById(fileMetadata.ID)
	if err != nil {
		c.mu.Unlock()
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "error re-reading metadata", "error", err, "id", fileMetadata.ID, "url", utils.RedactURL(fileMetadata.OriginalUrl))
		return
	}

	if current.DeleteAt.Valid {
		c.mu.Unlock()
		return
	}

	if current.Location != "" {
		updatedMetadata.Location = current.Location
	}
	// unconditional: notify has no sentinel value, so guarding it the way Location is guarded
	// would clear the flag for every tracked file with an empty location
	updatedMetadata.Notify = current.Notify

	updatedMetadata.LastSyncAt = time.Now()
	if magnetsEqual(current.Magnet, updatedMetadata.Magnet) {
		slog.InfoContext(ctx, "magnet unchanged, updating metadata silently", "id", fileMetadata.ID)

		if err := c.store.CreateOrReplace(updatedMetadata); err != nil {
			slog.ErrorContext(ctx, "error updating metadata", "error", err, "id", fileMetadata.ID, "url", utils.RedactURL(fileMetadata.OriginalUrl))
		}

		c.mu.Unlock()
		return
	}
	slog.InfoContext(ctx, "magnet changed, re-downloading", "id", fileMetadata.ID)

	if err := c.store.CreateOrReplace(updatedMetadata); err != nil {
		slog.ErrorContext(ctx, "error updating metadata", "error", err, "id", fileMetadata.ID, "url", utils.RedactURL(fileMetadata.OriginalUrl))
		c.mu.Unlock()
		return
	}

	c.mu.Unlock()

	if c.dryMode {
		slog.InfoContext(ctx, "dry mode is enabled, skipping download")
		c.sendUpdateNotification(ctx, updatedMetadata)
		return
	}

	if _, err := c.dClient.CreateDownloadTask(updatedMetadata.Magnet, updatedMetadata.Location); err != nil {
		// the new torrent is already in the client, added by hand or through /api/downloads:
		// the store keeps the new magnet, otherwise every sweep would find it "changed" again
		// and fail the same way for good (two tasks did exactly that, hourly, for a day)
		if errors.Is(err, types.ErrTorrentAlreadyExists) {
			slog.InfoContext(ctx, "torrent already in the download client, magnet accepted", "id", fileMetadata.ID)
			return
		}

		slog.ErrorContext(ctx, "error creating download task", "error", err, "id", fileMetadata.ID, "url", utils.RedactURL(fileMetadata.OriginalUrl))

		c.mu.Lock()
		updatedMetadata.Magnet = current.Magnet
		updatedMetadata.TorrentUpdatedAt = current.TorrentUpdatedAt
		if storeErr := c.store.CreateOrReplace(updatedMetadata); storeErr != nil {
			slog.ErrorContext(ctx, "error reverting metadata after download failure", "error", storeErr, "id", fileMetadata.ID, "url", utils.RedactURL(fileMetadata.OriginalUrl))
		}
		c.mu.Unlock()
		return
	}

	slog.InfoContext(ctx, "download task created", "name", updatedMetadata.Name)

	// a human pressing refresh must not wake the agent, the same rule the breaker and the
	// run state already follow
	if fromCron {
		c.publishReleaseUpdate(ctx, updatedMetadata)
	}

	c.sendUpdateNotification(ctx, updatedMetadata)
}

const releaseSubjectPrefix = "tuclaw.releases.updated."

// a dot or a space in the token would grow the subject an extra token and stop matching
// the filter the agent armed
var subjectTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type releaseUpdatePayload struct {
	FileID           string `json:"file_id"`
	Name             string `json:"name"`
	PageURL          string `json:"page_url"`
	LastComment      string `json:"last_comment"`
	Location         string `json:"location"`
	TorrentUpdatedAt string `json:"torrent_updated_at"`
	UpdatedAt        string `json:"updated_at"`
}

func (c *Client) publishReleaseUpdate(ctx context.Context, metadata *tracker.FileMetadata) {
	if !metadata.Notify || c.notifier == nil {
		return
	}

	if !subjectTokenPattern.MatchString(metadata.ID) {
		slog.ErrorContext(ctx, "file id is not a valid subject token, release update skipped", "id", metadata.ID)
		return
	}

	body, err := json.Marshal(releaseUpdatePayload{
		FileID:           metadata.ID,
		Name:             metadata.Name,
		PageURL:          metadata.OriginalUrl,
		LastComment:      metadata.LastComment,
		Location:         metadata.Location,
		TorrentUpdatedAt: metadata.TorrentUpdatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:        time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to build release update payload", "id", metadata.ID, "error", err)
		return
	}

	digest := sha256.Sum256([]byte(metadata.Magnet))
	msg := notify.Message{
		Subject: releaseSubjectPrefix + metadata.ID,
		MsgID:   metadata.ID + ":" + hex.EncodeToString(digest[:]),
		Payload: body,
	}

	if err := c.notifier.Publish(ctx, msg); err != nil {
		slog.ErrorContext(ctx, "failed to publish release update", "id", metadata.ID, "error", err)
	}
}

func (c *Client) recordSyncSuccess(ctx context.Context, id string) {
	if err := c.store.RecordSyncSuccess(id, time.Now()); err != nil {
		slog.ErrorContext(ctx, "error recording sync success", "error", err, "id", id)
		return
	}

	// the streak is over, so the next one has to be able to speak again — including when
	// this success came from a manual refresh, which never messages itself
	c.clearStaleNotified(id)
}

// recordSyncFailure reports the error text and kind it stored and whether the store moved.
// An error nobody classified is treated as transient: retrying is the cheap mistake.
func (c *Client) recordSyncFailure(ctx context.Context, id string, cause error) (string, string, bool) {
	text := cause.Error()
	kind := providers.KindTransient

	var providerErr *providers.ProviderError
	if errors.As(cause, &providerErr) {
		text = providerErr.Error()
		kind = providerErr.Kind
	}

	failure := taskStore.SyncFailure{Text: text, Kind: kind.Key(), At: time.Now()}
	if err := c.store.RecordSyncFailure(id, failure); err != nil {
		slog.ErrorContext(ctx, "error recording sync failure", "error", err, "id", id)
		return text, kind.Key(), false
	}

	return text, kind.Key(), true
}

// notifyFailure is the only place a failed check reaches the operator, and only for two
// reasons. A permanent failure parks the task: it is said once, on the way in, and the
// task is not checked again until a human acts (the previous kind is what metadata still
// carries, this run's write is not in it yet). Anything else is the tracker's bad hour and
// stays in the logs until the task has had no successful check for StaleAfter, when it is
// said once per streak. The counters, the breaker and recoveries are never announced:
// the operator learned to skip those, and then skipped the one that needed a decision.
func (c *Client) notifyFailure(ctx context.Context, metadata *tracker.FileMetadata, kind, lastError string) {
	if kind == providers.KindPermanent.Key() {
		if metadata.Parked() {
			return
		}

		c.send(ctx, escapeMarkdown(fmt.Sprintf(
			"⛔ Parked: %s (%s)\n\n%s\n\nThe page cannot be read any more, so it is not checked again. Track the new page if the topic moved, or remove this one.",
			metadata.Name, metadata.ID, lastError,
		)))
		return
	}

	if !metadata.Stale(time.Now()) {
		return
	}

	if !c.markStaleNotified(metadata.ID) {
		return
	}

	days := int(time.Since(metadata.LastSyncAt).Hours() / 24)
	c.send(ctx, escapeMarkdown(fmt.Sprintf(
		"⏳ No successful check for %d days: %s (%s)\n\nlast error: %s",
		days, metadata.Name, metadata.ID, lastError,
	)))
}

// send hands a message to the admin sender, giving up when the app context is cancelled:
// the consumer stops on shutdown, and a bare send on the unbuffered channel would then
// block this goroutine forever and skip the deferred run-state write.
func (c *Client) send(ctx context.Context, msg string) {
	select {
	case c.messagesForSend <- msg:
	case <-ctx.Done():
		slog.InfoContext(ctx, "dropped admin notification, shutting down")
	}
}

// markStaleNotified claims the message for id, reporting whether this caller won it.
func (c *Client) markStaleNotified(id string) bool {
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()

	if _, done := c.staleNotified[id]; done {
		return false
	}
	c.staleNotified[id] = struct{}{}
	return true
}

func (c *Client) clearStaleNotified(id string) {
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()

	delete(c.staleNotified, id)
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

func (c *Client) sendUpdateNotification(ctx context.Context, metadata *tracker.FileMetadata) {
	formatedMsg, err := MetadataToMsg(metadata)
	if err != nil {
		slog.ErrorContext(ctx, "error formatting metadata", "error", err)
		return
	}
	c.send(ctx, fmt.Sprintf("✅ Metadata updated:\n\n%s", formatedMsg))
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

	filesMetadata, err := c.store.GetAll()
	if err != nil {
		runOk = false
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "error getting files metadata", "error", err)
		return
	}

	now := time.Now()
	skipped := make(map[string]int)
	var checked, parked, deferred int
	for _, metadata := range filesMetadata {
		// shutdown cancels the sweep context: stop instead of failing every remaining
		// task, which would trip the breaker on each restart
		if ctx.Err() != nil {
			slog.InfoContext(ctx, "update sweep interrupted", "error", ctx.Err())
			return
		}

		if metadata.Parked() {
			parked++
			continue
		}

		if !isDue(metadata, now) {
			deferred++
			continue
		}

		name := c.tracker.ProviderName(metadata.OriginalUrl)
		if name != "" && !c.breaker.Allow(name) {
			skipped[name]++
			continue
		}

		checked++
		c.processFileMetadata(ctx, metadata, true)
	}

	for name, count := range skipped {
		slog.InfoContext(ctx, "provider is blocked, tasks skipped", "provider", name, "skipped", count)
	}

	slog.InfoContext(ctx, "update sweep finished", "checked", checked, "deferred", deferred, "parked", parked)
}

// checkInterval is how often a task is worth a request, from how recently its torrent changed.
// Zero means every sweep.
func checkInterval(torrentUpdatedAt, now time.Time) time.Duration {
	if torrentUpdatedAt.IsZero() {
		return 0
	}

	age := now.Sub(torrentUpdatedAt)
	switch {
	case age < freshFor:
		return 0
	case age < settledFor:
		return settledEvery
	default:
		return dormantEvery
	}
}

func isDue(metadata *tracker.FileMetadata, now time.Time) bool {
	interval := checkInterval(metadata.TorrentUpdatedAt, now)
	if interval == 0 {
		return true
	}

	last := metadata.LastAttemptAt()
	if last.IsZero() {
		return true
	}

	return now.Sub(last) >= interval-dueSlack
}

// RefreshAll re-checks every task on demand, parked and deferred ones included. It records
// store outcomes so the counters stay truthful, but leaves the breaker, the messages and the
// cron run state alone: a human pressing refresh must not trip a provider, send messages, or
// hide a dead cron from the health endpoint.
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

func (c *Client) recordRun(ctx context.Context, ok bool) {
	if err := c.store.SetLastRun(time.Now(), ok); err != nil {
		slog.ErrorContext(ctx, "error recording run state", "error", err)
	}
}

func (c *Client) RemoveTask(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.store.Remove(id); err != nil {
		return err
	}
	c.clearStaleNotified(id)

	return nil
}

func (c *Client) UpdateTaskLocation(id, location string) error {
	_, err := c.UpdateTaskSettings(id, nil, &location)
	return err
}

func (c *Client) UpdateTaskSettings(id string, notify *bool, location *string) (*tracker.FileMetadata, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	file, err := c.store.GetById(id)
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}

	if file.DeleteAt.Valid {
		return nil, fmt.Errorf("task %s: %w", id, sql.ErrNoRows)
	}

	if notify != nil {
		file.Notify = *notify
	}
	if location != nil {
		file.Location = *location
	}

	if err := c.store.UpdateSettings(id, file.Notify, file.Location); err != nil {
		return nil, fmt.Errorf("update task settings: %w", err)
	}

	return file, nil
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
