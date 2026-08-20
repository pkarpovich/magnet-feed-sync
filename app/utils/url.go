package utils

import (
	"net/url"
	"strings"
)

// what a source URL can carry as a credential: a jackett `/dl/` link and a torznab query both
// hold the api key in the query string, and a solver endpoint holds userinfo
var secretParams = []string{"apikey", "api_key", "passkey", "token", "secret", "password", "auth"}

// RedactURL masks the credentials a URL carries so it can be logged. A URL that holds none is
// returned verbatim — re-encoding a clean one would only churn its query order for the reader.
func RedactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "[redacted url]"
	}

	query := parsed.Query()
	redacted := parsed.User != nil
	for key := range query {
		if isSecretParam(key) {
			query.Set(key, "redacted")
			redacted = true
		}
	}

	if !redacted {
		return raw
	}

	if parsed.User != nil {
		parsed.User = url.User("redacted")
	}
	parsed.RawQuery = query.Encode()

	return parsed.String()
}

func isSecretParam(key string) bool {
	lower := strings.ToLower(key)
	for _, secret := range secretParams {
		if strings.Contains(lower, secret) {
			return true
		}
	}

	return false
}
