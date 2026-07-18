# Magnet Feed Sync

## Introduction

**Magnet Feed Sync** is a Telegram bot and web interface for automating torrent download management. It parses
tracker pages to extract magnet links, creates download tasks on qBittorrent, and logs task details in a database. The bot
also monitors for updates on tracked pages and schedules new download tasks as needed.

## Features

- Automated creation of download tasks on qBittorrent from provided magnet links.
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

- `POST /api/files` - Create a new tracked download task from a tracker URL (enables update monitoring)
- `POST /api/downloads` - One-shot fire-and-forget download from a magnet or `.torrent` URL (not monitored, no history)
- `GET /api/files` - List all tracked tasks
- `DELETE /api/files/{fileId}` - Remove a tracked task
- `PATCH /api/files/{fileId}/refresh` - Force refresh a specific task
- `PATCH /api/files/refresh` - Force refresh all tasks
- `GET /api/file-locations` - Get available download locations
- `POST /api/file-locations` - Update download location for a task
- `GET /api/health` - Health check

**POST /api/files** - tracker URL only (parses the page, persists a row, monitors for updates):
```json
{"url": "https://rutracker.org/forum/viewtopic.php?t=6810475", "location": "/downloads/tv shows"}
```
A bare `magnet` is no longer accepted here (it cannot be monitored); use `POST /api/downloads` instead.

**POST /api/downloads** - one-shot download, handed straight to the download client. The `source` is
forwarded verbatim (qBittorrent fetches a `.torrent` URL or raises a magnet itself); nothing is parsed,
persisted, or monitored. `location` is optional and defaults to the client's configured location.
Responds `201 {"status":"ok"}`.

With a magnet:
```json
{"source": "magnet:?xt=urn:btih:...", "location": "/downloads/movies"}
```

With a Jackett `/dl/` `.torrent` URL:
```json
{"source": "https://jackett.example.com/dl/indexer/?jackett_apikey=...&path=...", "location": "/downloads/movies"}
```

### Cron Jobs

Set to run every hour, checking for updates on tracked pages and initiating new download tasks if updates are found

## Configuration

Configure the bot using the following environment variables:

- `QBITTORRENT_URL`: URL to your qBittorrent instance.
- `QBITTORRENT_USERNAME`: qBittorrent username.
- `QBITTORRENT_PASSWORD`: qBittorrent password.
- `QBITTORRENT_DESTINATION`: Default download location on qBittorrent.
- `TELEGRAM_TOKEN`: Telegram bot token.
- `TELEGRAM_SUPER_USERS`: Comma-separated list of Telegram user IDs allowed to manage the bot.
- `JACKETT_URL`: Jackett instance base URL (optional, enables Jackett/Torznab support).

> Breaking change: the Synology DownloadStation client has been removed. qBittorrent is now the only supported download client. Remove any `DOWNLOAD_CLIENT` and `SYNOLOGY_*` variables from your environment.

## Contributors

To contribute to `magnet-feed-sync`, please fork the repository, create a feature branch, and submit a pull request.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
