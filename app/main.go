package main

import (
	"log"
	"magnet-feed-sync/app/tracker"
)

func main() {
	url := "https://nnmclub.to/forum/viewtopic.php?t=1701587&start=15#pagestart"

	t := tracker.NewParser()
	metadata, err := t.Parse(url)
	if err != nil {
		log.Fatalf("Error parsing url: %s", err)
	}

	log.Printf("Title: %s", metadata.Name)
	log.Printf("Magnet: %s", metadata.Magnet)
	log.Printf("Rss: %s", metadata.RssUrl)
}
