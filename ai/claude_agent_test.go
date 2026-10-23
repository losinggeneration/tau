package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	claude "github.com/losinggeneration/claude-agent-sdk-go"
)

func claudeAgentPromptString(t *testing.T, prompt any) string {
	t.Helper()
	s, ok := prompt.(string)
	if !ok {
		t.Fatalf("prompt = %T, want string", prompt)
	}
	return s
}

func TestClaudeAgentBuildPrompt(t *testing.T) {
	prompt := &Context{
		SystemPrompt: "stay terse",
		Messages: []Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: []ContentBlock{{Type: ContentBlockText, Text: "hello"}}},
			{Role: "user", Content: []ContentBlock{{Type: ContentBlockText, Text: "with attachment"}}},
			{Role: messageRoleToolResult, ToolName: "lookup", Content: []ContentBlock{{Type: ContentBlockToolResult, Text: "done"}}},
			{Role: "user", Content: ""},
		},
	}

	got := claudeAgentPromptContent(claudeAgentMessages(prompt))
	want := "User: hi\n\nAssistant: hello\n\nUser: with attachment\n\nTool result lookup: done"
	if claudeAgentPromptString(t, got) != want {
		t.Fatalf("prompt mismatch\nwant: %q\n got: %q", want, got)
	}
	if prompt.SystemPrompt != "stay terse" {
		t.Fatalf("system prompt mutated: %q", prompt.SystemPrompt)
	}
}

func TestClaudeAgentBuildPromptWithImage(t *testing.T) {
	messages := []Message{
		{Role: "user", Content: "look"},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: []ContentBlock{
			{Type: ContentBlockText, Text: "<file name=\"shot.png\">[image content]</file>"},
			{Type: ContentBlockImage, MimeType: "image/png", Data: "c2hvdA=="},
			{Type: ContentBlockText, Text: "what is it?"},
		}},
		{Role: messageRoleToolResult, ToolName: "read", Content: []ContentBlock{
			{Type: ContentBlockImage, Data: ""},
			{Type: ContentBlockImage, MimeType: "image/jpeg", Data: "anBn"},
		}},
	}

	got, ok := claudeAgentPromptContent(messages).([]map[string]any)
	if !ok {
		t.Fatalf("prompt = %T, want content blocks", claudeAgentPromptContent(messages))
	}
	want := []map[string]any{
		{"type": "text", "text": "User: look"},
		{"type": "text", "text": "Assistant: ok"},
		{"type": "text", "text": "User: <file name=\"shot.png\">[image content]</file>"},
		{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "c2hvdA=="}},
		{"type": "text", "text": "User: what is it?"},
		{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/jpeg", "data": "anBn"}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d blocks, want %d: %v", len(got), len(want), got)
	}
	for i, block := range got {
		if fmt.Sprint(block) != fmt.Sprint(want[i]) {
			t.Fatalf("block %d = %v, want %v", i, block, want[i])
		}
	}
}

func TestClaudeAgentBuildOptions(t *testing.T) {
	model := &Model{ID: "sonnet"}
	prompt := &Context{SystemPrompt: "system"}

	got := claudeAgentBuildOptions(model, prompt, applyOptions([]Option{WithReasoning(ReasoningHigh)}))
	if got.Model != "sonnet" {
		t.Fatalf("model = %q", got.Model)
	}
	if got.SystemPrompt != "system" {
		t.Fatalf("system prompt = %q", got.SystemPrompt)
	}
	if got.MaxTurns != 1 {
		t.Fatalf("max turns = %d", got.MaxTurns)
	}
	if got.Tools == nil || len(got.Tools) != 0 {
		t.Fatalf("tools = %#v", got.Tools)
	}
	if got.Effort != "high" {
		t.Fatalf("effort = %q", got.Effort)
	}

	got = claudeAgentBuildOptions(model, prompt, applyOptions([]Option{WithReasoning(ReasoningLow), WithEffort("medium"), WithSessionID("s1")}))
	if got.Effort != "medium" {
		t.Fatalf("explicit effort did not win: %q", got.Effort)
	}
	if got.SessionID != "" {
		t.Fatalf("session id = %q, want empty before session mapping", got.SessionID)
	}
}

