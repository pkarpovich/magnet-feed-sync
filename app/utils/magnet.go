package utils

import "strings"

func ExtractBtihHash(magnet string) string {
	lower := strings.ToLower(magnet)
	idx := strings.Index(lower, "urn:btih:")
	if idx == -1 {
		return ""
	}
	hash := magnet[idx+len("urn:btih:"):]
	if ampIdx := strings.Index(hash, "&"); ampIdx != -1 {
		hash = hash[:ampIdx]
	}
	return strings.ToLower(hash)
}

// IsInfoHash reports whether s is a 40-char hex infohash — the only form qbittorrent's
// torrents/info reports, so a base32 magnet hash must never be matched against it
func IsInfoHash(s string) bool {
	if len(s) != 40 {
		return false
	}

	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}

	return true
}

func ExtractXtParam(magnet string) string {
	lower := strings.ToLower(magnet)
	for _, prefix := range []string{"?xt=", "&xt="} {
		idx := strings.Index(lower, prefix)
		if idx == -1 {
			continue
		}
		value := magnet[idx+len(prefix):]
		if ampIdx := strings.Index(value, "&"); ampIdx != -1 {
			value = value[:ampIdx]
		}
		return strings.ToLower(value)
	}
	return ""
}
