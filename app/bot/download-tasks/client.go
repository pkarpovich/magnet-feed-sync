package download_tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

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
	}
}

type noopBreaker struct{}

func (noopBreaker) Allow(string) bool                         { return true }
func (noopBreaker) BeginRun()                                 {}
func (noopBreaker) RecordFailure(string, providers.ErrorKind) {}
func (noopBreaker) RecordSuccess(string)                      {}

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
		c.recordSyncFailure(ctx, fileMetadata.ID, err)
		if fromCron {
			c.recordParseFailure(fileMetadata.OriginalUrl, err)
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
	}
}

func (c *Client) recordSyncFailure(ctx context.Context, id string, cause error) {
	text := cause.Error()

	var providerErr *providers.ProviderError
	if errors.As(cause, &providerErr) {
		text = fmt.Sprintf("%s: %s", providerErr.Kind, providerErr.Err)
	}

	failure := taskStore.SyncFailure{Text: text, At: time.Now()}
	if err := c.store.RecordSyncFailure(id, failure); err != nil {
		slog.ErrorContext(ctx, "error recording sync failure", "error", err, "id", id)
	}
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
	defer func() { c.recordRun(ctx, runOk) }()

	c.breaker.BeginRun()

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
	return c.store.Remove(id)
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

	return fmt.Sprintf("```json\n%s\n```", string(jsonData)), nil
}