func TestClaudeAgentBuildOptionsWithTools(t *testing.T) {
	model := &Model{ID: "sonnet"}
	prompt := &Context{Tools: []Tool{{Name: "lookup", Description: "look", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)}}}

	got := claudeAgentBuildOptions(model, prompt, applyOptions(nil))
	if len(got.Tools) != 1 || got.Tools[0] != "mcp__tau__lookup" {
		t.Fatalf("tools = %#v", got.Tools)
	}
	if len(got.AllowedTools) != 1 || got.AllowedTools[0] != "mcp__tau__lookup" {
		t.Fatalf("allowed tools = %#v", got.AllowedTools)
	}
	if len(got.MCPServers) != 1 {
		t.Fatalf("mcp servers = %#v", got.MCPServers)
	}
}

func TestClaudeAgentBuildOptionsSkipsInvalidTools(t *testing.T) {
	prompt := &Context{Tools: []Tool{
		{Name: "good", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
		{Name: "bad name", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "bad_schema", Parameters: json.RawMessage(`{"type":"array"}`)},
		{Name: "bad_json", Parameters: json.RawMessage(`{`)},
	}}

	got := claudeAgentBuildOptions(&Model{ID: "sonnet"}, prompt, applyOptions(nil))
	want := []string{"mcp__tau__good"}
	if !reflect.DeepEqual(got.Tools, want) {
		t.Fatalf("tools = %#v, want %#v", got.Tools, want)
	}
	if !reflect.DeepEqual(got.AllowedTools, want) {
		t.Fatalf("allowed tools = %#v, want %#v", got.AllowedTools, want)
	}
	serverConfig, ok := got.MCPServers["tau"].(claude.McpSdkServerConfig)
	if !ok {
		t.Fatalf("mcp server = %T", got.MCPServers["tau"])
	}
	registered := serverConfig.Instance.Tools()
	if len(registered) != 1 || registered[0].Name != "good" {
		t.Fatalf("registered tools = %#v", registered)
	}
}

func TestClaudeAgentStreamText(t *testing.T) {
	client := &claudeAgentClient{runner: func(ctx context.Context, prompt any, opts claude.Options) (<-chan claude.Message, <-chan error) {
		messages := make(chan claude.Message, 2)
		errs := make(chan error)
		messages <- claude.AssistantMessage{Content: []claude.ContentBlock{claude.TextBlock{Text: "ok"}}}
		messages <- claude.ResultMessage{StopReason: "end_turn", Usage: map[string]any{"input_tokens": 1, "output_tokens": 2}}
		close(messages)
		close(errs)
		return messages, errs
	}}
	model := &Model{ID: "sonnet", API: APIClaudeAgent, Provider: ProviderClaudeAgent}

	var events []StreamEvent
	if err := client.Stream(context.Background(), model, &Context{}, func(_ context.Context, event StreamEvent) {
		events = append(events, event)
	}); err != nil {
		t.Fatalf("stream: %v", err)
	}

	gotTypes := eventTypeNames(events)
	wantTypes := []string{"Start", "TextStart", "TextDelta", "TextEnd", "Done"}
	if !reflect.DeepEqual(gotTypes, wantTypes) {
		t.Fatalf("events = %#v, want %#v", gotTypes, wantTypes)
	}
	done := events[len(events)-1].(Done)
	if done.Message.Content[0].Text != "ok" {
		t.Fatalf("content = %#v", done.Message.Content)
	}
	if done.Message.Usage.TotalTokens != 3 {
		t.Fatalf("usage = %#v", done.Message.Usage)
	}
}

func TestClaudeAgentStreamToolCall(t *testing.T) {
	client := &claudeAgentClient{runner: func(ctx context.Context, prompt any, opts claude.Options) (<-chan claude.Message, <-chan error) {
		messages := make(chan claude.Message, 2)
		errs := make(chan error)
		messages <- claude.AssistantMessage{Content: []claude.ContentBlock{claude.ToolUseBlock{ID: "call_1", Name: "lookup", Input: map[string]any{"q": "go"}}}}
		messages <- claude.ResultMessage{StopReason: "tool_use"}
		close(messages)
		close(errs)
		return messages, errs
	}}
	model := &Model{ID: "sonnet", API: APIClaudeAgent, Provider: ProviderClaudeAgent}

	var events []StreamEvent
	if err := client.Stream(context.Background(), model, &Context{}, func(_ context.Context, event StreamEvent) {
		events = append(events, event)
	}); err != nil {
		t.Fatalf("stream: %v", err)
	}

	gotTypes := eventTypeNames(events)
	wantTypes := []string{"Start", "ToolCallStart", "ToolCallDelta", "ToolCallEnd", "Done"}
	if !reflect.DeepEqual(gotTypes, wantTypes) {
		t.Fatalf("events = %#v, want %#v", gotTypes, wantTypes)
	}
	toolEnd := events[3].(ToolCallEnd)
	if toolEnd.ToolCall.ID != "call_1" || toolEnd.ToolCall.Name != "lookup" || string(toolEnd.ToolCall.Arguments) != `{"q":"go"}` {
		t.Fatalf("tool call = %#v", toolEnd.ToolCall)
	}
	done := events[4].(Done)
	if done.Reason != StopReasonToolUse {
		t.Fatalf("done reason = %q", done.Reason)
	}
}

// The CLI presents tau's tools through the tau MCP server, so tool_use blocks
// come back with the mcp__tau__ prefix. tau's registry and transcript use the
// plain tool name, so the provider strips it at the SDK boundary.
func TestClaudeAgentStreamToolCallStripsMCPPrefix(t *testing.T) {
	client := &claudeAgentClient{runner: func(ctx context.Context, prompt any, opts claude.Options) (<-chan claude.Message, <-chan error) {
		messages := make(chan claude.Message, 2)
		errs := make(chan error)
		messages <- claude.AssistantMessage{Content: []claude.ContentBlock{claude.ToolUseBlock{ID: "call_1", Name: "mcp__tau__bash", Input: map[string]any{"command": "ls"}}}}
		messages <- claude.ResultMessage{StopReason: "tool_use"}
		close(messages)
		close(errs)
		return messages, errs
	}}
	model := &Model{ID: "sonnet", API: APIClaudeAgent, Provider: ProviderClaudeAgent}

	var events []StreamEvent
	if err := client.Stream(context.Background(), model, &Context{}, func(_ context.Context, event StreamEvent) {
		events = append(events, event)
	}); err != nil {
		t.Fatalf("stream: %v", err)
	}

	toolEnd := events[3].(ToolCallEnd)
	if toolEnd.ToolCall.ID != "call_1" || toolEnd.ToolCall.Name != "bash" || string(toolEnd.ToolCall.Arguments) != `{"command":"ls"}` {
		t.Fatalf("tool call = %#v", toolEnd.ToolCall)
	}
}

func TestClaudeAgentStreamError(t *testing.T) {
	boom := errors.New("boom")
	client := &claudeAgentClient{runner: func(ctx context.Context, prompt any, opts claude.Options) (<-chan claude.Message, <-chan error) {
		messages := make(chan claude.Message)
		errs := make(chan error, 1)
		errs <- boom
		close(messages)
		close(errs)
		return messages, errs
	}}
	model := &Model{ID: "sonnet", API: APIClaudeAgent, Provider: ProviderClaudeAgent}

	var events []StreamEvent
	if err := client.Stream(context.Background(), model, &Context{}, func(_ context.Context, event StreamEvent) {
		events = append(events, event)
	}); err != nil {
		t.Fatalf("stream: %v", err)
	}
	gotTypes := eventTypeNames(events)
	wantTypes := []string{"Start", "StreamError"}
	if !reflect.DeepEqual(gotTypes, wantTypes) {
		t.Fatalf("events = %#v, want %#v", gotTypes, wantTypes)
	}
	streamErr := events[1].(StreamError)
	if !errors.Is(streamErr, boom) {
		t.Fatalf("error = %v", streamErr)
	}
	if streamErr.Msg.StopReason != StopReasonError {
		t.Fatalf("stop reason = %q", streamErr.Msg.StopReason)
	}
}

func TestClaudeAgentStreamCancellationEmitsAborted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &claudeAgentClient{runner: func(ctx context.Context, prompt any, opts claude.Options) (<-chan claude.Message, <-chan error) {
		return make(chan claude.Message), make(chan error)
	}}

	var events []StreamEvent
	if err := client.Stream(ctx, &Model{ID: "sonnet", API: APIClaudeAgent, Provider: ProviderClaudeAgent}, &Context{}, func(_ context.Context, event StreamEvent) {
		events = append(events, event)
	}); err != nil {
		t.Fatalf("stream: %v", err)
	}
	gotTypes := eventTypeNames(events)
	wantTypes := []string{"Start", "StreamError"}
	if !reflect.DeepEqual(gotTypes, wantTypes) {
		t.Fatalf("events = %#v, want %#v", gotTypes, wantTypes)
	}
	streamErr := events[1].(StreamError)
	if streamErr.Msg.StopReason != StopReasonAborted {
		t.Fatalf("stop reason = %q", streamErr.Msg.StopReason)
	}
}

