package ai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRetryAssistantCall(t *testing.T) {
	calls := 0
	produce := func(context.Context) (*AssistantMessage, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("servers are currently busy")
		}
		return &AssistantMessage{StopReason: StopReasonStop}, nil
	}
	result, err := RetryAssistantCall(context.Background(), produce, RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}, nil)
	if err != nil || result.StopReason != StopReasonStop || calls != 2 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
	}
	calls = 0
	result, err = RetryAssistantCall(context.Background(), func(context.Context) (*AssistantMessage, error) {
		calls++
		return &AssistantMessage{StopReason: StopReasonError, ErrorMessage: "429 insufficient_quota"}, nil
	}, RetryPolicy{MaxRetries: 2, BaseDelay: time.Millisecond}, nil)
	if err != nil || calls != 1 || result.StopReason != StopReasonError {
		t.Fatalf("quota retried: result=%+v err=%v calls=%d", result, err, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls = 0
	_, err = RetryAssistantCall(ctx, produce, RetryPolicy{MaxRetries: 1, BaseDelay: time.Hour}, func(int, time.Duration, string) { cancel() })
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("backoff ignored cancellation: %v, %d", err, calls)
	}
}

func TestRetryAssistantCallDoesNotRetryZAIUsageLimits(t *testing.T) {
	for _, message := range []string{
		`zai: status 429: {"error":{"code":"1310","message":"Weekly/Monthly Limit Exhausted"}}`,
		"zai: status 429: Weekly/Monthly Limit Exhausted. Available again in 120h0m0s at 2099-10-25 00:43:16",
		"zai: status 429: Usage limit reached for 5 hour. Available again in 5h0m0s at 2099-10-25 00:43:16",
	} {
		t.Run(message, func(t *testing.T) {
			providerErr := errors.New(message)
			calls := 0
			_, err := RetryAssistantCall(t.Context(), func(context.Context) (*AssistantMessage, error) {
				calls++
				return nil, providerErr
			}, RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond}, nil)
			if err != providerErr || calls != 1 {
				t.Fatalf("quota retried: err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestRetryAssistantCallZeroDelayCap(t *testing.T) {
	var calls int
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := RetryAssistantCall(ctx, func(context.Context) (*AssistantMessage, error) {
		calls++
		return &AssistantMessage{StopReason: StopReasonError, ErrorMessage: "server busy 503"}, nil
	}, RetryPolicy{MaxRetries: 3, BaseDelay: time.Hour, MaxDelay: 0}, func(_ int, delay time.Duration, _ string) {
		if delay != 0 {
			t.Errorf("delay=%v, want zero", delay)
			cancel()
		}
	})
	if err != nil || calls != 4 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestRetryAfterDelay(t *testing.T) {
	for _, tc := range []struct {
		name, ms, seconds string
		want              time.Duration
		ok                bool
	}{
		{name: "absent"},
		{name: "milliseconds override", ms: "1.5", seconds: "2", want: 1500 * time.Microsecond, ok: true},
		{name: "seconds", seconds: "0.25", want: 250 * time.Millisecond, ok: true},
		{name: "zero", ms: "0", want: 0, ok: true},
		{name: "negative", seconds: "-1", want: 0, ok: true},
		{name: "invalid milliseconds", ms: "NaN", seconds: "2", want: 2 * time.Second, ok: true},
		{name: "infinite", ms: "Inf", seconds: "invalid"},
		{name: "overflow", seconds: "1e300", want: time.Duration(1<<63 - 1), ok: true},
		{name: "past date", seconds: time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), want: 0, ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delay, ok := retryAfterDelay(http.Header{"Retry-After-Ms": []string{tc.ms}, "Retry-After": []string{tc.seconds}})
			if delay != tc.want || ok != tc.ok {
				t.Fatalf("delay=%s ok=%v want %s %v", delay, ok, tc.want, tc.ok)
			}
		})
	}
	delay, ok := retryAfterDelay(http.Header{"Retry-After": []string{time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)}})
	if !ok || delay < 59*time.Minute || delay > time.Hour {
		t.Fatalf("future date delay=%s ok=%v", delay, ok)
	}
}

func TestSendProviderRequestFollowsUpstreamPolicy(t *testing.T) {
	failure, readErr, closeErr := errors.New("connection reset"), errors.New("read failed"), errors.New("close failed")
	response := func(status int, header http.Header, body *codexTestBody) *http.Response {
		if header == nil {
			header = make(http.Header)
		}
		if body == nil {
			body = &codexTestBody{Reader: strings.NewReader("rejected")}
		}
		return &http.Response{StatusCode: status, Header: header, Body: body}
	}
	instant := http.Header{"Retry-After-Ms": {"0"}}
	for _, tc := range []struct {
		name      string
		opts      []Option
		first     func() (*http.Response, error)
		retryable func(int) bool
		permanent func(*providerStatusError) error
		calls     int
		want      string
		wantErr   error
	}{
		{name: "default makes one attempt", first: func() (*http.Response, error) { return response(503, instant, nil), nil }, calls: 1, want: "test: status 503: rejected"},
		{name: "server error", opts: []Option{WithMaxRetries(1)}, first: func() (*http.Response, error) { return response(503, instant, nil), nil }, calls: 2},
		{name: "rate limit", opts: []Option{WithMaxRetries(1)}, first: func() (*http.Response, error) { return response(429, instant, nil), nil }, calls: 2},
		{name: "network failure", opts: []Option{WithMaxRetries(1)}, first: func() (*http.Response, error) { return nil, failure }, calls: 2},
		{name: "bad request", opts: []Option{WithMaxRetries(1)}, first: func() (*http.Response, error) { return response(400, instant, nil), nil }, calls: 1, want: "status 400"},
		{name: "server forbids retry", opts: []Option{WithMaxRetries(1)}, first: func() (*http.Response, error) {
			return response(503, http.Header{"X-Should-Retry": {"false"}}, nil), nil
		}, calls: 1, want: "status 503"},
		{name: "server requests retry", opts: []Option{WithMaxRetries(1)}, first: func() (*http.Response, error) {
			return response(400, http.Header{"X-Should-Retry": {"true"}, "Retry-After-Ms": {"0"}}, nil), nil
		}, calls: 2},
		{name: "server delay over limit", opts: []Option{WithMaxRetries(1)}, first: func() (*http.Response, error) {
			return response(503, http.Header{"Retry-After": {"61"}}, nil), nil
		}, calls: 1, want: "retry delay 1m1s exceeds limit"},
		{name: "zero limit allows server delay", opts: []Option{WithMaxRetries(1), WithMaxRetryDelayMs(0)}, first: func() (*http.Response, error) {
			return response(503, http.Header{"Retry-After-Ms": {"1"}}, nil), nil
		}, calls: 2},
		{name: "body failure stops retries", opts: []Option{WithMaxRetries(1)}, first: func() (*http.Response, error) {
			return response(503, instant, &codexTestBody{readErr: readErr, closeErr: closeErr}), nil
		}, calls: 1, wantErr: closeErr},
		{name: "narrowed policy", opts: []Option{WithMaxRetries(1)}, retryable: func(status int) bool { return status >= 500 }, first: func() (*http.Response, error) {
			return response(429, instant, nil), nil
		}, calls: 1, want: "status 429"},
		{name: "permanent rejection", opts: []Option{WithMaxRetries(1)}, permanent: func(*providerStatusError) error { return failure }, first: func() (*http.Response, error) {
			return response(503, instant, nil), nil
		}, calls: 1, wantErr: failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var final *codexTestBody
			resp, err := sendProviderRequest(t.Context(), applyOptions(tc.opts), providerRequest{
				label: "test", retryable: tc.retryable, permanent: tc.permanent, backoff: func(int) time.Duration { return 0 },
				send: func() (*http.Response, error) {
					calls++
					if calls == 1 {
						return tc.first()
					}
					final = &codexTestBody{Reader: strings.NewReader("ok")}
					return response(http.StatusOK, nil, final), nil
				},
			})
			require.Equal(t, tc.calls, calls)
			if tc.want == "" && tc.wantErr == nil {
				require.NoError(t, err)
				require.Same(t, final, resp.Body)
				return
			}
			require.Nil(t, resp)
			require.ErrorContains(t, err, tc.want)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			}
		})
	}
}

func TestSendProviderRequestCancelsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	_, err := sendProviderRequest(ctx, applyOptions([]Option{WithMaxRetries(3)}), providerRequest{label: "test", backoff: func(int) time.Duration { return time.Hour }, send: func() (*http.Response, error) {
		calls++
		cancel()
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("busy"))}, nil
	}})
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorContains(t, err, "status 503: busy")
	require.Equal(t, 1, calls)
}

func TestRetryAssistantCallCancellationBoundaries(t *testing.T) {
	for _, phase := range []string{"entry", "success", "failure", "permanent failure", "retryable message", "retry callback", "backoff"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				cause := errors.New("retry owner cancelled")
				providerErr := errors.New("server error 503")
				var calls, retries int
				if phase == "entry" {
					cancel(cause)
				}
				var backoffDone chan struct{}
				if phase == "backoff" {
					backoffDone = make(chan struct{})
					defer func() { <-backoffDone }()
				}
				_, err := RetryAssistantCall(ctx, func(context.Context) (*AssistantMessage, error) {
					calls++
					switch phase {
					case "success":
						cancel(cause)
						return &AssistantMessage{StopReason: StopReasonStop}, nil
					case "failure", "permanent failure":
						cancel(cause)
						if phase == "permanent failure" {
							providerErr = errors.New("invalid request")
						}
						return nil, providerErr
					case "retryable message":
						cancel(cause)
						return &AssistantMessage{StopReason: StopReasonError, ErrorMessage: providerErr.Error()}, nil
					default:
						return nil, providerErr
					}
				}, RetryPolicy{MaxRetries: 2, BaseDelay: time.Hour, MaxDelay: time.Hour}, func(int, time.Duration, string) {
					retries++
					if phase == "backoff" {
						go func() {
							defer close(backoffDone)
							synctest.Wait()
							cancel(cause)
						}()
					} else {
						cancel(cause)
					}
				})
				require.ErrorIs(t, err, context.Canceled)
				require.ErrorIs(t, err, cause)
				if phase == "failure" || phase == "permanent failure" || phase == "retry callback" || phase == "backoff" {
					require.ErrorIs(t, err, providerErr)
				}
				if phase == "entry" {
					require.Zero(t, calls)
				} else {
					require.Equal(t, 1, calls)
				}
				if phase == "retry callback" || phase == "backoff" {
					require.Equal(t, 1, retries)
				} else {
					require.Zero(t, retries)
				}
			})
		})
	}
}

