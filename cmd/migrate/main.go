package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"magnet-feed-sync/app/database"
	"magnet-feed-sync/app/migrations"
)

func run() error {
	client, err := database.NewClient("tasks.db")
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}

	applied, err := migrations.Apply(client.DB())

	// closed explicitly on every path: a skipped Close leaves the WAL unrolled
	// for the app container that starts seconds later
	if closeErr := client.Close(); closeErr != nil {
		return errors.Join(err, fmt.Errorf("close database: %w", closeErr))
	}

	if err != nil {
		return err
	}

	slog.Info("migrations applied", "count", applied)

	return nil
}

func main() {
	if err := run(); err != nil {
		slog.Error("migrate failed", "error", err)
		os.Exit(1)
	}
}