func TestClaudeAgentSessionFirstTurnUsesNewSessionID(t *testing.T) {
	tauSessionID := "f2df8aa3-20c8-4483-8f67-97604bd017de"
	var gotPrompt any
	var gotOpts claude.Options
	client := &claudeAgentClient{
		state: &claudeAgentState{sessions: make(map[string]*ClaudeAgentSessionState)},
		runner: func(ctx context.Context, prompt any, opts claude.Options) (<-chan claude.Message, <-chan error) {
			gotPrompt = prompt
			gotOpts = opts
			messages := make(chan claude.Message, 1)
			errs := make(chan error)
			messages <- claude.ResultMessage{SessionID: tauSessionID, StopReason: "end_turn"}
			close(messages)
			close(errs)
			return messages, errs
		},
	}

	_, err := client.Complete(context.Background(), &Model{ID: "sonnet", API: APIClaudeAgent, Provider: ProviderClaudeAgent}, &Context{Messages: []Message{{Role: "user", Content: "one"}}}, WithSessionID(tauSessionID))
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if gotOpts.SessionID == "" {
		t.Fatal("session id is empty")
	}
	if gotOpts.SessionID == tauSessionID {
		t.Fatalf("session id reused tau session id %q", tauSessionID)
	}
	if _, err := uuid.Parse(gotOpts.SessionID); err != nil {
		t.Fatalf("session id = %q, want uuid: %v", gotOpts.SessionID, err)
	}
	if gotOpts.Resume != "" {
		t.Fatalf("resume = %q, want empty", gotOpts.Resume)
	}
	if claudeAgentPromptString(t, gotPrompt) != "User: one" {
		t.Fatalf("prompt = %q", gotPrompt)
	}
}

