package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestStaleRunAfter(t *testing.T) {
	tests := []struct {
		name string
		cron string
		want time.Duration
	}{
		{name: "hourly", cron: "0 * * * *", want: 2 * time.Hour},
		{name: "every 15 minutes", cron: "*/15 * * * *", want: 30 * time.Minute},
		// the first gap is 1h but the schedule is silent for 23h, so a window built from the
		// first gap alone would report 503 for most of the day
		{name: "clustered", cron: "0 9,10 * * *", want: 46 * time.Hour},
		{name: "invalid", cron: "not a cron", want: staleRunFallback},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, staleRunAfter(tt.cron))
		})
	}
}

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "userinfo", in: "http://user:s3cret@solver:8191/v1", want: "http://redacted@solver:8191/v1"},
		{name: "api key", in: "http://jackett:9117/api?apikey=s3cret", want: "http://jackett:9117/api?apikey=redacted"},
		{name: "nothing to hide", in: "http://solver:8191/v1", want: "http://solver:8191/v1"},
		{name: "unparsable", in: "://nope", want: "<invalid url>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, redactURL(tt.in))
		})
	}
}
