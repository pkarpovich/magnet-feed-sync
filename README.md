# Magnet Feed Sync

## Introduction

**Magnet Feed Sync** is a Telegram bot and web interface for automating torrent download management. It parses
tracker pages to extract magnet links, creates download tasks on Synology NAS DownloadStation or qBittorrent, and logs task details in a database. The bot
also monitors for updates on tracked pages and schedules new download tasks as needed.

## Features

- Automated creation of download tasks on Synology NAS DownloadStation or qBittorrent from provided magnet links.
- Real-time interaction and management via Telegram.
- Persistent storage and management of download tasks.
- Database logging for task status and history.

## Usage

**Magnet Feed Sync** can be interacted through Telegram commands and automated cron jobs:

### Telegram Commands

Users can send commands to initiate downloads, view active tasks, or manage settings.

To create a new download task, send a message to the bot with tracker page.

**Supported Trackers:**

- [rutracker.org](https://rutracker.org)
- [nnmclub.to](https://nnmclub.to)
- [Jackett](https://github.com/Jackett/Jackett) (Torznab API) - any indexer supported by your Jackett instance

**Commands:**

- `/get_active_tasks` - Retrieve tasks for monitoring
- `/ping` - Check if bot is running

### HTTP API

Manage tracking tasks programmatically via the REST API:

- `POST /api/files` - Create a new download task (from URL or magnet link)
- `GET /api/files` - List all tracked tasks
- `DELETE /api/files/{fileId}` - Remove a tracked task
- `PATCH /api/files/{fileId}/refresh` - Force refresh a specific task
- `PATCH /api/files/refresh` - Force refresh all tasks
- `GET /api/file-locations` - Get available download locations
- `POST /api/file-locations` - Update download location for a task
- `GET /api/health` - Health check

**POST /api/files** examples:

With a tracker URL (enables update monitoring):
```json
{"url": "https://rutracker.org/forum/viewtopic.php?t=6810475", "location": "/downloads/tv shows"}
```

With a direct magnet link (no update monitoring):
```json
{"magnet": "magnet:?xt=urn:btih:...", "name": "Torrent Name", "location": "/downloads/movies"}
```

### Cron Jobs

Set to run every hour, checking for updates on tracked pages and initiating new download tasks if updates are found

## Configuration

Configure the bot using the following environment variables:

- `DOWNLOAD_CLIENT`: Download client type ("synology" or "qbittorrent").
- `SYNOLOGY_URL`: URL to your Synology NAS.
- `SYNOLOGY_USERNAME`: NAS username.
- `SYNOLOGY_PASSWORD`: NAS password.
- `QBITTORRENT_URL`: URL to your qBittorrent instance.
- `QBITTORRENT_USERNAME`: qBittorrent username.
- `QBITTORRENT_PASSWORD`: qBittorrent password.
- `TELEGRAM_TOKEN`: Telegram bot token.
- `TELEGRAM_SUPER_USERS`: Comma-separated list of Telegram user IDs allowed to manage the bot.
- `JACKETT_URL`: Jackett instance base URL (optional, enables Jackett/Torznab support).

## Contributors

To contribute to `magnet-feed-sync`, please fork the repository, create a feature branch, and submit a pull request.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
