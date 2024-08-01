package download_client

import (
	"fmt"
	"magnet-feed-sync/app/config"
	downloadStation "magnet-feed-sync/app/download-client/download-station"
	"magnet-feed-sync/app/download-client/qbittorrent"
)

type Client interface {
	CreateDownloadTask(url string) error
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
