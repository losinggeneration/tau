package ai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcessResponsesStreamTerminalOutcomesMatchUpstream(t *testing.T) {
	const (
		added     = `data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":""}}`
		done      = `data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{}"}}`
		completed = `data: {"type":"response.completed","response":{"id":"r","status":"completed"}}`
	)
	for _, tc := range []struct {
		name, message string
		lines         []string
		cancel        bool
		reason        StopReason
	}{
		{name: "tool use", lines: []string{added, done, completed}, reason: StopReasonToolUse},
		{name: "incomplete event length", lines: []string{`data: {"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`}, reason: StopReasonLength},
		{name: "incomplete filter", lines: []string{`data: {"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"content_filter"}}}`}, reason: StopReasonError, message: "Response incomplete: content_filter"},
		{name: "no terminal event", lines: []string{added, done}, reason: StopReasonError, message: "openai responses stream ended before a terminal response event"},
		{name: "unfinished tool call", lines: []string{added, completed}, reason: StopReasonError, message: "openai responses stream completed with an unfinished tool call: read (call_1|fc_1)"},
		{name: "canceled", lines: []string{added, done, completed}, cancel: true, reason: StopReasonAborted, message: "request was aborted: context canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			reason, message := streamTerminal(t, func(handler EventHandler) {
				reader := newResponsesStreamReader(strings.NewReader(strings.Join(tc.lines, "\n") + "\n"))
				if err := ProcessResponsesStream(ctx, reader, &AssistantMessage{Role: "assistant"}, handler, &Model{ID: "gpt"}, nil); err != nil {
					t.Fatal(err)
				}
			})
			if reason != tc.reason || message != tc.message {
				t.Fatalf("got %s %q, want %s %q", reason, message, tc.reason, tc.message)
			}
		})
	}
}

func TestResponsesToolNamespacesSurviveStreamAndReplay(t *testing.T) {
	model := &Model{ID: "gpt", Provider: ProviderOpenAI, API: APIOpenAIResponses}
	for _, kind := range []string{"function_call", "custom_tool_call"} {
		for _, stage := range []string{"added", "done", "replaced", "empty", "absent"} {
			t.Run(kind+"/"+stage, func(t *testing.T) {
				added := map[string]any{"type": kind, "id": "fc_1", "call_id": "call_1", "name": "lookup", "arguments": "", "input": ""}
				done := map[string]any{"type": kind, "id": "fc_1", "call_id": "call_1", "name": "lookup", "arguments": `{"input":"hello"}`, "input": "hello"}
				if kind == "custom_tool_call" {
					added["id"], done["id"] = "ctc_1", "ctc_1"
				}
				var namespace any
				switch stage {
				case "added":
					added["namespace"], namespace = "tools", "tools"
				case "done":
					done["namespace"], namespace = "tools", "tools"
				case "replaced":
					added["namespace"], done["namespace"], namespace = "old", "tools", "tools"
				case "empty":
					added["namespace"], done["namespace"], namespace = "old", "", ""
				}
				var body strings.Builder
				for i, item := range []map[string]any{added, done} {
					event := map[string]any{"type": []string{"response.output_item.added", "response.output_item.done"}[i], "item": item}
					data, err := json.Marshal(event)
					require.NoError(t, err)
					body.WriteString("data: " + string(data) + "\n\n")
				}
				body.WriteString("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
				var output AssistantMessage
				output.Role, output.Provider, output.API, output.Model = "assistant", model.Provider, model.API, model.ID
				var ended ToolCall
				require.NoError(t, ProcessResponsesStream(t.Context(), newResponsesStreamReader(strings.NewReader(body.String())), &output, func(_ context.Context, event StreamEvent) {
					if e, ok := event.(ToolCallEnd); ok {
						ended = e.ToolCall
					}
				}, model, nil))
				require.Len(t, output.Content, 1)
				for _, value := range []any{output.Content[0], ended} {
					data, err := json.Marshal(value)
					require.NoError(t, err)
					var fields map[string]any
					require.NoError(t, json.Unmarshal(data, &fields))
					require.Equal(t, namespace, fields["namespace"])
				}
				grammar := map[string]string{}
				if kind == "custom_tool_call" {
					grammar["lookup"] = "input"
				}
				message := Message{Role: "assistant", Provider: output.Provider, API: output.API, Model: output.Model, Content: output.Content}
				for _, target := range []*Model{model, {ID: "other", Provider: model.Provider, API: model.API}, {ID: model.ID, Provider: "azure", API: model.API}, {ID: model.ID, Provider: model.Provider, API: "openai-codex-responses"}} {
					transformed, err := TransformContext(&Context{Messages: []Message{message}}, target, nil)
					require.NoError(t, err)
					items, err := convertAssistantMessageForResponses(target, transformed.Messages[0], 0, grammar)
					require.NoError(t, err)
					require.Equal(t, kind, items[0]["type"])
					want := namespace
					if target != model {
						want = nil
					}
					require.Equal(t, want, items[0]["namespace"])
				}
			})
		}
	}
}
