package main

import (
	"fmt"
	tbapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"log"
	downloadTasks "magnet-feed-sync/app/bot/download-tasks"
	"magnet-feed-sync/app/config"
	downloadStation "magnet-feed-sync/app/download-station"
	"magnet-feed-sync/app/events"
	"magnet-feed-sync/app/tracker"
)

func main() {
	log.Printf("[INFO] Starting app")

	cfg, err := config.Init()
	if err != nil {
		log.Fatalf("[ERROR] Error reading config: %s", err)
	}

	if err := run(cfg); err != nil {
		log.Fatalf("[ERROR] Error running app: %s", err)
	}
}

func run(cfg *config.Config) error {
	t := tracker.NewParser()
	dsClient := downloadStation.NewClient(cfg.Synology)

	downloadTasksClient := downloadTasks.NewClient(t, dsClient)

	tbAPI, err := tbapi.NewBotAPI(cfg.Telegram.Token)
	if err != nil {
		return fmt.Errorf("failed to create Telegram events: %w", err)
	}

	tgListener := &events.TelegramListener{
		SuperUsers: cfg.Telegram.SuperUsers,
		TbAPI:      tbAPI,
		Bot:        downloadTasksClient,
	}

	if err := tgListener.Do(); err != nil {
		return fmt.Errorf("failed to start Telegram listener: %w", err)
	}

	return nil
}
