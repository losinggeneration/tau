package ai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// withFastChatCompletionsRetries shrinks the backoff base so retry tests run
// in milliseconds and restores it afterwards.
func withFastChatCompletionsRetries(t *testing.T) {
	t.Helper()

	origBase, origMax := chatCompletionsRetryBase, chatCompletionsMaxRetries
	chatCompletionsRetryBase = time.Millisecond
	chatCompletionsMaxRetries = 3
	t.Cleanup(func() {
		chatCompletionsRetryBase = origBase
		chatCompletionsMaxRetries = origMax
	})
}

func TestDoOpenAIChatCompletionsRetries5xx(t *testing.T) {
	withFastChatCompletionsRetries(t)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != chatCompletionsPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if requests.Add(1) <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	model := &Model{ID: "glm-5", Provider: ProviderZAI}
	resp, err := doOpenAIChatCompletions(context.Background(), model, &options{}, NewTransport(server.URL, "", nil), "{}")
	if err != nil {
		t.Fatalf("doOpenAIChatCompletions: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
}

func TestDoOpenAIChatCompletionsGivesUpAfterMaxRetries(t *testing.T) {
	withFastChatCompletionsRetries(t)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)

	model := &Model{ID: "glm-5", Provider: ProviderZAI}
	_, err := doOpenAIChatCompletions(context.Background(), model, &options{}, NewTransport(server.URL, "", nil), "{}")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := requests.Load(); got != int32(chatCompletionsMaxRetries)+1 {
		t.Fatalf("requests = %d, want %d", got, chatCompletionsMaxRetries+1)
	}
	if want := "zai: status 502"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want prefix %q", err.Error(), want)
	}
}

func TestDoOpenAIChatCompletionsDoesNotRetryNon5xx(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	model := &Model{ID: "glm-5", Provider: ProviderZAI}
	_, err := doOpenAIChatCompletions(context.Background(), model, &options{}, NewTransport(server.URL, "", nil), "{}")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestChatCompletionsRetryScheduleAboutTwoMinutes(t *testing.T) {
	var total time.Duration
	for attempt := 0; attempt < chatCompletionsMaxRetries; attempt++ {
		total += chatCompletionsRetryDelay(attempt)
	}

	if total < 110*time.Second || total > 130*time.Second {
		t.Fatalf("total retry window = %v, want ~2 minutes", total)
	}
}

func TestDoOpenAIChatCompletionsReplaysRequestBody(t *testing.T) {
	withFastChatCompletionsRetries(t)

	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)

	model := &Model{ID: "glm-5", Provider: ProviderZAI}
	_, _ = doOpenAIChatCompletions(context.Background(), model, &options{}, NewTransport(server.URL, "", nil), `{"model":"glm-5"}`)

	for i, body := range bodies {
		if body != `{"model":"glm-5"}` {
			t.Fatalf("attempt %d body = %q, want replayed request body", i, body)
		}
	}
	if len(bodies) != chatCompletionsMaxRetries+1 {
		t.Fatalf("attempts = %d, want %d", len(bodies), chatCompletionsMaxRetries+1)
	}
}
