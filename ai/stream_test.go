package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type streamGolden struct {
	EventTypes []string
	StopReason StopReason
	Content    []ContentBlock
}

func assertStreamGolden(t *testing.T, events []StreamEvent, want streamGolden) AssistantMessage {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("stream emitted no events")
	}

	gotTypes := make([]string, len(events))
	terminalIndex := -1
	var terminal AssistantMessage
	for i, event := range events {
		gotTypes[i] = streamEventTypeName(event)
		switch e := event.(type) {
		case Done:
			if terminalIndex != -1 {
				t.Fatalf("multiple terminal stream events: first at %d, second at %d", terminalIndex, i)
			}
			terminalIndex = i
			terminal = e.Message
			if e.Reason != want.StopReason {
				t.Fatalf("done stop reason = %q, want %q", e.Reason, want.StopReason)
			}
		case StreamError:
			if terminalIndex != -1 {
				t.Fatalf("multiple terminal stream events: first at %d, second at %d", terminalIndex, i)
			}
			terminalIndex = i
			terminal = e.Msg
		}
	}
	if terminalIndex == -1 {
		t.Fatalf("no terminal Done/StreamError in events: %v", gotTypes)
	}
	if terminalIndex != len(events)-1 {
		t.Fatalf("terminal event index = %d, want final index %d; events=%v", terminalIndex, len(events)-1, gotTypes)
	}
	if len(want.EventTypes) > 0 && !reflect.DeepEqual(gotTypes, want.EventTypes) {
		t.Fatalf("event types = %v, want %v", gotTypes, want.EventTypes)
	}
	if len(want.Content) > 0 && !reflect.DeepEqual(terminal.Content, want.Content) {
		t.Fatalf("terminal content = %#v, want %#v", terminal.Content, want.Content)
	}

	assertStreamContentIndexes(t, events, terminal)
	assertToolArgumentsAreParsedObjects(t, terminal)
	assertNoStreamScratchState(t, terminal)

	return terminal
}

func streamEventTypeName(event StreamEvent) string {
	switch event.(type) {
	case Start:
		return "Start"
	case TextStart:
		return "TextStart"
	case TextDelta:
		return "TextDelta"
	case TextEnd:
		return "TextEnd"
	case ThinkingStart:
		return "ThinkingStart"
	case ThinkingDelta:
		return "ThinkingDelta"
	case ThinkingEnd:
		return "ThinkingEnd"
	case ToolCallStart:
		return "ToolCallStart"
	case ToolCallDelta:
		return "ToolCallDelta"
	case ToolCallEnd:
		return "ToolCallEnd"
	case *Usage:
		return "Usage"
	case Done:
		return "Done"
	case StreamError:
		return "StreamError"
	default:
		return fmt.Sprintf("%T", event)
	}
}

func assertStreamContentIndexes(t *testing.T, events []StreamEvent, final AssistantMessage) {
	t.Helper()
	for i, event := range events {
		idx, ok := streamEventContentIndex(event)
		if !ok {
			continue
		}
		if idx < 0 || idx >= len(final.Content) {
			t.Fatalf("event %d (%s) contentIndex = %d outside final content len %d", i, streamEventTypeName(event), idx, len(final.Content))
		}
		switch event.(type) {
		case TextStart, TextDelta, TextEnd:
			if final.Content[idx].Type != ContentBlockText {
				t.Fatalf("event %d (%s) indexes final block type %q, want text", i, streamEventTypeName(event), final.Content[idx].Type)
			}
		case ThinkingStart, ThinkingDelta, ThinkingEnd:
			if final.Content[idx].Type != ContentBlockThinking {
				t.Fatalf("event %d (%s) indexes final block type %q, want thinking", i, streamEventTypeName(event), final.Content[idx].Type)
			}
		case ToolCallStart, ToolCallDelta, ToolCallEnd:
			if final.Content[idx].Type != ContentBlockToolCall {
				t.Fatalf("event %d (%s) indexes final block type %q, want toolCall", i, streamEventTypeName(event), final.Content[idx].Type)
			}
		}
	}
}

func streamEventContentIndex(event StreamEvent) (int, bool) {
	switch e := event.(type) {
	case TextStart:
		return e.ContentIndex, true
	case TextDelta:
		return e.ContentIndex, true
	case TextEnd:
		return e.ContentIndex, true
	case ThinkingStart:
		return e.ContentIndex, true
	case ThinkingDelta:
		return e.ContentIndex, true
	case ThinkingEnd:
		return e.ContentIndex, true
	case ToolCallStart:
		return e.ContentIndex, true
	case ToolCallDelta:
		return e.ContentIndex, true
	case ToolCallEnd:
		return e.ContentIndex, true
	default:
		return 0, false
	}
}