func TestRetryAssistantCallConnectionFailures(t *testing.T) {
	reset := "openai: read stream: read SSE stream: read tcp 192.168.1.101:42730->47.251.169.173:443: read: connection reset by peer"
	down := []string{
		`openai: request: Post "https://api.openai.com/v1/responses": dial tcp 1.2.3.4:443: i/o timeout`,
		"dial tcp 1.2.3.4:443: connect: network is unreachable",
		"dial tcp 1.2.3.4:443: connect: no route to host",
		"dial tcp 1.2.3.4:443: connect: connection refused",
		"dial tcp: lookup api.openai.com: Temporary failure in name resolution",
		"dial tcp: lookup api.openai.com on 192.168.1.1:53: server misbehaving",
		"net/http: TLS handshake timeout",
	}
	run := func(failures []string) (int, []time.Duration) {
		var (
			calls  int
			delays []time.Duration
		)
		_, err := RetryAssistantCall(t.Context(), func(context.Context) (*AssistantMessage, error) {
			calls++
			if calls > len(failures) {
				return &AssistantMessage{StopReason: StopReasonStop}, nil
			}
			return nil, errors.New(failures[calls-1])
		}, RetryPolicy{MaxRetries: 3, BaseDelay: time.Nanosecond, MaxDelay: 4 * time.Nanosecond}, func(_ int, delay time.Duration, _ string) {
			delays = append(delays, delay)
		})
		require.NoError(t, err)
		return calls, delays
	}

	calls, _ := run([]string{reset, reset})
	require.Equal(t, 3, calls, "reset of an open stream retries")

	_, err := RetryAssistantCall(t.Context(), func(context.Context) (*AssistantMessage, error) {
		return nil, errors.New(reset)
	}, RetryPolicy{MaxRetries: 2, BaseDelay: time.Nanosecond}, nil)
	require.ErrorContains(t, err, "connection reset", "reset retries stay capped")

	for _, message := range down {
		failures := []string{reset, reset, reset}
		for range 20 {
			failures = append(failures, message)
		}
		calls, delays := run(failures)
		require.Equal(t, len(failures)+1, calls, "network down retries past MaxRetries: %s", message)
		require.Equal(t, 4*time.Nanosecond, delays[len(delays)-1], "backoff stays at MaxDelay")
	}

	var calls0 int
	_, err = RetryAssistantCall(t.Context(), func(context.Context) (*AssistantMessage, error) {
		calls0++
		return nil, errors.New(down[0])
	}, RetryPolicy{MaxRetries: 0}, nil)
	require.Error(t, err)
	require.Equal(t, 1, calls0, "disabled retries stay disabled")
}
