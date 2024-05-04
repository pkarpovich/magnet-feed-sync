package database

import (
	"database/sql"
	"fmt"
	_ "modernc.org/sqlite"
	"os"
)

type Client struct {
	db *sql.DB
}

func NewClient(filename string) (*Client, error) {
	db, err := openDB(filename)
	if err != nil {
		return nil, err
	}

	return &Client{db: db}, nil
}

func createFolderIfNotExists(folder string) error {
	if _, err := os.Stat(folder); os.IsNotExist(err) {
		return os.Mkdir(folder, os.ModePerm)
	}

	return nil
}

func openDB(filename string) (*sql.DB, error) {
	dbFolder := ".db"
	if err := createFolderIfNotExists(dbFolder); err != nil {
		return nil, err
	}

	dbPath := fmt.Sprintf("%s/%s", dbFolder, filename)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}

	return db, nil
}

func (c *Client) Close() error {
	return c.db.Close()
}

func (c *Client) Exec(query string, args ...any) (sql.Result, error) {
	return c.db.Exec(query, args...)
}

func (c *Client) Query(query string, args ...any) (*sql.Rows, error) {
	return c.db.Query(query, args...)
}
