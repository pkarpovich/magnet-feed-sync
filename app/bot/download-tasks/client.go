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
	downloadClient "magnet-feed-sync/app/download-client"
	"magnet-feed-sync/app/tracker"
	"magnet-feed-sync/app/utils"
)

type FileParser interface {
	Parse(ctx context.Context, url, location string) (*tracker.FileMetadata, error)
}

type FileStore interface {
	GetById(id string) (*tracker.FileMetadata, error)
	CreateOrReplace(metadata *tracker.FileMetadata) error
	GetAll() ([]*tracker.FileMetadata, error)
	Remove(id string) error
}

type Client struct {
	mu              sync.Mutex
	messagesForSend chan string
	tracker         FileParser
	dClient         downloadClient.Client
	store           FileStore
	dryMode         bool
}

type ClientCtx struct {
	MessagesForSend chan string
	Tracker         FileParser
	DClient         downloadClient.Client
	Store           FileStore
	DryMode         bool
}

func NewClient(ctx *ClientCtx) *Client {
	return &Client{
		messagesForSend: ctx.MessagesForSend,
		tracker:         ctx.Tracker,
		dClient:         ctx.DClient,
		dryMode:         ctx.DryMode,
		store:           ctx.Store,
	}
}

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

func (c *Client) CreateFromMagnet(ctx context.Context, hash, magnet, name, location string) (*tracker.FileMetadata, error) {
	metadata := &tracker.FileMetadata{
		ID:         hash,
		Name:       name,
		Magnet:     magnet,
		Location:   location,
		LastSyncAt: time.Now(),
	}

	return c.createWithLock(ctx, metadata)
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

func (c *Client) processFileMetadata(ctx context.Context, fileMetadata *tracker.FileMetadata) {
	ctx, span := otel.Tracer("download-tasks").Start(ctx, "processFileMetadata")
	defer span.End()

	if fileMetadata.OriginalUrl == "" {
		return
	}

	updatedMetadata, err := c.tracker.Parse(ctx, fileMetadata.OriginalUrl, "")
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "error parsing metadata", "error", err)
		return
	}

	c.mu.Lock()

	current, err := c.store.GetById(fileMetadata.ID)
	if err != nil {
		c.mu.Unlock()
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "error re-reading metadata", "error", err)
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
			slog.ErrorContext(ctx, "error updating metadata", "error", err)
		}

		c.mu.Unlock()
		return
	}
	slog.InfoContext(ctx, "magnet changed, re-downloading", "id", fileMetadata.ID)

	if err := c.store.CreateOrReplace(updatedMetadata); err != nil {
		slog.ErrorContext(ctx, "error updating metadata", "error", err)
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
		slog.ErrorContext(ctx, "error creating download task", "error", err)

		c.mu.Lock()
		updatedMetadata.Magnet = current.Magnet
		updatedMetadata.TorrentUpdatedAt = current.TorrentUpdatedAt
		if storeErr := c.store.CreateOrReplace(updatedMetadata); storeErr != nil {
			slog.ErrorContext(ctx, "error reverting metadata after download failure", "error", storeErr)
		}
		c.mu.Unlock()
		return
	}

	slog.InfoContext(ctx, "download task created", "name", updatedMetadata.Name)
	c.sendUpdateNotification(updatedMetadata)
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

	filesMetadata, err := c.store.GetAll()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "error getting files metadata", "error", err)
		return
	}

	for _, metadata := range filesMetadata {
		c.processFileMetadata(ctx, metadata)
	}
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
		slog.ErrorContext(ctx, "error getting metadata", "error", err)
		return
	}

	c.processFileMetadata(ctx, metadata)
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
