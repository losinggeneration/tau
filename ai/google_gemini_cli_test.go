package ai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestStreamGeminiCLIResponseTerminalOutcomes(t *testing.T) {
	const text = `data: {"response":{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}}`
	finish := func(reason string) string {
		return `data: {"response":{"candidates":[{"finishReason":"` + reason + `"}]}}`
	}
	for _, tc := range []struct {
		name, message string
		lines         []string
		reason        StopReason
	}{
		{name: "stop", lines: []string{text, finish("STOP")}, reason: StopReasonStop},
		{name: "content-free safety finish", lines: []string{text, finish("SAFETY")}, reason: StopReasonError, message: "Provider stopped with: SAFETY"},
		{name: "no finish", lines: []string{text}, reason: StopReasonError, message: "google gemini cli stream ended without a finish reason"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{Body: io.NopCloser(strings.NewReader(strings.Join(tc.lines, "\n\n") + "\n\n"))}
			reason, message := streamTerminal(t, func(handler EventHandler) {
				if err := streamGeminiCLIResponse(context.Background(), &Model{ID: "gemini"}, response, handler, &options{}); err != nil {
					t.Fatal(err)
				}
			})
			if reason != tc.reason || message != tc.message {
				t.Fatalf("got %s %q, want %s %q", reason, message, tc.reason, tc.message)
			}
		})
	}
}

func TestGeminiCLIExplicitZeroRetries(t *testing.T) {
	for _, status := range []int{0, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			calls := 0
			failure := errors.New("network failure")
			previous := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if status == 0 {
					return nil, failure
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"unavailable"}}`))}, nil
			})}
			t.Cleanup(func() { http.DefaultClient = previous })
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			err := (&googleGeminiCLIClient{}).Stream(ctx, &Model{ID: "gemini", BaseURL: "https://test.invalid"}, &Context{}, func(context.Context, StreamEvent) {}, WithAPIKey(`{"token":"token","projectId":"project"}`), WithMaxRetries(0))
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("zero retries waited for another attempt: %v", err)
			}
			if status == 0 && !errors.Is(err, failure) {
				t.Fatalf("network cause lost: %v", err)
			}
			if calls != 1 {
				t.Fatalf("requests = %d, want 1", calls)
			}
		})
	}
}

func TestGeminiCLIRetryBudgetAndPermanentFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		retries int
		want    int
	}{
		{name: "one retry", status: http.StatusServiceUnavailable, retries: 1, want: 2},
		{name: "permanent failure", status: http.StatusBadRequest, retries: 3, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			previous := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"failed"}}`))}, nil
			})}
			t.Cleanup(func() { http.DefaultClient = previous })
			ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
			defer cancel()
			err := (&googleGeminiCLIClient{}).Stream(ctx, &Model{ID: "gemini", BaseURL: "https://test.invalid"}, &Context{}, func(context.Context, StreamEvent) {}, WithAPIKey(`{"token":"token","projectId":"project"}`), WithMaxRetries(tc.retries))
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("retry budget not honored: %v", err)
			}
			if calls != tc.want {
				t.Fatalf("requests = %d, want %d", calls, tc.want)
			}
		})
	}
}

func TestGeminiCLIRetryDelayLimit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    []Option
		limited bool
	}{
		{name: "default", limited: true},
		{name: "explicit", opts: []Option{WithMaxRetryDelayMs(1)}, limited: true},
		{name: "disabled", opts: []Option{WithMaxRetryDelayMs(0)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"120"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"busy"}}`))}, nil
			})}
			t.Cleanup(func() { http.DefaultClient = previous })
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			opts := append([]Option{WithAPIKey(`{"token":"token","projectId":"project"}`), WithMaxRetries(1)}, tc.opts...)
			err := (&googleGeminiCLIClient{}).Stream(ctx, &Model{ID: "gemini", BaseURL: "https://test.invalid"}, &Context{}, func(context.Context, StreamEvent) {}, opts...)
			if tc.limited {
				if err == nil || !strings.Contains(err.Error(), "retry delay") || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("missing delay limit: %v", err)
				}
			} else if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("unbounded wait did not honor cancellation: %v", err)
			}
		})
	}
}

func TestGeminiCLIHeaderTimeout(t *testing.T) {
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	t.Cleanup(func() { http.DefaultClient = previous })
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err := (&googleGeminiCLIClient{}).Stream(ctx, &Model{ID: "gemini", BaseURL: "https://test.invalid"}, &Context{}, func(context.Context, StreamEvent) {}, WithAPIKey(`{"token":"token","projectId":"project"}`), WithMaxRetries(0), WithTimeout(time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("header timeout was not applied before parent deadline: %v", err)
	}
}

type geminiCLITestBody struct {
	io.Reader
	ctx      context.Context
	delay    time.Duration
	closeErr error
	closed   bool
}

func (b *geminiCLITestBody) Read(p []byte) (int, error) {
	if b.delay > 0 {
		timer := time.NewTimer(b.delay)
		defer timer.Stop()
		b.delay = 0
		select {
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		case <-timer.C:
		}
	}
	return b.Reader.Read(p)
}

func (b *geminiCLITestBody) Close() error { b.closed = true; return b.closeErr }

func TestGeminiCLIHeaderTimeoutDoesNotLimitBody(t *testing.T) {
	for _, timeout := range []time.Duration{0, time.Millisecond} {
		t.Run(timeout.String(), func(t *testing.T) {
			var body *geminiCLITestBody
			previous := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if timeout == 0 {
					timer := time.NewTimer(3 * time.Millisecond)
					defer timer.Stop()
					select {
					case <-req.Context().Done():
						return nil, req.Context().Err()
					case <-timer.C:
					}
				}
				body = &geminiCLITestBody{Reader: strings.NewReader("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]},\"finishReason\":\"STOP\"}]}}\n\n"), ctx: req.Context(), delay: 10 * time.Millisecond}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
			})}
			t.Cleanup(func() { http.DefaultClient = previous })
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			message, err := (&googleGeminiCLIClient{}).Complete(ctx, &Model{ID: "gemini", BaseURL: "https://test.invalid"}, &Context{}, WithAPIKey(`{"token":"token","projectId":"project"}`), WithMaxRetries(0), WithTimeout(timeout))
			if err != nil {
				t.Fatal(err)
			}
			if message.StopReason != StopReasonStop {
				t.Fatalf("terminal outcome = %#v", message)
			}
			if body == nil || !body.closed {
				t.Fatal("successful body was not closed")
			}
			if body.ctx.Err() == nil {
				t.Fatal("successful request context was not released")
			}
		})
	}
}

func TestGeminiCLIErrorBodyCloseFailureIsNotRetried(t *testing.T) {
	closeErr := errors.New("close error body failed")
	body := &geminiCLITestBody{Reader: strings.NewReader("busy"), closeErr: closeErr}
	calls := 0
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		body.ctx = req.Context()
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: body}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = previous })
	err := (&googleGeminiCLIClient{}).Stream(t.Context(), &Model{ID: "gemini", BaseURL: "https://test.invalid"}, &Context{}, func(context.Context, StreamEvent) {}, WithAPIKey(`{"token":"token","projectId":"project"}`))
	if !errors.Is(err, closeErr) || calls != 1 || !body.closed || body.ctx.Err() == nil {
		t.Fatalf("cleanup failure lost or retried: calls=%d closed=%v error=%v", calls, body.closed, err)
	}
}