func assertToolArgumentsAreParsedObjects(t *testing.T, msg AssistantMessage) {
	t.Helper()
	for _, block := range msg.Content {
		if block.Type != ContentBlockToolCall {
			continue
		}
		if len(block.Arguments) == 0 {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(block.Arguments, &obj); err != nil {
			t.Fatalf("tool call %q arguments are not a JSON object: %s: %v", block.ID, string(block.Arguments), err)
		}
	}
}

func assertNoStreamScratchState(t *testing.T, msg AssistantMessage) {
	t.Helper()
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal final assistant message: %v", err)
	}
	for _, scratch := range []string{"partial_json", "partialJson", "delta", "stream", "scratch"} {
		if strings.Contains(string(data), scratch) {
			t.Fatalf("final assistant message contains stream scratch marker %q: %s", scratch, string(data))
		}
	}
}

func TestOpenAIStreamSetupFailureReturnsDirectErrorBeforeEvents(t *testing.T) {
	client := &openAIClient{
		model:     &Model{ID: "gpt-4o", Provider: ProviderOpenAI},
		transport: NewTransport("://bad-url", "", nil),
	}

	var events []StreamEvent
	err := client.Stream(
		context.Background(),
		client.model,
		&Context{Messages: []Message{{Role: "user", Content: "hello"}}},
		func(_ context.Context, event StreamEvent) {
			events = append(events, event)
		},
	)
	if err == nil {
		t.Fatal("Stream error = nil, want setup failure")
	}
	if len(events) != 0 {
		t.Fatalf("len(events) = %d, want 0 for setup failure", len(events))
	}
}

func TestAzureStreamInBandFailureEmitsTerminalStreamErrorMessage(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_123"}}`,
		`data: {"type":"response.failed","response":{"id":"resp_123","status":"failed","error":{"message":"boom"}}}`,
		"",
	}, "\n"))

	var (
		events     []StreamEvent
		streamErrs []StreamError
	)
	err := streamAzureResponse(
		context.Background(),
		&Model{ID: "gpt-4.1", Provider: ProviderAzureOpenAI},
		stream,
		func(_ context.Context, event StreamEvent) {
			events = append(events, event)
			if e, ok := event.(StreamError); ok {
				streamErrs = append(streamErrs, e)
			}
		},
		nil,
	)
	if err != nil {
		t.Fatalf("streamAzureResponse returned %v, want nil after emitting StreamError", err)
	}
	if len(streamErrs) != 1 {
		t.Fatalf("len(streamErrs) = %d, want 1", len(streamErrs))
	}

	got := streamErrs[0]
	if got.Msg.StopReason != StopReasonError {
		t.Fatalf("streamErr.Msg.StopReason = %q, want %q", got.Msg.StopReason, StopReasonError)
	}
	if got.Msg.ErrorMessage != "boom" {
		t.Fatalf("streamErr.Msg.ErrorMessage = %q, want boom", got.Msg.ErrorMessage)
	}
	if got.Msg.ResponseID != "resp_123" {
		t.Fatalf("streamErr.Msg.ResponseID = %q, want resp_123", got.Msg.ResponseID)
	}

	var doneCount int
	for _, event := range events {
		if _, ok := event.(Done); ok {
			doneCount++
		}
	}
	if doneCount != 0 {
		t.Fatalf("doneCount = %d, want 0 on terminal stream error", doneCount)
	}
}

func TestOpenAIStreamInBandFailureDoesNotReturnDuplicateError(t *testing.T) {
	client := &openAIClient{
		model: &Model{ID: "gpt-4o", Provider: ProviderOpenAI},
	}

	var streamErrs []StreamError
	err := client.handleChunk(
		context.Background(),
		openAIChunk{
			Error: &openAIError{Message: "boom"},
		},
		func(_ context.Context, event StreamEvent) {
			if e, ok := event.(StreamError); ok {
				streamErrs = append(streamErrs, e)
			}
		},
	)
	if err != nil {
		t.Fatalf("handleChunk returned %v, want nil after emitting StreamError", err)
	}
	if len(streamErrs) != 1 {
		t.Fatalf("len(streamErrs) = %d, want 1", len(streamErrs))
	}
	if streamErrs[0].Msg.StopReason != StopReasonError {
		t.Fatalf("streamErr.Msg.StopReason = %q, want %q", streamErrs[0].Msg.StopReason, StopReasonError)
	}
	if streamErrs[0].Msg.ErrorMessage != "boom" {
		t.Fatalf("streamErr.Msg.ErrorMessage = %q, want boom", streamErrs[0].Msg.ErrorMessage)
	}
	if streamErrs[0].Err == nil {
		t.Fatal("streamErr.Err = nil, want underlying error")
	}
	if streamErrs[0].Err.Error() != "boom" {
		t.Fatalf("streamErr.Err.Error() = %q, want boom", streamErrs[0].Err.Error())
	}
}
