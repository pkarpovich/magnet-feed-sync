package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExtractBtihHash(t *testing.T) {
	tests := []struct {
		name     string
		magnet   string
		expected string
	}{
		{
			name:     "standard magnet",
			magnet:   "magnet:?xt=urn:btih:abc123def456&dn=test",
			expected: "abc123def456",
		},
		{
			name:     "magnet without extra params",
			magnet:   "magnet:?xt=urn:btih:abc123def456",
			expected: "abc123def456",
		},
		{
			name:     "no btih",
			magnet:   "magnet:?dn=test",
			expected: "",
		},
		{
			name:     "empty string",
			magnet:   "",
			expected: "",
		},
		{
			name:     "uppercase URN",
			magnet:   "magnet:?xt=URN:BTIH:ABC123&dn=test",
			expected: "abc123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, ExtractBtihHash(tt.magnet))
		})
	}
}
