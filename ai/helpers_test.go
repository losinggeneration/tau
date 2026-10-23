package ai

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

// mockClient is a test client that records Stream calls.
type mockClient struct {
	streamCalls   int
	completeCalls int
	streamErr     error
	completeMsg   *AssistantMessage
	completeErr   error
}

func (m *mockClient) Stream(ctx context.Context, model *Model, prompt *Context, handler EventHandler, opts ...Option) error {
	m.streamCalls++
	if m.streamErr != nil {
		return m.streamErr
	}
	handler(ctx, Done{
		Reason: StopReasonStop,
		Message: AssistantMessage{
			Role:       "assistant",
			Content:    []ContentBlock{{Type: ContentBlockText, Text: "test response"}},
			StopReason: StopReasonStop,
			Timestamp:  1234567890,
		},
	})
	return nil
}

func (m *mockClient) Complete(ctx context.Context, model *Model, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	m.completeCalls++
	if m.completeErr != nil {
		return nil, m.completeErr
	}
	if m.completeMsg != nil {
		return m.completeMsg, nil
	}
	return &AssistantMessage{
		Role:       "assistant",
		Content:    []ContentBlock{{Type: ContentBlockText, Text: "test complete"}},
		StopReason: StopReasonStop,
		Timestamp:  1234567890,
	}, nil
}

func TestStreamWithModelResolvesClient(t *testing.T) {
	// Register a mock provider for testing
	Register("test-api-12345", &mockProvider{apiName: "test-api-12345"})

	model := &Model{
		ID:       "test-model",
		API:      "test-api-12345",
		Provider: "test-provider",
		Cost:     CostProfile{Input: 1, Output: 2},
	}

	var streamCalls int
	handler := func(_ context.Context, event StreamEvent) {
		if _, ok := event.(Done); ok {
			streamCalls++
		}
	}

	err := StreamWithModel(context.Background(), model, &Context{Messages: []Message{{Role: "user", Content: "test", Timestamp: 1}}}, handler)
	if err != nil {
		t.Fatalf("StreamWithModel: %v", err)
	}

	if streamCalls != 1 {
		t.Errorf("streamCalls = %d, want 1", streamCalls)
	}
}

func TestCompleteWithModelResolvesClient(t *testing.T) {
	// Register a mock provider for testing
	Register("test-api-complete-12345", &mockProvider{apiName: "test-api-complete-12345"})

	model := &Model{
		ID:       "test-model",
		API:      "test-api-complete-12345",
		Provider: "test-provider",
		Cost:     CostProfile{Input: 1, Output: 2},
	}

	msg, err := CompleteWithModel(context.Background(), model, &Context{Messages: []Message{{Role: "user", Content: "test", Timestamp: 1}}})
	if err != nil {
		t.Fatalf("CompleteWithModel: %v", err)
	}

	if msg == nil {
		t.Fatal("msg is nil")
	}
	if msg.Role != "assistant" {
		t.Errorf("msg.Role = %q, want assistant", msg.Role)
	}
}

// stubStreamer is a minimal Streamer implementation for testing
// completeViaStream. It emits the events configured in events and returns
// streamErr. If events is nil and streamErr is nil, Stream emits nothing
// (used to test the zero-result edge case).
type stubStreamer struct {
	events      []StreamEvent
	streamErr   error
	streamCalls int
}

func (s *stubStreamer) Stream(_ context.Context, _ *Model, _ *Context, handler EventHandler, _ ...Option) error {
	s.streamCalls++
	for _, e := range s.events {
		handler(context.Background(), e)
	}
	return s.streamErr
}

func TestCompleteViaStream(t *testing.T) {
	doneMsg := AssistantMessage{Role: "assistant", Content: []ContentBlock{{Type: ContentBlockText, Text: "done"}}}
	doneEvent := Done{Message: doneMsg}
	errMsg := AssistantMessage{Role: "assistant", ErrorMessage: "boom"}
	errEvent := StreamError{Msg: errMsg}

	tests := []struct {
		name        string
		events      []StreamEvent
		streamErr   error
		wantErr     bool
		wantContent string
		wantIsError bool
	}{
		{
			name:        "Done event returns the assistant message",
			events:      []StreamEvent{doneEvent},
			wantContent: "done",
		},
		{
			name:        "StreamError event returns the error message",
			events:      []StreamEvent{errEvent},
			wantIsError: true,
		},
		{
			name:      "Stream returning error propagates as Go error",
			events:    nil,
			streamErr: fmt.Errorf("transport failed"),
			wantErr:   true,
		},
		{
			name:    "no terminal event returns error",
			events:  nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &stubStreamer{events: tt.events, streamErr: tt.streamErr}
			msg, err := completeViaStream(context.Background(), s, &Model{ID: "m"}, &Context{}, []Option{}...)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if msg == nil {
				t.Fatal("msg is nil")
			}
			if (msg.ErrorMessage != "") != tt.wantIsError {
				t.Errorf("ErrorMessage = %q, wantIsError = %v", msg.ErrorMessage, tt.wantIsError)
			}
			// Verify content from Done event.
			if len(tt.events) > 0 {
				if d, ok := tt.events[0].(Done); ok {
					if len(d.Message.Content) > 0 && d.Message.Content[0].Text != tt.wantContent {
						t.Errorf("content = %q, want %q", d.Message.Content[0].Text, tt.wantContent)
					}
				}
			}
		})
	}
}

