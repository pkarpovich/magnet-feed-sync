package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	_ "modernc.org/sqlite"
	"os"
	"strings"
	"time"
)

type Client struct {
	db *sql.DB
}

func NewClient(filename string) (*Client, error) {
	db, err := openDB(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Hour)

	return &Client{db: db}, nil
}

// carried in the DSN rather than executed once after opening: everything except
// journal_mode is per-connection state, and `db.Exec` reaches whichever single connection
// the pool hands out, leaving the other nine at the SQLite defaults (busy_timeout 0)
var sqlitePragmas = []string{
	"journal_mode(WAL)",
	"busy_timeout(30000)",
	"synchronous(NORMAL)",
	"cache_size(1000)",
	"foreign_keys(ON)",
}

func createFolderIfNotExists(folder string) error {
	if _, err := os.Stat(folder); os.IsNotExist(err) {
		return os.MkdirAll(folder, os.ModePerm)
	}
	return nil
}

func openDB(filename string) (*sql.DB, error) {
	dbFolder := ".db"
	if err := createFolderIfNotExists(dbFolder); err != nil {
		return nil, fmt.Errorf("failed to create database folder: %w", err)
	}

	dbPath := fmt.Sprintf("file:%s/%s?_pragma=%s", dbFolder, filename, strings.Join(sqlitePragmas, "&_pragma="))
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	if err := db.Ping(); err != nil {
		if err := db.Close(); err != nil {
			return nil, fmt.Errorf("failed to close database: %w", err)
		}

		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	return db, nil
}

func (c *Client) DB() *sql.DB {
	return c.db
}

func (c *Client) Close() error {
	return c.db.Close()
}

func (c *Client) withRetry(ctx context.Context, op func() error) error {
	maxRetries := 5
	backoff := 100 * time.Millisecond

	for attempt := 0; attempt < maxRetries; attempt++ {
		err := op()
		if err == nil || errors.Is(err, sql.ErrNoRows) {
			return err
		}

		if err.Error() == "database is locked (5) (SQLITE_BUSY)" {
			jitter := time.Duration(rand.Intn(100)) * time.Millisecond
			sleepTime := backoff + jitter

			select {
			case <-time.After(sleepTime):
				backoff *= 2
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		return err
	}

	return fmt.Errorf("failed after %d attempts", maxRetries)
}

func (c *Client) ExecWithRetry(ctx context.Context, query string, args ...any) (sql.Result, error) {
	var result sql.Result

	err := c.withRetry(ctx, func() error {
		var err error
		result, err = c.db.ExecContext(ctx, query, args...)
		return err
	})

	return result, err
}

func (c *Client) QueryWithRetry(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	var rows *sql.Rows

	err := c.withRetry(ctx, func() error {
		var err error
		rows, err = c.db.QueryContext(ctx, query, args...)
		return err
	})

	return rows, err
}

func (c *Client) QueryRowWithRetry(ctx context.Context, query string, args ...any) *sql.Row {
	var row *sql.Row

	_ = c.withRetry(ctx, func() error {
		row = c.db.QueryRowContext(ctx, query, args...)
		var dummy int
		err := row.Scan(&dummy)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})

	return c.db.QueryRowContext(ctx, query, args...)
}

func (c *Client) Exec(query string, args ...any) (sql.Result, error) {
	ctx := context.Background()
	return c.ExecWithRetry(ctx, query, args...)
}

func (c *Client) Query(query string, args ...any) (*sql.Rows, error) {
	ctx := context.Background()
	return c.QueryWithRetry(ctx, query, args...)
}

func (c *Client) QueryRow(query string, args ...any) *sql.Row {
	ctx := context.Background()
	return c.QueryRowWithRetry(ctx, query, args...)
}
