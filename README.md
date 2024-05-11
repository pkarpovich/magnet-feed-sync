# Magnet Feed Sync

## Introduction

**Magnet Feed Sync** is a Telegram bot designed to automate the management of torrent downloads from trackers. It parses
pages to extract magnet links, creates download tasks on a Synology NAS, and logs task details in a database. The bot
also monitors for updates on tracked pages and schedules new download tasks as needed.

## Features

- Automated creation of download tasks on Synology NAS from provided magnet links.
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

**Commands:**

- `/get_active_tasks` - Retrieve tasks for monitoring
- `/ping` - Check if bot is running

### Cron Jobs

Set to run every hour, checking for updates on tracked pages and initiating new download tasks if updates are found

## Configuration

Configure the bot using the following environment variables:

- `SYNOLOGY_URL`: URL to your Synology NAS.
- `SYNOLOGY_USERNAME`: NAS username.
- `SYNOLOGY_PASSWORD`: NAS password.
- `TELEGRAM_TOKEN`: Telegram bot token.
- `TELEGRAM_SUPER_USERS`: Comma-separated list of Telegram user IDs allowed to manage the bot.

## Contributors

To contribute to `magnet-feed-sync`, please fork the repository, create a feature branch, and submit a pull request.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
