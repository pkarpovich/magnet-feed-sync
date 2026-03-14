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