func TestStreamWithModelErrorNoProvider(t *testing.T) {
	model := &Model{
		ID:       "unknown-model",
		API:      "nonexistent-api",
		Provider: "unknown-provider",
	}

	handler := func(_ context.Context, event StreamEvent) {}

	err := StreamWithModel(context.Background(), model, &Context{}, handler)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestCompleteWithModelErrorNoProvider(t *testing.T) {
	model := &Model{
		ID:       "unknown-model",
		API:      "nonexistent-api",
		Provider: "unknown-provider",
	}

	_, err := CompleteWithModel(context.Background(), model, &Context{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// mockProvider implements Provider for testing.
// It returns the API name it was initialized with, so tests can register
// multiple providers with different APIs.
type mockProvider struct {
	apiName string
}

func (p *mockProvider) API() string {
	return p.apiName
}

func (p *mockProvider) IsAlias() bool { return false }

func (p *mockProvider) NewClient(model *Model) (Client, error) {
	return &mockClient{}, nil
}

func TestStreamSimpleWithProvider(t *testing.T) {
	// StreamSimple should resolve the first model from ListModels(provider)
	// If no models are available for a provider, it should return an error

	// Test with a provider that has models
	anthropicModels := ListModels(ProviderAnthropic)
	if len(anthropicModels) == 0 {
		t.Skip("no Anthropic models available")
	}

	// StreamSimple should not fail due to "no models" for Anthropic
	handler := func(_ context.Context, event StreamEvent) {}

	err := StreamSimple(context.Background(), ProviderAnthropic, &Context{
		Messages: []Message{{Role: "user", Content: "test", Timestamp: 1}},
	}, handler, nil)
	// We expect either success or a network error, not "no models available"
	if err != nil && err.Error() == "stream simple: no models available for provider: anthropic" {
		t.Errorf("StreamSimple returned no models error for Anthropic: %v", err)
	}
}

func TestCompleteSimpleWithProvider(t *testing.T) {
	// Test with a provider that has models
	mistralModels := ListModels(ProviderMistral)
	if len(mistralModels) == 0 {
		t.Skip("no Mistral models available")
	}

	msg, err := CompleteSimple(context.Background(), ProviderMistral, &Context{
		Messages: []Message{{Role: "user", Content: "test", Timestamp: 1}},
	}, nil)
	// We expect either success or a network error, not "no models available"
	if err != nil && err.Error() == "complete simple: no models available for provider: mistral" {
		t.Errorf("CompleteSimple returned no models error for Mistral: %v", err)
	}
	_ = msg // May be nil on network errors
}

// Test that Stream/Complete (model ID string versions) still work
func TestStreamWithModelID(t *testing.T) {
	// Get a known model from the registry
	model, err := GetModel("anthropic", "claude-3-5-sonnet-20241022")
	if err != nil {
		t.Skip("claude-3-5-sonnet-20241022 not available")
	}

	_ = model

	handler := func(_ context.Context, event StreamEvent) {}

	// Stream with model ID string should use GetModel then StreamWithModel
	err = Stream(context.Background(), "anthropic/claude-3-5-sonnet-20241022", &Context{
		Messages: []Message{{Role: "user", Content: "test", Timestamp: 1}},
	}, handler)
	if err != nil && err.Error() == "faux stream: model not found: anthropic/claude-3-5-sonnet-20241022" {
		t.Errorf("Stream returned model not found error: %v", err)
	}
}

func TestCompleteWithModelID(t *testing.T) {
	model, err := GetModel("anthropic", "claude-3-5-sonnet-20241022")
	if err != nil {
		t.Skip("claude-3-5-sonnet-20241022 not available")
	}

	msg, err := Complete(context.Background(), "anthropic/claude-3-5-sonnet-20241022", &Context{
		Messages: []Message{{Role: "user", Content: "test", Timestamp: 1}},
	})
	if err != nil && err.Error() == "faux complete: model not found: anthropic/claude-3-5-sonnet-20241022" {
		t.Errorf("Complete returned model not found error: %v", err)
	}
	_ = msg
	_ = model // Use the variable
}

// Test helper with options
func TestStreamWithModelAndOptions(t *testing.T) {
	// Register test provider
	Register("test-options-api-12345", &mockProvider{apiName: "test-options-api-12345"})

	model := &Model{
		ID:       "test-model",
		API:      "test-options-api-12345",
		Provider: "test-provider",
		Cost:     CostProfile{Input: 1, Output: 2},
	}

	var streamCalls int
	handler := func(_ context.Context, event StreamEvent) {
		if _, ok := event.(Done); ok {
			streamCalls++
		}
	}

	// Test with options
	err := StreamWithModel(context.Background(), model, &Context{
		Messages: []Message{{Role: "user", Content: "test", Timestamp: 1}},
	}, handler, WithTemperature(0.7))
	if err != nil {
		t.Fatalf("StreamWithModel with options: %v", err)
	}

	if streamCalls != 1 {
		t.Errorf("streamCalls = %d, want 1", streamCalls)
	}
}

// Test error propagation from client
func TestStreamWithModelErrorPropagation(t *testing.T) {
	// Create a provider that returns an error
	Register("test-error-api-12345", &errorProvider{})

	model := &Model{
		ID:       "test-model",
		API:      "test-error-api-12345",
		Provider: "test-provider",
		Cost:     CostProfile{Input: 1, Output: 2},
	}

	handler := func(_ context.Context, event StreamEvent) {}

	err := StreamWithModel(context.Background(), model, &Context{}, handler)
	if err == nil {
		t.Error("expected error, got nil")
	}
}

// errorProvider always returns errors.
type errorProvider struct{}

func (p *errorProvider) API() string {
	return "test-error-api-12345"
}

func (p *errorProvider) IsAlias() bool { return false }

func (p *errorProvider) NewClient(model *Model) (Client, error) {
	return &errorClient{}, nil
}

type errorClient struct{}

func (c *errorClient) Stream(ctx context.Context, model *Model, prompt *Context, handler EventHandler, opts ...Option) error {
	return errors.New("test stream error")
}

func (c *errorClient) Complete(ctx context.Context, model *Model, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	return nil, errors.New("test complete error")
}

// Test that options are passed through correctly
func TestCompleteWithModelAndOptions(t *testing.T) {
	Register("test-complete-options-api-12345", &mockProvider{apiName: "test-complete-options-api-12345"})

	model := &Model{
		ID:       "test-model",
		API:      "test-complete-options-api-12345",
		Provider: "test-provider",
		Cost:     CostProfile{Input: 1, Output: 2},
	}

	msg, err := CompleteWithModel(context.Background(), model, &Context{
		Messages: []Message{{Role: "user", Content: "test", Timestamp: 1}},
	}, WithTemperature(0.5), WithMaxTokens(100))
	if err != nil {
		t.Fatalf("CompleteWithModel with options: %v", err)
	}

	if msg == nil {
		t.Fatal("msg is nil")
	}
}

func TestCompleteViaStreamRecordsDuration(t *testing.T) {
	msg, err := completeViaStream(context.Background(), &mockClient{}, &Model{}, &Context{})
	if err != nil {
		t.Fatal(err)
	}
	if msg.DurationMs == nil || *msg.DurationMs < 0 {
		t.Fatalf("missing duration: %#v", msg.DurationMs)
	}
}

func TestCompleteViaStreamRetainsFirstTerminal(t *testing.T) {
	first := AssistantMessage{Role: "assistant", Content: []ContentBlock{{Type: ContentBlockText, Text: "first"}}, Usage: Usage{Input: 7}, DurationMs: new(12.5)}
	late := AssistantMessage{Role: "assistant", Content: []ContentBlock{{Type: ContentBlockText, Text: "late"}}, Usage: Usage{Input: 99}}
	for _, failed := range []bool{false, true} {
		for _, cleanup := range []bool{false, true} {
			t.Run(fmt.Sprintf("failed=%t/cleanup=%t", failed, cleanup), func(t *testing.T) {
				var terminal StreamEvent = Done{Message: first}
				if failed {
					terminal = StreamError{Msg: first, Err: errors.New("first failure")}
				}
				s := &stubStreamer{events: []StreamEvent{terminal, Start{Partial: late}, Done{Message: late}, StreamError{Msg: late}}}
				failure := errors.New("cleanup failed")
				if cleanup {
					s.streamErr = failure
				}
				got, err := completeViaStream(t.Context(), s, &Model{}, &Context{})
				if cleanup {
					if !errors.Is(err, failure) || got != nil {
						t.Fatalf("got=%+v err=%v", got, err)
					}
					return
				}
				if err != nil || got == nil || !reflect.DeepEqual(*got, first) {
					t.Fatalf("got=%+v err=%v; want first terminal %+v", got, err, first)
				}
			})
		}
	}
}

func TestTimedEventHandlerIgnoresEventsAfterTerminal(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			var got []StreamEvent
			handler := timedEventHandler(func(_ context.Context, event StreamEvent) { got = append(got, event) })
			handler(t.Context(), Start{})
			first := AssistantMessage{Role: "assistant", DurationMs: new(12.5)}
			var terminal StreamEvent = Done{Message: first}
			if failed {
				terminal = StreamError{Msg: first}
			}
			handler(t.Context(), terminal)
			var workers sync.WaitGroup
			for range 8 {
				workers.Go(func() {
					handler(t.Context(), Start{})
					handler(t.Context(), TextDelta{Delta: "late"})
					handler(t.Context(), Done{})
					handler(t.Context(), StreamError{})
				})
			}
			workers.Wait()
			if len(got) != 2 || !reflect.DeepEqual(got[1], terminal) {
				t.Fatalf("received %d events; want start and original terminal", len(got))
			}
		})
	}
}
