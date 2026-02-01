package providers

import (
	"os"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html/charset"
)

func TestRutrackerProvider_GetLastUpdatedDate(t *testing.T) {
	tests := []struct {
		name        string
		fixtureFile string
		wantStable  bool
		wantNonZero bool
	}{
		{
			name:        "topic_6810475_should_return_stable_date",
			fixtureFile: "testdata/rutracker_6810475.html",
			wantStable:  true,
			wantNonZero: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file, err := os.Open(tt.fixtureFile)
			require.NoError(t, err, "failed to open fixture file")
			defer file.Close()

			utf8Reader, err := charset.NewReaderLabel("windows-1251", file)
			require.NoError(t, err, "failed to create charset reader")

			doc, err := goquery.NewDocumentFromReader(utf8Reader)
			require.NoError(t, err, "failed to parse HTML")

			provider := &RutrackerProvider{}

			result1 := provider.GetLastUpdatedDate(doc)
			time.Sleep(10 * time.Millisecond)
			result2 := provider.GetLastUpdatedDate(doc)

			if tt.wantStable {
				assert.Equal(t, result1, result2, "GetLastUpdatedDate should return stable value, not time.Now()")
			}

			if tt.wantNonZero {
				assert.False(t, result1.IsZero(), "GetLastUpdatedDate should return non-zero time")
			}

			if tt.wantStable && tt.wantNonZero {
				assert.True(t, result1.Before(time.Now().Add(-time.Minute)),
					"GetLastUpdatedDate returned recent time (likely time.Now()), got: %v", result1)
			}
		})
	}
}

func TestRutrackerProvider_GetId(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "valid_url",
			url:  "https://rutracker.org/forum/viewtopic.php?t=6810475",
			want: "6810475",
		},
		{
			name: "url_with_extra_params",
			url:  "https://rutracker.org/forum/viewtopic.php?t=123456&start=0",
			want: "123456",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &RutrackerProvider{}
			got := provider.GetId(tt.url)
			assert.Equal(t, tt.want, got)
		})
	}
}
