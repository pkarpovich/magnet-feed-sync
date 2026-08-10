package providers

import (
	"context"
	"time"
)

type Provider interface {
	Parse(ctx context.Context, url string) (*Result, error)
	CanHandle(url string) bool
	Name() string
}

type Result struct {
	ID         string
	Title      string
	Magnet     string
	UpdatedAt  time.Time
	Comment    string
	TrackerURL string
}
