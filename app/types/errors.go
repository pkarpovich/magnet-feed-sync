package types

import "errors"

// ErrTorrentNotFound reports that a magnet has no matching torrent in the download client.
// It lives here so consumers can branch on it without importing the concrete client.
var ErrTorrentNotFound = errors.New("torrent not found")
