package ai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// RetryPolicy bounds transient assistant-call retries.
type RetryPolicy struct {
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration // Zero disables the backoff delay.
}

var (
	nonRetryableProviderError = regexp.MustCompile(`(?i)GoUsageLimitError|FreeUsageLimitError|usage limit reached|limit exhausted|available balance|insufficient_quota|out of budget|quota exceeded|billing|subscription_sharing_usage_limit_exceeded|context.{0,20}(window|length|limit)|too many tokens`)
	retryableProviderError    = regexp.MustCompile(`(?i)overloaded|server_busy|servers are currently busy|currently experiencing high demand|model is at capacity|rate.?limit|too many requests|429|500|502|503|504|520|524|service.?unavailable|server.?error|internal.?error|provider.?returned.?(an.?)?error|exceeded request buffer limit|network.?error|connection.?(error|refused|lost)|other side closed|fetch failed|getaddrinfo|ENOTFOUND|EAI_AGAIN|upstream.?connect|reset before headers|socket hang up|socket connection was closed|timed? out|timeout|terminated|websocket.?(closed|error)|ended without|stream ended before|http2 request did not get a response|pending stream has been canceled|retry delay|retry your request|try your request again|ResourceExhausted|subscription_sharing_(usage|user)_unavailable`)
)

// IsRetryableAssistantError excludes quota, billing, context overflow and aborted outcomes.
func IsRetryableAssistantError(message *AssistantMessage) bool {
	return message != nil && message.StopReason == StopReasonError && !nonRetryableProviderError.MatchString(message.ErrorMessage) && retryableProviderError.MatchString(message.ErrorMessage)
}

// RetryAssistantCall retries transient failures with cancellable exponential backoff.
func RetryAssistantCall(ctx context.Context, produce func(context.Context) (*AssistantMessage, error), policy RetryPolicy, onRetry func(int, time.Duration, string)) (message *AssistantMessage, err error) {
	defer func() {
		if ctx.Err() != nil {
			err = fmt.Errorf("assistant retry: %w", errors.Join(err, ctx.Err(), context.Cause(ctx)))
		}
	}()
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return nil, err
		}
		message, err = produce(ctx)
		if ctx.Err() != nil {
			return message, err
		}
		failure := message
		if err != nil {
			failure = &AssistantMessage{StopReason: StopReasonError, ErrorMessage: err.Error()}
		}
		if attempt >= policy.MaxRetries || !IsRetryableAssistantError(failure) {
			return message, err
		}
		delay := max(time.Duration(0), min(policy.BaseDelay, policy.MaxDelay))
		for range attempt {
			if delay >= policy.MaxDelay-delay {
				delay = max(time.Duration(0), policy.MaxDelay)
				break
			}
			delay *= 2
		}
		if onRetry != nil {
			onRetry(attempt+1, delay, failure.ErrorMessage)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, err
		case <-timer.C:
		}
	}
}

func retryAfterDelay(headers http.Header) (time.Duration, bool) {
	for _, h := range []struct {
		name string
		unit time.Duration
	}{
		{"retry-after-ms", time.Millisecond}, {"retry-after", time.Second},
	} {
		raw := strings.TrimSpace(headers.Get(h.name))
		if raw == "" {
			continue
		}
		if value, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) {
			nanos := max(0, value) * float64(h.unit)
			if nanos >= float64(math.MaxInt64) {
				return time.Duration(math.MaxInt64), true
			}
			return time.Duration(nanos), true
		}
		if h.name == "retry-after" {
			if date, err := http.ParseTime(raw); err == nil {
				return max(0, time.Until(date)), true
			}
		}
	}
	return 0, false
}

var providerRetryBase = 500 * time.Millisecond

// providerBackoff matches upstream's provider backoff without jitter.
func providerBackoff(attempt int) time.Duration {
	return time.Duration(1<<min(attempt, 4)) * providerRetryBase
}

// waitProviderRetry applies upstream's provider retry policy to a failed
// attempt and sleeps before a retry. Status 0 marks a failure without a response.
func waitProviderRetry(ctx context.Context, label string, cfg *options, attempt, retries, status int, headers http.Header, backoff time.Duration, cause error) (bool, error) {
	if ctx.Err() != nil {
		return false, nil
	}
	retry := status == 0 || status == http.StatusRequestTimeout || status == http.StatusConflict || status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
	switch headers.Get("x-should-retry") {
	case "true":
		retry = true
	case "false":
		retry = false
	}
	if !retry || attempt >= retries {
		return false, nil
	}
	if serverDelay, ok := retryAfterDelay(headers); ok {
		maxDelay := cfg.maxRetryDelayMs
		if !cfg.maxRetryDelaySet {
			maxDelay = 60000
		}
		if maxDelay > 0 && float64(serverDelay)/float64(time.Millisecond) > float64(maxDelay) {
			return false, fmt.Errorf("%s retry delay %s exceeds limit: %w", label, serverDelay, cause)
		}
		backoff = serverDelay
	}
	if err := sleepWithContext(ctx, backoff); err != nil {
		return false, fmt.Errorf("%s retry: %w", label, errors.Join(cause, err))
	}
	return true, nil
}

// providerStatusError is a rejected provider response.
type providerStatusError struct {
	label  string
	status int
	body   []byte
}

func (e *providerStatusError) Error() string {
	return fmt.Sprintf("%s: status %d: %s", e.label, e.status, e.body)
}

// providerRequest describes one retried streaming request.
type providerRequest struct {
	label   string
	retries int                     // used when the options leave retries unset
	backoff func(int) time.Duration // nil uses providerBackoff
	// permanent turns a rejected response into an error that is never retried.
	permanent func(*providerStatusError) error
	// retryable narrows the provider policy; status 0 is a failure without a response.
	retryable func(status int) bool
	send      func() (*http.Response, error)
}

// sendProviderRequest sends a request with provider retries and returns the
// first 200 response. Rejected bodies that fail to read or close end the retries.
func sendProviderRequest(ctx context.Context, cfg *options, request providerRequest) (*http.Response, error) {
	retries := request.retries
	if cfg.maxRetries != nil {
		retries = *cfg.maxRetries
	}
	backoff := request.backoff
	if backoff == nil {
		backoff = providerBackoff
	}
	for attempt := 0; ; attempt++ {
		var (
			status  int
			headers http.Header
		)
		resp, err := request.send()
		if err != nil {
			err = fmt.Errorf("%s: request: %w", request.label, err)
		} else {
			if resp.StatusCode == http.StatusOK {
				return resp, nil
			}
			status, headers = resp.StatusCode, resp.Header
			body, readErr := io.ReadAll(resp.Body)
			if closeErr := resp.Body.Close(); readErr != nil || closeErr != nil {
				if readErr != nil {
					readErr = fmt.Errorf("read error response body: %w", readErr)
				}
				if closeErr != nil {
					closeErr = fmt.Errorf("close error response body: %w", closeErr)
				}
				return nil, fmt.Errorf("%s: status %d: %w", request.label, status, errors.Join(readErr, closeErr))
			}
			rejected := &providerStatusError{label: request.label, status: status, body: body}
			if request.permanent != nil {
				if err := request.permanent(rejected); err != nil {
					return nil, err
				}
			}
			err = rejected
		}
		retry := request.retryable == nil || request.retryable(status)
		if retry {
			var waitErr error
			if retry, waitErr = waitProviderRetry(ctx, request.label, cfg, attempt, retries, status, headers, backoff(attempt), err); waitErr != nil {
				return nil, waitErr
			}
		}
		if !retry {
			if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
				err = errors.Join(err, ctxErr)
			}
			return nil, err
		}
	}
}
