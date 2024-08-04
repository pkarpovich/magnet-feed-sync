package main

import (
	"context"
	"fmt"
	tbapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"log"
	downloadTasks "magnet-feed-sync/app/bot/download-tasks"
	"magnet-feed-sync/app/config"
	"magnet-feed-sync/app/database"
	downloadClient "magnet-feed-sync/app/download-client"
	"magnet-feed-sync/app/events"
	"magnet-feed-sync/app/http"
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
	ctx := context.Background()
	t := tracker.NewParser()
	dClient, err := downloadClient.NewClient(*cfg)
	if err != nil {
		return fmt.Errorf("failed to create download client: %w", err)
	}

	db, err := database.NewClient("tasks.db")
	if err != nil {
		return fmt.Errorf("failed to create database client: %w", err)
	}
	store, err := taskStore.NewRepository(db)
	if err != nil {
		return fmt.Errorf("failed to create task store: %w", err)
	}

	messagesForSend := make(chan string)

	downloadTasksClient := downloadTasks.NewClient(&downloadTasks.ClientCtx{
		Tracker:         t,
		DClient:         dClient,
		Store:           store,
		DryMode:         cfg.DryMode,
		MessagesForSend: messagesForSend,
	})

	s, err := schedular.NewService(cfg)
	if err != nil {
		return fmt.Errorf("failed to create scheduler: %w", err)
	}

	go func() {
		err = s.Start(downloadTasksClient.CheckForUpdates)
		if err != nil {
			log.Fatalf("[ERROR] Error starting scheduler: %s", err)
		}
	}()

	tbAPI, err := tbapi.NewBotAPI(cfg.Telegram.Token)
	if err != nil {
		return fmt.Errorf("failed to create Telegram events: %w", err)
	}

	tgListener := &events.TelegramListener{
		SuperUsers:      cfg.Telegram.SuperUsers,
		TbAPI:           tbAPI,
		Bot:             downloadTasksClient,
		Store:           store,
		MessagesForSend: messagesForSend,
	}

	go tgListener.SendMessagesForAdmins()
	go http.NewClient(cfg.Http, store).Start(ctx)

	if err := tgListener.Do(); err != nil {
		return fmt.Errorf("failed to start Telegram listener: %w", err)
	}

	return nil
}