func TestClaudeAgentSessionResumeUsesResume(t *testing.T) {
	state := &claudeAgentState{sessions: map[string]*ClaudeAgentSessionState{"tau-1": {SessionID: "claude-1", LastMessageCount: 2}}}
	var gotPrompt any
	var gotOpts claude.Options
	client := &claudeAgentClient{
		state: state,
		runner: func(ctx context.Context, prompt any, opts claude.Options) (<-chan claude.Message, <-chan error) {
			gotPrompt = prompt
			gotOpts = opts
			messages := make(chan claude.Message, 1)
			errs := make(chan error)
			messages <- claude.ResultMessage{SessionID: "claude-1", StopReason: "end_turn"}
			close(messages)
			close(errs)
			return messages, errs
		},
	}
	prompt := &Context{Messages: []Message{
		{Role: "user", Content: "old"},
		{Role: "assistant", Content: []ContentBlock{{Type: ContentBlockText, Text: "old response"}}},
		{Role: "user", Content: "new"},
	}}

	_, err := client.Complete(context.Background(), &Model{ID: "sonnet", API: APIClaudeAgent, Provider: ProviderClaudeAgent}, prompt, WithSessionID("tau-1"))
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if gotOpts.Resume != "claude-1" {
		t.Fatalf("resume = %q, want claude-1", gotOpts.Resume)
	}
	if gotOpts.SessionID != "" {
		t.Fatalf("session id = %q, want empty", gotOpts.SessionID)
	}
	if claudeAgentPromptString(t, gotPrompt) != "User: new" {
		t.Fatalf("prompt = %q, want only new message", gotPrompt)
	}
}

func TestClaudeAgentSessionStoresReturnedSessionID(t *testing.T) {
	state := &claudeAgentState{sessions: make(map[string]*ClaudeAgentSessionState)}
	client := &claudeAgentClient{
		state: state,
		runner: func(ctx context.Context, prompt any, opts claude.Options) (<-chan claude.Message, <-chan error) {
			messages := make(chan claude.Message, 1)
			errs := make(chan error)
			messages <- claude.ResultMessage{SessionID: "claude-returned", StopReason: "end_turn"}
			close(messages)
			close(errs)
			return messages, errs
		},
	}
	prompt := &Context{Messages: []Message{{Role: "user", Content: "one"}, {Role: "user", Content: "two"}}}

	_, err := client.Complete(context.Background(), &Model{ID: "sonnet", API: APIClaudeAgent, Provider: ProviderClaudeAgent}, prompt, WithSessionID("tau-1"))
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	got := state.sessions["tau-1"]
	if got == nil || got.SessionID != "claude-returned" {
		t.Fatalf("stored session = %#v", got)
	}
	if got.LastMessageCount != 2 {
		t.Fatalf("last message count = %d", got.LastMessageCount)
	}
}

