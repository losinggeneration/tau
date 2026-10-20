package ai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseZAIUsageLimitError(t *testing.T) {
	resetsAt := time.Now().In(zaiResetLocation).Add(90 * time.Minute).Truncate(time.Second)
	body := fmt.Appendf(nil,
		`{"error":{"code":"1308","message":"Usage limit reached for 5 hour. Your limit will reset at %s"}}`,
		resetsAt.Format(time.DateTime))

	usageErr := parseZAIUsageLimitError(ProviderZAI, http.StatusTooManyRequests, body)
	if usageErr == nil {
		t.Fatal("parseZAIUsageLimitError = nil, want error")
	}
	if usageErr.Provider != ProviderZAI {
		t.Fatalf("Provider = %q, want %q", usageErr.Provider, ProviderZAI)
	}
	if usageErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("StatusCode = %d, want %d", usageErr.StatusCode, http.StatusTooManyRequests)
	}
	if want := "Usage limit reached for 5 hour"; usageErr.Message != want {
		t.Fatalf("Message = %q, want %q", usageErr.Message, want)
	}
	if !usageErr.ResetsAt.Equal(resetsAt) {
		t.Fatalf("ResetsAt = %v, want %v", usageErr.ResetsAt, resetsAt)
	}
	// The clock advances between the fixture and the parse, so allow a second.
	if diff := 90*time.Minute - usageErr.ResetsIn; diff < 0 || diff > time.Second {
		t.Fatalf("ResetsIn = %v, want ~%v", usageErr.ResetsIn, 90*time.Minute)
	}

	wantMessage := fmt.Sprintf("zai: status 429: Usage limit reached for 5 hour. Available again in %s at %s",
		usageErr.ResetsIn, resetsAt.In(time.Local).Format(time.DateTime))
	if usageErr.Error() != wantMessage {
		t.Fatalf("Error() = %q, want %q", usageErr.Error(), wantMessage)
	}
}

func TestParseZAIUsageLimitErrorReadsBeijingTime(t *testing.T) {
	// A fixed timestamp pins the timezone assumption: z.ai stamps the reset in
	// Beijing time (UTC+8), so 08:18:14 there is 00:18:14 UTC.
	body := []byte(`{"error":{"code":"1308","message":"Usage limit reached for 5 hour. Your limit will reset at 2026-08-19 08:18:14"}}`)

	usageErr := parseZAIUsageLimitError(ProviderZAI, http.StatusTooManyRequests, body)
	if usageErr == nil {
		t.Fatal("parseZAIUsageLimitError = nil, want error")
	}
	if want := time.Date(2026, 8, 19, 0, 18, 14, 0, time.UTC); !usageErr.ResetsAt.Equal(want) {
		t.Fatalf("ResetsAt = %v, want %v", usageErr.ResetsAt.UTC(), want)
	}
	if _, offset := usageErr.ResetsAt.Zone(); usageErr.ResetsAt.Location() != time.Local {
		t.Fatalf("ResetsAt location = %v (offset %d), want local", usageErr.ResetsAt.Location(), offset)
	}
}

func TestParseZAIUsageLimitErrorClampsElapsedReset(t *testing.T) {
	body := []byte(`{"error":{"code":"1308","message":"Usage limit reached for 5 hour. Your limit will reset at 2020-01-02 03:04:05"}}`)

	usageErr := parseZAIUsageLimitError(ProviderZAI, http.StatusTooManyRequests, body)
	if usageErr == nil {
		t.Fatal("parseZAIUsageLimitError = nil, want error")
	}
	if usageErr.ResetsIn != 0 {
		t.Fatalf("ResetsIn = %v, want 0", usageErr.ResetsIn)
	}
}

func TestParseZAIUsageLimitErrorAcceptsNumericCode(t *testing.T) {
	body := []byte(`{"error":{"code":1308,"message":"Usage limit reached, resets at 2099-01-02 03:04:05"}}`)

	usageErr := parseZAIUsageLimitError(ProviderZAI, http.StatusTooManyRequests, body)
	if usageErr == nil {
		t.Fatal("parseZAIUsageLimitError = nil, want error")
	}
	if want := "Usage limit reached"; usageErr.Message != want {
		t.Fatalf("Message = %q, want %q", usageErr.Message, want)
	}
}

