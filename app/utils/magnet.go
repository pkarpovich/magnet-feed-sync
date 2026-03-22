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