func TestClaudeAgentResumePromptSkipsLocalAssistantMessages(t *testing.T) {
	state := &claudeAgentState{sessions: map[string]*ClaudeAgentSessionState{"tau-1": {SessionID: "claude-1", LastMessageCount: 2}}}
	var gotPrompt any
	client := &claudeAgentClient{
		state: state,
		runner: func(ctx context.Context, prompt any, opts claude.Options) (<-chan claude.Message, <-chan error) {
			gotPrompt = prompt
			messages := make(chan claude.Message, 1)
			errs := make(chan error)
			messages <- claude.ResultMessage{SessionID: "claude-1", StopReason: "end_turn"}
			close(messages)
			close(errs)
			return messages, errs
		},
	}
	prompt := &Context{Messages: []Message{
		{Role: "user", Content: "old"},
		{Role: "assistant", Content: []ContentBlock{{Type: ContentBlockText, Text: "old response"}}},
		{Role: "assistant", Content: []ContentBlock{{Type: ContentBlockText, Text: "looking now"}, {Type: ContentBlockToolCall, ID: "call_1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)}}},
		{Role: messageRoleTool, ToolName: "bash", Content: "file list"},
	}}

	_, err := client.Complete(context.Background(), &Model{ID: "sonnet", API: APIClaudeAgent, Provider: ProviderClaudeAgent}, prompt, WithSessionID("tau-1"))
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if claudeAgentPromptString(t, gotPrompt) != "Tool result bash: file list" {
		t.Fatalf("prompt = %q, want only the tool result", gotPrompt)
	}
}

func TestClaudeAgentCompactionStartsNewSession(t *testing.T) {
	tauSessionID := "df6b5a63-2697-4307-9d9c-6ccf515f4fa6"
	state := &claudeAgentState{sessions: map[string]*ClaudeAgentSessionState{tauSessionID: {SessionID: "old-claude", LastMessageCount: 4, CompactionSignature: "Session Summary: old"}}}
	var gotPrompt any
	var gotOpts claude.Options
	client := &claudeAgentClient{
		state: state,
		runner: func(ctx context.Context, prompt any, opts claude.Options) (<-chan claude.Message, <-chan error) {
			gotPrompt = prompt
			gotOpts = opts
			messages := make(chan claude.Message, 1)
			errs := make(chan error)
			messages <- claude.ResultMessage{SessionID: "new-claude", StopReason: "end_turn"}
			close(messages)
			close(errs)
			return messages, errs
		},
	}
	prompt := &Context{SystemPrompt: "base\n\nSession Summary: compacted", Messages: []Message{{Role: "user", Content: "after compact"}}}

	_, err := client.Complete(context.Background(), &Model{ID: "sonnet", API: APIClaudeAgent, Provider: ProviderClaudeAgent}, prompt, WithSessionID(tauSessionID))
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if gotOpts.Resume != "" {
		t.Fatalf("resume = %q, want empty", gotOpts.Resume)
	}
	if gotOpts.SessionID == "" || gotOpts.SessionID == tauSessionID {
		t.Fatalf("session id = %q, want fresh uuid", gotOpts.SessionID)
	}
	if !strings.Contains(gotOpts.SystemPrompt, "Session Summary: compacted") {
		t.Fatalf("system prompt = %q", gotOpts.SystemPrompt)
	}
	if claudeAgentPromptString(t, gotPrompt) != "User: after compact" {
		t.Fatalf("prompt = %q", gotPrompt)
	}
	if state.sessions[tauSessionID].SessionID != "new-claude" {
		t.Fatalf("stored session = %#v", state.sessions[tauSessionID])
	}
}

func TestClaudeAgentResumeSendsOnlyNewMessages(t *testing.T) {
	state := &claudeAgentState{sessions: map[string]*ClaudeAgentSessionState{"tau-1": {SessionID: "claude-1", LastMessageCount: 3}}}
	prompt, opts, _ := (&claudeAgentClient{state: state}).buildRequest(&Model{ID: "sonnet"}, &Context{Messages: []Message{
		{Role: "user", Content: "old user"},
		{Role: "assistant", Content: []ContentBlock{{Type: ContentBlockText, Text: "old assistant"}}},
		{Role: messageRoleToolResult, ToolName: "lookup", Content: []ContentBlock{{Type: ContentBlockToolResult, Text: "old tool"}}},
		{Role: messageRoleToolResult, ToolName: "lookup", Content: []ContentBlock{{Type: ContentBlockToolResult, Text: "new tool"}}},
		{Role: "user", Content: "new user"},
	}}, applyOptions([]Option{WithSessionID("tau-1")}))

	if opts.Resume != "claude-1" {
		t.Fatalf("resume = %q", opts.Resume)
	}
	got := claudeAgentPromptString(t, prompt)
	if strings.Contains(got, "old") {
		t.Fatalf("prompt replayed old content: %q", got)
	}
	if !strings.Contains(got, "Tool result lookup: new tool") || !strings.Contains(got, "User: new user") {
		t.Fatalf("prompt missing new content: %q", got)
	}
}

func TestClaudeAgentModelChangeStartsNewSession(t *testing.T) {
	tauSessionID := "f2df8aa3-20c8-4483-8f67-97604bd017de"
	state := &claudeAgentState{sessions: map[string]*ClaudeAgentSessionState{tauSessionID: {SessionID: "old-claude", LastMessageCount: 2, ModelID: "sonnet"}}}
	var gotPrompt any
	var gotOpts claude.Options
	client := &claudeAgentClient{
		state: state,
		runner: func(ctx context.Context, prompt any, opts claude.Options) (<-chan claude.Message, <-chan error) {
			gotPrompt = prompt
			gotOpts = opts
			messages := make(chan claude.Message, 1)
			errs := make(chan error)
			messages <- claude.ResultMessage{SessionID: "new-claude", StopReason: "end_turn"}
			close(messages)
			close(errs)
			return messages, errs
		},
	}
	prompt := &Context{Messages: []Message{
		{Role: "user", Content: "old"},
		{Role: "assistant", Content: []ContentBlock{{Type: ContentBlockText, Text: "old response"}}},
		{Role: "user", Content: "new"},
	}}

	_, err := client.Complete(context.Background(), &Model{ID: "opus", API: APIClaudeAgent, Provider: ProviderClaudeAgent}, prompt, WithSessionID(tauSessionID))
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if gotOpts.Resume != "" {
		t.Fatalf("resume = %q, want empty after model change", gotOpts.Resume)
	}
	if gotOpts.SessionID == "" || gotOpts.SessionID == tauSessionID {
		t.Fatalf("session id = %q, want fresh uuid", gotOpts.SessionID)
	}
	if claudeAgentPromptString(t, gotPrompt) != "User: old\n\nAssistant: old response\n\nUser: new" {
		t.Fatalf("prompt = %q", gotPrompt)
	}
	if state.sessions[tauSessionID].SessionID != "new-claude" || state.sessions[tauSessionID].ModelID != "opus" {
		t.Fatalf("stored state = %#v", state.sessions[tauSessionID])
	}
}

func TestClaudeAgentStreamMaxTurnsIsToolUse(t *testing.T) {
	client := &claudeAgentClient{
		state: &claudeAgentState{sessions: make(map[string]*ClaudeAgentSessionState)},
		runner: func(ctx context.Context, prompt any, opts claude.Options) (<-chan claude.Message, <-chan error) {
			messages := make(chan claude.Message, 2)
			errs := make(chan error)
			messages <- claude.AssistantMessage{Content: []claude.ContentBlock{claude.ToolUseBlock{ID: "call_1", Name: "bash", Input: map[string]any{"command": "ls"}}}}
			messages <- claude.ResultMessage{
				Subtype:    "error_max_turns",
				StopReason: "tool_use",
				IsError:    true,
				Errors:     []string{"Reached maximum number of turns (1)"},
				SessionID:  "cli-1",
			}
			close(messages)
			close(errs)
			return messages, errs
		},
	}
	model := &Model{ID: "sonnet", API: APIClaudeAgent, Provider: ProviderClaudeAgent}

	var events []StreamEvent
	if err := client.Stream(context.Background(), model, &Context{}, func(_ context.Context, event StreamEvent) {
		events = append(events, event)
	}, WithSessionID("tau-1")); err != nil {
		t.Fatalf("stream: %v", err)
	}

	gotTypes := eventTypeNames(events)
	wantTypes := []string{"Start", "ToolCallStart", "ToolCallDelta", "ToolCallEnd", "Done"}
	if !reflect.DeepEqual(gotTypes, wantTypes) {
		t.Fatalf("events = %#v, want %#v", gotTypes, wantTypes)
	}
	done := events[len(events)-1].(Done)
	if done.Reason != StopReasonToolUse {
		t.Fatalf("done reason = %q, want %q", done.Reason, StopReasonToolUse)
	}
	if done.Message.ErrorMessage != "" {
		t.Fatalf("error message = %q, want empty", done.Message.ErrorMessage)
	}
	if done.Message.StopReason != StopReasonToolUse {
		t.Fatalf("stop reason = %q, want %q", done.Message.StopReason, StopReasonToolUse)
	}
	session := client.state.sessions["tau-1"]
	if session == nil || session.SessionID != "cli-1" {
		t.Fatalf("stored session = %#v, want cli-1", session)
	}
}

func TestClaudeAgentToolMappingDefersToTauLoop(t *testing.T) {
	sdkOptions := claudeAgentBuildOptions(&Model{ID: "sonnet"}, &Context{Tools: []Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)}}}, applyOptions(nil))
	if len(sdkOptions.MCPServers) != 1 || len(sdkOptions.Tools) != 1 {
		t.Fatalf("sdk tool config = %#v", sdkOptions)
	}
	serverConfig, ok := sdkOptions.MCPServers["tau"].(claude.McpSdkServerConfig)
	if !ok {
		t.Fatalf("mcp server = %T", sdkOptions.MCPServers["tau"])
	}
	result, err := serverConfig.Instance.CallTool(context.Background(), "lookup", map[string]any{"q": "go"})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if result.IsError {
		t.Fatalf("result error = %#v", result)
	}
	if len(result.Content) == 0 || !strings.Contains(fmt.Sprint(result.Content[0]["text"]), "next message") {
		t.Fatalf("result = %#v, want deferral note", result)
	}
}

