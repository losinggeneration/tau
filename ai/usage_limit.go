package ai

import (
	"errors"
	"time"
)

// UsageLimitResetAt reports when a provider usage limit becomes available
// again, for errors that carry a provider-reported reset time (HTTP 429
// usage-limit responses). ok is false for any error without usable reset
// information.
func UsageLimitResetAt(err error) (resetAt time.Time, ok bool) {
	var codexErr *CodexUsageLimitError
	var zaiErr *ZAIUsageLimitError

	switch {
	case errors.As(err, &codexErr):
		resetAt = codexErr.ResetsAt
		if resetAt.IsZero() {
			if codexErr.ResetsIn <= 0 {
				return time.Time{}, false
			}
			resetAt = time.Now().Add(codexErr.ResetsIn)
		}
	case errors.As(err, &zaiErr):
		resetAt = zaiErr.ResetsAt
	}

	return resetAt, !resetAt.IsZero()
}
