package ai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// zaiUsageLimitCode is the error.code z.ai returns with HTTP 429 when a plan's
// usage window is exhausted, as opposed to ordinary per-minute rate limiting.
const zaiUsageLimitCode = "1308"

// zaiResetLocation is the timezone z.ai stamps reset times in. The message
// carries a bare "2026-08-19 08:18:14" in Beijing time with no zone marker, so
// the offset is fixed here rather than loaded from the system tzdata, which
// minimal containers often ship without.
var zaiResetLocation = time.FixedZone("CST", 8*60*60)

var (
	// zaiResetTimePattern matches the reset timestamp in a usage limit message.
	zaiResetTimePattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}`)
	// zaiResetLeadInPattern matches the phrase introducing that timestamp, e.g.
	// the "Your limit will reset at " in "Your limit will reset at <time>". It
	// is stripped so the error can restate the reset in local time instead of
	// carrying both.
	zaiResetLeadInPattern = regexp.MustCompile(`(?i)(?:\byour\b\s+)?(?:\blimit\b\s+)?(?:\bwill\b\s+)?\bresets?\b(?:\s+\bat\b)?[\s,;:.]*$`)
)

// ZAIUsageLimitError is returned when z.ai reports that a plan's usage limit
// has been reached (HTTP 429 with error code 1308) and the reset time in the
// message could be parsed. It restates that reset in local time so callers do
// not have to reason about Beijing time.
type ZAIUsageLimitError struct {
	// Provider is the provider name the request was made under.
	Provider string
	// StatusCode is the HTTP status code of the response (typically 429).
	StatusCode int
	// Message is the provider-supplied message with the reset phrase removed.
	Message string
	// ResetsAt is when the usage limit resets, in local time.
	ResetsAt time.Time
	// ResetsIn is how long until the usage limit resets.
	ResetsIn time.Duration
}

// Error implements error.
func (e *ZAIUsageLimitError) Error() string {
	provider := e.Provider
	if provider == "" {
		provider = ProviderZAI
	}

	message := e.Message
	if message == "" {
		message = "Usage limit reached"
	}

	return fmt.Sprintf("%s: status %d: %s. Available again in %s at %s",
		provider, e.StatusCode, message, e.ResetsIn, e.ResetsAt.Format(time.DateTime))
}

// zaiErrorResponse mirrors the error envelope returned by the z.ai API. Code is
// documented as a string but decoded loosely so a numeric code still matches.
type zaiErrorResponse struct {
	Error struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// parseZAIUsageLimitError returns a *ZAIUsageLimitError when body is a z.ai
// usage limit response whose reset time could be parsed, and nil otherwise.
// Returning nil leaves the caller free to report the raw response body.
func parseZAIUsageLimitError(provider string, statusCode int, body []byte) *ZAIUsageLimitError {
	if statusCode != http.StatusTooManyRequests {
		return nil
	}

	var parsed zaiErrorResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	if zaiErrorCode(parsed.Error.Code) != zaiUsageLimitCode {
		return nil
	}

	match := zaiResetTimePattern.FindStringIndex(parsed.Error.Message)
	if match == nil {
		return nil
	}

	resetsAt, err := time.ParseInLocation(time.DateTime, parsed.Error.Message[match[0]:match[1]], zaiResetLocation)
	if err != nil {
		return nil
	}

	resetsIn := max(time.Until(resetsAt).Round(time.Second), 0)

	return &ZAIUsageLimitError{
		Provider:   provider,
		StatusCode: statusCode,
		Message:    trimZAIResetPhrase(parsed.Error.Message[:match[0]]),
		ResetsAt:   resetsAt.In(time.Local),
		ResetsIn:   resetsIn,
	}
}

// zaiErrorCode normalizes a decoded error code to its string form.
func zaiErrorCode(code any) string {
	switch c := code.(type) {
	case string:
		return c
	case float64:
		return strconv.FormatFloat(c, 'f', -1, 64)
	default:
		return ""
	}
}

// trimZAIResetPhrase returns head — the part of a usage limit message before
// its reset timestamp — without the phrase that introduced the timestamp or any
// punctuation left dangling by the cut.
func trimZAIResetPhrase(head string) string {
	head = zaiResetLeadInPattern.ReplaceAllString(head, "")
	return strings.Trim(strings.TrimSpace(head), " .,;:")
}
