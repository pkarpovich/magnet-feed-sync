package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "jackett download link loses its api key",
			raw:  "http://jackett:9117/dl/nnmclub/?jackett_apikey=s3cret&path=abc&file=one.torrent",
			want: "http://jackett:9117/dl/nnmclub/?file=one.torrent&jackett_apikey=redacted&path=abc",
		},
		{
			name: "torznab query loses its api key",
			raw:  "http://jackett:9117/api/v2.0/indexers/all/results/torznab?apikey=s3cret&t=search&q=one",
			want: "http://jackett:9117/api/v2.0/indexers/all/results/torznab?apikey=redacted&q=one&t=search",
		},
		{
			name: "userinfo is masked",
			raw:  "http://user:pass@solver:8191/v1",
			want: "http://redacted@solver:8191/v1",
		},
		{
			name: "a clean url is returned verbatim",
			raw:  "https://rutracker.org/forum/viewtopic.php?t=123",
			want: "https://rutracker.org/forum/viewtopic.php?t=123",
		},
		{
			name: "a magnet carries no credential and is untouched",
			raw:  "magnet:?xt=urn:btih:2566e2b012ea1ef9087465bc97a7ac4449f4f0de&dn=Some.Name",
			want: "magnet:?xt=urn:btih:2566e2b012ea1ef9087465bc97a7ac4449f4f0de&dn=Some.Name",
		},
		{
			name: "unparseable input discloses nothing",
			raw:  "http://\x7f/?apikey=s3cret",
			want: "[redacted url]",
		},
		{
			name: "empty input stays empty",
			raw:  "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactURL(tt.raw)

			assert.Equal(t, tt.want, got)
			assert.NotContains(t, got, "s3cret")
			assert.NotContains(t, got, "pass@")
		})
	}
}
