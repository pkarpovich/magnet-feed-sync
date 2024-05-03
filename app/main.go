package main

import (
	"log"
	"magnet-feed-sync/app/config"
	downloadStation "magnet-feed-sync/app/download-station"
	"magnet-feed-sync/app/tracker"
)

func main() {
	url := "https://nnmclub.to/forum/viewtopic.php?t=1701587&start=15#pagestart"

	config, err := config.Init()
	if err != nil {
		log.Fatalf("Error reading config: %s", err)
	}

	t := tracker.NewParser()
	metadata, err := t.Parse(url)
	if err != nil {
		log.Fatalf("Error parsing url: %s", err)
	}

	log.Printf("Title: %s", metadata.Name)
	log.Printf("Magnet: %s", metadata.Magnet)
	log.Printf("Rss: %s", metadata.RssUrl)

	downloadClient := downloadStation.NewClient(config.Synology)
	err = downloadClient.CreateDownloadTask(metadata.Magnet)
	if err != nil {
		log.Fatalf("Error creating download task: %s", err)
	}
}
