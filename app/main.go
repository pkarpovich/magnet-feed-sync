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
	"os"
	"os/signal"
	"syscall"
	"time"
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})

	dClient, err := downloadClient.NewClient(*cfg)
	t := tracker.NewParser(dClient)
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

	go tgListener.SendMessagesForAdmins(ctx)
	go http.NewClient(cfg.Http, store, downloadTasksClient, dClient).Start(ctx, done)

	go func() {
		if err := tgListener.Do(); err != nil {
			log.Printf("[ERROR] error in telegram listener: %s", err)
			panic(err)
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	cancel()

	select {
	case <-done:
		log.Println("[INFO] Application shutdown completed")
	case <-time.After(15 * time.Second):
		log.Println("[INFO] Application shutdown timed out")
	}

	return nil
}