func TestClaudeAgentProviderRegistration(t *testing.T) {
	provider, ok := Get(APIClaudeAgent)
	if !ok {
		t.Fatal("provider not registered")
	}
	if _, ok := provider.(*ClaudeAgentProvider); !ok {
		t.Fatalf("provider = %T", provider)
	}
	client, err := GetClient(&Model{ID: "sonnet", API: APIClaudeAgent, Provider: ProviderClaudeAgent})
	if err != nil {
		t.Fatalf("get client: %v", err)
	}
	if _, ok := client.(*claudeAgentClient); !ok {
		t.Fatalf("client = %T", client)
	}
	if _, err := ResolveModelID("claude-agent/sonnet"); err != nil {
		t.Fatalf("resolve alias model: %v", err)
	}
	if len(ListModels(ProviderClaudeAgent)) <= 3 {
		t.Fatalf("claude-agent models missing anthropic mirror: %d", len(ListModels(ProviderClaudeAgent)))
	}
	mirrored, err := GetAvailableModel(ProviderClaudeAgent, "claude-opus-4-6")
	if err != nil {
		t.Fatalf("resolve mirrored model: %v", err)
	}
	if mirrored.API != APIClaudeAgent || mirrored.Provider != ProviderClaudeAgent {
		t.Fatalf("mirrored model = %s/%s via %s", mirrored.Provider, mirrored.ID, mirrored.API)
	}
	if !reflect.DeepEqual(mirrored.Cost, CostProfile{}) {
		t.Fatalf("mirrored model cost = %#v, want zero", mirrored.Cost)
	}
}

