package main

import (
	"fmt"
	tbapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"log"
	downloadTasks "magnet-feed-sync/app/bot/download-tasks"
	"magnet-feed-sync/app/config"
	"magnet-feed-sync/app/database"
	downloadStation "magnet-feed-sync/app/download-station"
	"magnet-feed-sync/app/events"
	"magnet-feed-sync/app/schedular"
	taskStore "magnet-feed-sync/app/task-store"
	"magnet-feed-sync/app/tracker"
)

func main() {
	log.Printf("[INFO] Starting app")

	cfg, err := config.Init()
	if err != nil {
		log.Fatalf("[ERROR] Error reading config: %s", err)
	}

	if cfg.DryMode {
		log.Printf("[WARN] Dry mode is enabled")
	}

	if err := run(cfg); err != nil {
		log.Fatalf("[ERROR] Error running app: %s", err)
	}

}

func run(cfg *config.Config) error {
	t := tracker.NewParser()
	dsClient := downloadStation.NewClient(cfg.Synology)

	db, err := database.NewClient("tasks.db")
	if err != nil {
		return fmt.Errorf("failed to create database client: %w", err)
	}
	store, err := taskStore.NewRepository(db)
	if err != nil {
		return fmt.Errorf("failed to create task store: %w", err)
	}

	downloadTasksClient := downloadTasks.NewClient(&downloadTasks.ClientCtx{
		Tracker:  t,
		DSClient: dsClient,
		Store:    store,
		DryMode:  cfg.DryMode,
	})

	s, err := schedular.NewService()
	if err != nil {
		return fmt.Errorf("failed to create scheduler: %w", err)
	}

	go func() {
		err = s.Start(func() {
			log.Printf("[INFO] Running scheduler")

			filesMetadata, err := store.GetAll()
			if err != nil {
				log.Fatalf("[ERROR] Error getting files metadata: %s", err)
			}

			for _, metadata := range filesMetadata {
				updatedMetadata, err := t.Parse(metadata.OriginalUrl)
				if err != nil {
					log.Printf("[ERROR] Error parsing metadata: %s", err)
					continue
				}

				if metadata.TorrentUpdatedAt == updatedMetadata.TorrentUpdatedAt {
					log.Printf("[INFO] Metadata is up to date: %s", metadata.ID)
					continue
				}
				log.Printf("[INFO] Metadata is outdated: %s", metadata.ID)

				if err := store.CreateOrReplace(updatedMetadata); err != nil {
					log.Printf("[ERROR] Error updating metadata: %s", err)
				}
				log.Printf("[INFO] Metadata updated: %s", metadata.ID)

				if cfg.DryMode {
					log.Printf("[INFO] Dry mode is enabled, skipping download")
					continue
				}

				if err := dsClient.CreateDownloadTask(updatedMetadata.Magnet); err != nil {
					log.Printf("[ERROR] Error creating download task: %s", err)
				}

				log.Printf("[INFO] Download task created: %s", updatedMetadata.Name)
			}

		})
		if err != nil {
			log.Fatalf("[ERROR] Error starting scheduler: %s", err)
		}
	}()

	tbAPI, err := tbapi.NewBotAPI(cfg.Telegram.Token)
	if err != nil {
		return fmt.Errorf("failed to create Telegram events: %w", err)
	}

	tgListener := &events.TelegramListener{
		SuperUsers: cfg.Telegram.SuperUsers,
		TbAPI:      tbAPI,
		Bot:        downloadTasksClient,
		Store:      store,
	}

	if err := tgListener.Do(); err != nil {
		return fmt.Errorf("failed to start Telegram listener: %w", err)
	}

	return nil
}
