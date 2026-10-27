package ai

import (
	"errors"
	"time"
)

// UsageLimitResetAt reports when a provider usage limit becomes available
// again, for errors that carry a provider-reported reset time.
// ok is false for any error without usable reset
// information.
//
// The agent loop waits for the reset and retries on these errors, where Pi
// ends the run. That's an intentional divergence, so keep it when porting
// upstream changes. Every transport of a provider with reset times has to
// return its typed error, otherwise the retry quietly stops working.
func UsageLimitResetAt(err error) (resetAt time.Time, ok bool) {
	var codexErr *CodexUsageLimitError
	var zaiErr *ZAIUsageLimitError
	var claudeErr *ClaudeAgentUsageLimitError

	switch {
	case errors.As(err, &codexErr):
		resetAt = codexErr.ResetsAt
		if resetAt.IsZero() {
			if codexErr.ResetsIn <= 0 {
				return time.Time{}, false
			}
			resetAt = time.Now().Add(codexErr.ResetsIn)
		}
	case errors.As(err, &claudeErr):
		resetAt = claudeErr.ResetsAt
	case errors.As(err, &zaiErr):
		resetAt = zaiErr.ResetsAt
	}

	return resetAt, !resetAt.IsZero()
}
