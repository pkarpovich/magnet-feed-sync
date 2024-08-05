package download_client

import (
	"fmt"
	"magnet-feed-sync/app/config"
	downloadStation "magnet-feed-sync/app/download-client/download-station"
	"magnet-feed-sync/app/download-client/qbittorrent"
	"magnet-feed-sync/app/types"
)

type Client interface {
	CreateDownloadTask(url, destination string) error
	SetLocation(taskID, location string) error
	GetLocations() []types.Location
	GetHashByMagnet(magnet string) (string, error)
	GetDefaultLocation() string
}

const (
	QBittorrentClientType     = "qbittorrent"
	DownloadStationClientType = "download_station"
)

func NewClient(cfg config.Config) (Client, error) {
	switch cfg.DownloadClient {
	case QBittorrentClientType:
		return qbittorrent.NewClient(cfg.QBittorrent), nil
	case DownloadStationClientType:
		return downloadStation.NewClient(cfg.Synology), nil
	default:
		return nil, fmt.Errorf("client type not found: %s", cfg.DownloadClient)
	}
}