func eventTypeNames(events []StreamEvent) []string {
	names := make([]string, 0, len(events))
	for _, event := range events {
		switch event.(type) {
		case Start:
			names = append(names, "Start")
		case TextStart:
			names = append(names, "TextStart")
		case TextDelta:
			names = append(names, "TextDelta")
		case TextEnd:
			names = append(names, "TextEnd")
		case ToolCallStart:
			names = append(names, "ToolCallStart")
		case ToolCallDelta:
			names = append(names, "ToolCallDelta")
		case ToolCallEnd:
			names = append(names, "ToolCallEnd")
		case Done:
			names = append(names, "Done")
		case StreamError:
			names = append(names, "StreamError")
		}
	}
	return names
}

func TestClaudeAgentStreamUsageLimit(t *testing.T) {
	const message = "You've hit your session limit · resets 6:10am (America/Los_Angeles)"
	for _, source := range []string{"text", "assistant", "errors", "result", "runner"} {
		t.Run(source, func(t *testing.T) {
			client := &claudeAgentClient{runner: func(context.Context, any, claude.Options) (<-chan claude.Message, <-chan error) {
				messages := make(chan claude.Message, 2)
				errs := make(chan error, 1)
				result := claude.ResultMessage{IsError: true, StopReason: "error"}
				switch source {
				case "assistant":
					messages <- claude.AssistantMessage{Error: "rate_limit", Content: []claude.ContentBlock{claude.TextBlock{Text: message}}}
				case "text":
					messages <- claude.AssistantMessage{Content: []claude.ContentBlock{claude.TextBlock{Text: message}}}
				case "errors":
					result.Errors = []string{message}
				case "result":
					result.Result = message
				case "runner":
					errs <- errors.New(message)
				}
				if source != "runner" {
					messages <- result
				}
				close(messages)
				close(errs)
				return messages, errs
			}}
			var terminal StreamEvent
			before := time.Now()
			err := client.Stream(context.Background(), &Model{ID: "sonnet", API: APIClaudeAgent, Provider: ProviderClaudeAgent}, &Context{}, func(_ context.Context, event StreamEvent) { terminal = event })
			if err != nil {
				t.Fatal(err)
			}
			streamErr, ok := terminal.(StreamError)
			if !ok {
				t.Fatalf("terminal = %T, want StreamError", terminal)
			}
			resetAt, ok := UsageLimitResetAt(fmt.Errorf("wrapped: %w", streamErr))
			if !ok {
				t.Fatalf("missing reset time: %v", streamErr)
			}
			loc, err := time.LoadLocation("America/Los_Angeles")
			if err != nil {
				t.Fatal(err)
			}
			local := resetAt.In(loc)
			if local.Hour() != 6 || local.Minute() != 10 || !resetAt.After(before) || resetAt.Sub(before) > 25*time.Hour {
				t.Fatalf("reset time = %v", resetAt)
			}
			if streamErr.Msg.StopReason != StopReasonError || !strings.Contains(streamErr.Msg.ErrorMessage, "session limit") {
				t.Fatalf("message = %#v", streamErr.Msg)
			}
		})
	}
}

