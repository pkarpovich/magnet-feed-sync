package providers

import (
	"fmt"
	"strings"
)

// ErrorKind classifies why a provider call failed.
type ErrorKind int

const (
	// KindTransient marks a failure that is expected to recover on its own.
	KindTransient ErrorKind = iota
	// KindBlocked marks a tracker refusing to serve us (challenge, rate limit, no solver).
	KindBlocked
	// KindPermanent marks a failure that retrying cannot fix.
	KindPermanent
)

func (k ErrorKind) String() string {
	switch k {
	case KindBlocked:
		return "Blocked"
	case KindPermanent:
		return "Permanent"
	default:
		return "Transient"
	}
}

// Key is the form stored in the database, so a renamed String never rewrites history.
func (k ErrorKind) Key() string {
	return strings.ToLower(k.String())
}

// ProviderError carries the classification alongside the underlying error.
type ProviderError struct {
	Kind ErrorKind
	Err  error
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("%s: %s", e.Kind, e.Err)
}

func (e *ProviderError) Unwrap() error {
	return e.Err
}