func TestParseZAIUsageLimitErrorIgnoresOtherErrors(t *testing.T) {
	cases := map[string][]byte{
		"invalid json":     []byte(`rate limit exceeded`),
		"other code":       []byte(`{"error":{"code":"1113","message":"Insufficient balance"}}`),
		"missing code":     []byte(`{"error":{"message":"Usage limit reached. Your limit will reset at 2026-08-19 08:18:14"}}`),
		"unparsable time":  []byte(`{"error":{"code":"1308","message":"Usage limit reached for 5 hour. Your limit will reset soon"}}`),
		"malformed time":   []byte(`{"error":{"code":"1308","message":"Usage limit reached. Your limit will reset at 2026-08-19T08:18:14Z"}}`),
		"impossible time":  []byte(`{"error":{"code":"1308","message":"Usage limit reached. Your limit will reset at 2026-13-45 08:18:14"}}`),
		"empty payload":    []byte(`{}`),
		"wrong error type": []byte(`{"code":"1308","message":"Usage limit reached at 2026-08-19 08:18:14"}`),
	}

	for name, body := range cases {
		if usageErr := parseZAIUsageLimitError(ProviderZAI, http.StatusTooManyRequests, body); usageErr != nil {
			t.Fatalf("%s: parseZAIUsageLimitError = %v, want nil", name, usageErr)
		}
	}
}

func TestParseZAIUsageLimitErrorIgnoresNon429(t *testing.T) {
	body := []byte(`{"error":{"code":"1308","message":"Usage limit reached for 5 hour. Your limit will reset at 2099-08-19 08:18:14"}}`)

	if usageErr := parseZAIUsageLimitError(ProviderZAI, http.StatusInternalServerError, body); usageErr != nil {
		t.Fatalf("parseZAIUsageLimitError = %v, want nil", usageErr)
	}
}

func TestDoOpenAIChatCompletionsReturnsZAIUsageLimitError(t *testing.T) {
	// The fixture's reset is a Beijing timestamp, so the local date it lands on
	// depends on the machine's timezone. Derive it rather than hardcoding it.
	resetsAt := time.Date(2099, 8, 19, 8, 18, 14, 0, zaiResetLocation).In(time.Local)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"1308","message":"Usage limit reached for 5 hour. Your limit will reset at 2099-08-19 08:18:14"}}`)
	}))
	defer server.Close()

	model := &Model{ID: "glm-5.2", Provider: ProviderZAI, BaseURL: server.URL}
	transport, err := newOpenAIChatCompletionsTransport(model)
	if err != nil {
		t.Fatalf("newOpenAIChatCompletionsTransport error = %v", err)
	}

	_, err = doOpenAIChatCompletions(context.Background(), model, &options{}, transport, `{"model":"glm-5.2"}`)
	if err == nil {
		t.Fatal("doOpenAIChatCompletions error = nil, want usage limit error")
	}

	var usageErr *ZAIUsageLimitError
	if !errors.As(err, &usageErr) {
		t.Fatalf("error type = %T, want *ZAIUsageLimitError (%v)", err, err)
	}
	if !strings.HasPrefix(err.Error(), "zai: status 429: Usage limit reached for 5 hour. Available again in ") {
		t.Fatalf("error = %q, want formatted usage limit message", err)
	}
	if want := " at " + resetsAt.Format(time.DateTime); !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want reset time %q", err, want)
	}
}

func TestDoOpenAIChatCompletionsKeepsUnparsableZAIBody(t *testing.T) {
	body := `{"error":{"code":"1308","message":"Usage limit reached for 5 hour"}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()

	model := &Model{ID: "glm-5.2", Provider: ProviderZAI, BaseURL: server.URL}
	transport, err := newOpenAIChatCompletionsTransport(model)
	if err != nil {
		t.Fatalf("newOpenAIChatCompletionsTransport error = %v", err)
	}

	_, err = doOpenAIChatCompletions(context.Background(), model, &options{}, transport, `{"model":"glm-5.2"}`)
	if err == nil {
		t.Fatal("doOpenAIChatCompletions error = nil, want status error")
	}
	if want := "zai: status 429: " + body; err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

func TestParseZAIWeeklyMonthlyLimitError(t *testing.T) {
	for _, code := range []string{`"1310"`, `1310`} {
		body := fmt.Appendf(nil, `{"error":{"code":%s,"message":"Weekly/Monthly Limit Exhausted. Your limit will reset at 2026-10-25 08:43:16"}}`, code)
		usageErr := parseZAIUsageLimitError(ProviderZAI, http.StatusTooManyRequests, body)
		if usageErr == nil {
			t.Fatalf("parseZAIUsageLimitError(%s) = nil", code)
		}
		if usageErr.Message != "Weekly/Monthly Limit Exhausted" {
			t.Fatalf("Message = %q", usageErr.Message)
		}
		wantReset := time.Date(2026, 10, 25, 0, 43, 16, 0, time.UTC)
		resetAt, ok := UsageLimitResetAt(usageErr)
		if !ok || !resetAt.Equal(wantReset) {
			t.Fatalf("UsageLimitResetAt = %v, %t, want %v, true", resetAt, ok, wantReset)
		}
	}
}