func TestParseClaudeAgentUsageLimitError(t *testing.T) {
	for _, tt := range []struct{ name, message, now, want string }{
		{"recorded", "You've hit your session limit · resets 6:10am (America/Los_Angeles)", "2026-10-11T12:39:52Z", "2026-10-11T13:10:00Z"},
		{"tomorrow", "You've hit your session limit · resets 6:10am (America/Los_Angeles)", "2026-10-11T14:00:00Z", "2026-10-12T13:10:00Z"},
		{"no minutes", "You've hit your session limit · resets 12am (Asia/Tokyo)", "2026-10-11T12:00:00Z", "2026-10-11T15:00:00Z"},
		{"noon", "You've hit your session limit · resets 12:10pm (UTC)", "2026-10-11T12:00:00Z", "2026-10-11T12:10:00Z"},
		{"DST rollover", "You've hit your session limit · resets 6:10am (America/Los_Angeles)", "2026-10-31T14:00:00Z", "2026-11-01T14:10:00Z"},
		{"invalid clock", "You've hit your session limit · resets 6:99am (America/Los_Angeles)", "2026-10-11T12:00:00Z", ""},
		{"invalid zone", "You've hit your session limit · resets 6:10am (Unknown/Zone)", "2026-10-11T12:00:00Z", ""},
		{"missing zone", "You've hit your session limit · resets 6:10am", "2026-10-11T12:00:00Z", ""},
		{"ordinary text", "Our meeting resets 6:10am (America/Los_Angeles)", "2026-10-11T12:00:00Z", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tt.now)
			if err != nil {
				t.Fatal(err)
			}
			got := parseClaudeAgentUsageLimitError(tt.message, now)
			if tt.want == "" {
				if got != nil {
					t.Fatalf("unexpected usage limit: %v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("missing usage limit")
			}
			if actual := got.ResetsAt.UTC().Format(time.RFC3339); actual != tt.want {
				t.Fatalf("reset = %s, want %s", actual, tt.want)
			}
		})
	}
}
