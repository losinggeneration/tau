package agent

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/losinggeneration/tau/ai"
)

func detailsMap(t *testing.T, details any) map[string]any {
	t.Helper()

	result, ok := details.(map[string]any)
	if !ok {
		t.Fatalf("details type = %T, want map[string]any", details)
	}

	return result
}

func TestMessageRoles(t *testing.T) {
	if RoleUser != "user" {
		t.Errorf("RoleUser = %q, want %q", RoleUser, "user")
	}

	if RoleAssistant != "assistant" {
		t.Errorf("RoleAssistant = %q, want %q", RoleAssistant, "assistant")
	}

	if RoleTool != "tool" {
		t.Errorf("RoleTool = %q, want %q", RoleTool, "tool")
	}

	if RoleSystem != "system" {
		t.Errorf("RoleSystem = %q, want %q", RoleSystem, "system")
	}
}

func TestNewUserMessage(t *testing.T) {
	msg := NewUserMessage("Hello, world!")

	if msg.Role != RoleUser {
		t.Errorf("Role = %q, want %q", msg.Role, RoleUser)
	}

	if len(msg.Content) != 1 {
		t.Fatalf("Content length = %d, want 1", len(msg.Content))
	}

	if msg.Content[0].Type != ai.ContentBlockText {
		t.Errorf("Content[0].Type = %q, want %q", msg.Content[0].Type, ai.ContentBlockText)
	}

	if msg.Content[0].Text != "Hello, world!" {
		t.Errorf("Content[0].Text = %q, want %q", msg.Content[0].Text, "Hello, world!")
	}

	if msg.Timestamp.IsZero() {
		t.Error("Timestamp should not be zero")
	}
}

func TestNewAssistantMessage(t *testing.T) {
	content := []ai.ContentBlock{
		{Type: ai.ContentBlockText, Text: "I am an assistant."},
	}

	msg := NewAssistantMessage(content)

	if msg.Role != RoleAssistant {
		t.Errorf("Role = %q, want %q", msg.Role, RoleAssistant)
	}

	if len(msg.Content) != 1 {
		t.Fatalf("Content length = %d, want 1", len(msg.Content))
	}

	if msg.Timestamp.IsZero() {
		t.Error("Timestamp should not be zero")
	}
}

func TestNewToolMessage(t *testing.T) {
	msg := NewToolMessage("call_123", "read", "file contents here", false)

	if msg.Role != RoleTool {
		t.Errorf("Role = %q, want %q", msg.Role, RoleTool)
	}

	if msg.ToolCallID != "call_123" {
		t.Errorf("ToolCallID = %q, want %q", msg.ToolCallID, "call_123")
	}

	if msg.ToolName != "read" {
		t.Errorf("ToolName = %q, want %q", msg.ToolName, "read")
	}

	if msg.IsError {
		t.Error("IsError should be false")
	}

	if len(msg.Content) != 1 {
		t.Fatalf("Content length = %d, want 1", len(msg.Content))
	}

	if msg.Content[0].Text != "file contents here" {
		t.Errorf("Content[0].Text = %q, want %q", msg.Content[0].Text, "file contents here")
	}
}

func TestNewToolMessageError(t *testing.T) {
	msg := NewToolMessage("call_456", "bash", "Error: command failed", true)

	if !msg.IsError {
		t.Error("IsError should be true")
	}
}

func TestThinkingLevels(t *testing.T) {
	tests := []struct {
		level    ThinkingLevel
		expected string
	}{
		{ThinkingOff, "off"},
		{ThinkingMinimal, "minimal"},
		{ThinkingLow, "low"},
		{ThinkingMedium, "medium"},
		{ThinkingHigh, "high"},
		{ThinkingXHigh, "xhigh"},
	}

	for _, tt := range tests {
		if string(tt.level) != tt.expected {
			t.Errorf("ThinkingLevel = %q, want %q", tt.level, tt.expected)
		}
	}
}

func TestToolDefinition(t *testing.T) {
	tool := Tool{
		Name:        "test_tool",
		Description: "A test tool",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"arg1": map[string]any{"type": "string"},
			},
		},
		Execute: func(ctx ToolContext) (*ToolResult, error) {
			return &ToolResult{Content: "executed"}, nil
		},
	}

	if tool.Name != "test_tool" {
		t.Errorf("Name = %q, want %q", tool.Name, "test_tool")
	}

	if tool.Description != "A test tool" {
		t.Errorf("Description = %q, want %q", tool.Description, "A test tool")
	}
}

func TestToolExecution(t *testing.T) {
	tool := Tool{
		Name:        "greet",
		Description: "Greet someone",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
		},
		Execute: func(ctx ToolContext) (*ToolResult, error) {
			name, ok := ctx.Args["name"].(string)
			if !ok {
				return nil, nil
			}

			return &ToolResult{Content: "Hello, " + name + "!"}, nil
		},
	}

	result, err := tool.Execute(ToolContext{
		ToolCallID: "call_1",
		Name:       "greet",
		Args:       map[string]any{"name": "Alice"},
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if result == nil {
		t.Fatal("Result should not be nil")
	}

	if result.Content != "Hello, Alice!" {
		t.Errorf("Content = %q, want %q", result.Content, "Hello, Alice!")
	}

	if result.IsError {
		t.Error("IsError should be false")
	}
}

func TestToolExecutionWithError(t *testing.T) {
	tool := Tool{
		Name:        "fail",
		Description: "Always fails",
		Parameters:  map[string]any{},
		Execute: func(ctx ToolContext) (*ToolResult, error) {
			return nil, context.DeadlineExceeded
		},
	}

	result, err := tool.Execute(ToolContext{
		ToolCallID: "call_2",
		Name:       "fail",
		Args:       map[string]any{},
	})

	// Error is returned, result may be nil
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want %v", err, context.DeadlineExceeded)
	}

	_ = result // May be nil or partial
}

func TestToolContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	toolCtx := ToolContext{
		Context:    ctx,
		ToolCallID: "call_789",
		Name:       "test",
		Args:       map[string]any{"key": "value"},
	}

	if toolCtx.ToolCallID != "call_789" {
		t.Errorf("ToolCallID = %q, want %q", toolCtx.ToolCallID, "call_789")
	}

	if toolCtx.Name != "test" {
		t.Errorf("Name = %q, want %q", toolCtx.Name, "test")
	}

	if toolCtx.Args["key"] != "value" {
		t.Errorf("Args[key] = %v, want %q", toolCtx.Args["key"], "value")
	}

	// Test context cancellation.
	select {
	case <-toolCtx.Context.Done():
		t.Error("context should not be canceled yet")
	default:
		// OK
	}

	cancel()
	<-toolCtx.Context.Done()
}

func TestStateClone(t *testing.T) {
	state := &State{
		Model: &ai.Model{
			ID:    "test-model",
			Input: []ai.InputType{ai.InputText},
		},
		SystemPrompt: "You are a test assistant.",
		Messages: []*Message{
			NewUserMessage("Hello"),
			NewAssistantMessage([]ai.ContentBlock{{Type: ai.ContentBlockText, Text: "Hi!"}}),
		},
		ThinkingLevel: ThinkingMedium,
		PendingToolIDs: map[string]struct{}{
			"call_1": {},
		},
		Error:        context.DeadlineExceeded,
		ErrorMessage: context.DeadlineExceeded.Error(),
	}

	clone := state.Clone()

	// Verify values match
	if clone.Model.ID != state.Model.ID {
		t.Errorf("Model.ID = %q, want %q", clone.Model.ID, state.Model.ID)
	}

	if clone.SystemPrompt != state.SystemPrompt {
		t.Errorf("SystemPrompt = %q, want %q", clone.SystemPrompt, state.SystemPrompt)
	}

	if len(clone.Messages) != len(state.Messages) {
		t.Errorf("Messages length = %d, want %d", len(clone.Messages), len(state.Messages))
	}

	if clone.ThinkingLevel != state.ThinkingLevel {
		t.Errorf("ThinkingLevel = %q, want %q", clone.ThinkingLevel, state.ThinkingLevel)
	}

	if !errors.Is(clone.Error, state.Error) {
		t.Errorf("Error = %v, want %v", clone.Error, state.Error)
	}

	if clone.ErrorMessage != state.ErrorMessage {
		t.Errorf("ErrorMessage = %q, want %q", clone.ErrorMessage, state.ErrorMessage)
	}

	if _, ok := clone.PendingToolIDs["call_1"]; !ok {
		t.Fatal("cloned PendingToolIDs missing call_1")
	}

	// Verify it's a deep clone (modifying clone doesn't affect original)
	clone.Messages = append(clone.Messages, NewUserMessage("New"))
	if len(state.Messages) == len(clone.Messages) {
		t.Error("Clone should be independent of original")
	}

	delete(clone.PendingToolIDs, "call_1")
	if _, ok := state.PendingToolIDs["call_1"]; !ok {
		t.Error("PendingToolIDs clone should be independent of original")
	}
}

func TestEventTypes(t *testing.T) {
	// Verify all event types implement the interface
	var (
		_ Event = MessageUpdate{Message: NewAssistantMessage(nil), AssistantMessageEvent: ai.TextDelta{}}
		_ Event = TurnStart{}
		_ Event = TurnEnd{Message: NewUserMessage("test")}
		_ Event = ToolStart{ToolCallID: "t1", Name: "test", Args: nil}
		_ Event = ToolEnd{ToolCallID: "t1", Result: &ToolResult{Content: "ok"}}
	)
}

func TestDefaultConvertToLlmSkipsExcludedMessages(t *testing.T) {
	included := &Message{Role: RoleUser, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "keep"}}, Timestamp: time.Now()}
	excluded := &Message{Role: RoleTool, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "skip"}}, ExcludeFromContext: true, Timestamp: time.Now()}

	got, err := DefaultConvertToLlm(context.Background(), []*Message{included, excluded})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("converted = %#v, want one message", got)
	}
	blocks, ok := got[0].Content.([]ai.ContentBlock)
	if !ok || len(blocks) != 1 || blocks[0].Text != "keep" {
		t.Fatalf("converted = %#v, want only keep", got)
	}
}

func TestBuiltInTools(t *testing.T) {
	tools := BuiltInTools()

	if len(tools) != 7 {
		t.Errorf("BuiltInTools count = %d, want 7", len(tools))
	}

	names := make(map[string]bool)
	for _, tool := range tools {
		names[tool.Name] = true
	}

	if !names["read"] {
		t.Error("read tool should be in built-in tools")
	}

	if !names["write"] {
		t.Error("write tool should be in built-in tools")
	}

	if !names["bash"] {
		t.Error("bash tool should be in built-in tools")
	}

	if !names["edit"] {
		t.Error("edit tool should be in built-in tools")
	}

	if !names["grep"] {
		t.Error("grep tool should be in built-in tools")
	}

	if !names["find"] {
		t.Error("find tool should be in built-in tools")
	}

	if !names["ls"] {
		t.Error("ls tool should be in built-in tools")
	}
}

func TestReadToolReadsDirectoryListing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := ReadTool(ToolConfig{}).Execute(ToolContext{Args: map[string]any{"path": dir}})
	if err != nil {
		t.Fatalf("ReadTool.Execute: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.Content)
	}
	if result.Content == "" {
		t.Fatal("expected directory listing content")
	}
}

func TestBashToolHonorsTimeout(t *testing.T) {
	result, err := BashTool(ToolConfig{}).Execute(ToolContext{
		Args: map[string]any{
			"command": "sleep 1",
			"timeout": float64(0.01),
		},
	})
	if err != nil {
		t.Fatalf("BashTool.Execute: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected timeout to report error")
	}
	details := detailsMap(t, result.Details)
	if details == nil {
		t.Fatal("expected timeout details")
	}
	if _, ok := details["cancel_reason"]; !ok {
		t.Fatal("expected cancel_reason detail for timeout")
	}
}

func TestReadToolParameters(t *testing.T) {
	tool := ReadTool(ToolConfig{})

	if tool.Name != "read" {
		t.Errorf("Name = %q, want %q", tool.Name, "read")
	}

	params, ok := tool.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("Parameters should have 'properties'")
	}

	if _, ok := params["path"]; !ok {
		t.Error("Should have 'path' parameter")
	}
}

func TestWriteToolParameters(t *testing.T) {
	tool := WriteTool(ToolConfig{})

	if tool.Name != "write" {
		t.Errorf("Name = %q, want %q", tool.Name, "write")
	}

	params, ok := tool.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("Parameters should have 'properties'")
	}

	if _, ok := params["path"]; !ok {
		t.Error("Should have 'path' parameter")
	}

	if _, ok := params["content"]; !ok {
		t.Error("Should have 'content' parameter")
	}
}

func TestBashToolParameters(t *testing.T) {
	tool := BashTool(ToolConfig{})

	if tool.Name != "bash" {
		t.Errorf("Name = %q, want %q", tool.Name, "bash")
	}

	params, ok := tool.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("Parameters should have 'properties'")
	}

	if _, ok := params["command"]; !ok {
		t.Error("Should have 'command' parameter")
	}
}

func TestEditToolParameters(t *testing.T) {
	tool := EditTool(ToolConfig{})

	if tool.Name != "edit" {
		t.Errorf("Name = %q, want %q", tool.Name, "edit")
	}

	params, ok := tool.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("Parameters should have 'properties'")
	}

	if _, ok := params["path"]; !ok {
		t.Error("Should have 'path' parameter")
	}
	if _, ok := params["old_string"]; !ok {
		t.Error("Should have 'old_string' parameter")
	}
	if _, ok := params["new_string"]; !ok {
		t.Error("Should have 'new_string' parameter")
	}
}

func TestGrepToolParameters(t *testing.T) {
	tool := GrepTool(ToolConfig{})

	if tool.Name != "grep" {
		t.Errorf("Name = %q, want %q", tool.Name, "grep")
	}

	params, ok := tool.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("Parameters should have 'properties'")
	}

	if _, ok := params["path"]; !ok {
		t.Error("Should have 'path' parameter")
	}
	if _, ok := params["pattern"]; !ok {
		t.Error("Should have 'pattern' parameter")
	}
}

func TestFindToolParameters(t *testing.T) {
	tool := FindTool(ToolConfig{})

	if tool.Name != "find" {
		t.Errorf("Name = %q, want %q", tool.Name, "find")
	}

	params, ok := tool.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("Parameters should have 'properties'")
	}

	if _, ok := params["path"]; !ok {
		t.Error("Should have 'path' parameter")
	}
	if _, ok := params["query"]; !ok {
		t.Error("Should have 'query' parameter")
	}
}

func TestLSToolParameters(t *testing.T) {
	tool := LSTool(ToolConfig{})

	if tool.Name != "ls" {
		t.Errorf("Name = %q, want %q", tool.Name, "ls")
	}

	params, ok := tool.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("Parameters should have 'properties'")
	}

	if _, ok := params["path"]; !ok {
		t.Error("Should have 'path' parameter")
	}
}

func TestToolResult(t *testing.T) {
	result := &ToolResult{
		Content: "test output",
		IsError: false,
		Details: map[string]any{"key": "value"},
	}

	if result.Content != "test output" {
		t.Errorf("Content = %q, want %q", result.Content, "test output")
	}

	if result.IsError {
		t.Error("IsError should be false")
	}

	details := detailsMap(t, result.Details)
	if details["key"] != "value" {
		t.Errorf("Details[key] = %v, want %q", details["key"], "value")
	}
}

func TestToolResultNormalizedContent(t *testing.T) {
	result := &ToolResult{Content: "hello"}
	content := result.NormalizedContent()
	if len(content) != 1 {
		t.Fatalf("len(content) = %d, want 1", len(content))
	}
	if content[0].Text != "hello" {
		t.Fatalf("content[0].Text = %q, want hello", content[0].Text)
	}

	structured := &ToolResult{
		ContentBlocks: []ai.ContentBlock{{Type: ai.ContentBlockImage, Data: "x", MimeType: "image/png"}},
	}
	content = structured.NormalizedContent()
	if len(content) != 1 || content[0].Type != ai.ContentBlockImage {
		t.Fatal("expected structured content blocks to be preserved")
	}
}

func TestSlicesClone(t *testing.T) {
	original := []*Message{
		NewUserMessage("one"),
		NewUserMessage("two"),
	}

	clone := slicesClone(original)

	if len(clone) != len(original) {
		t.Errorf("Clone length = %d, want %d", len(clone), len(original))
	}

	// Verify independence
	clone[0] = NewUserMessage("modified")

	if original[0].Content[0].Text == "modified" {
		t.Error("Clone should be independent")
	}
}

func TestSlicesCloneNil(t *testing.T) {
	var nilSlice []*Message

	clone := slicesClone(nilSlice)

	if clone != nil {
		t.Error("Cloning nil slice should return nil")
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := Config{}

	if cfg.Model != nil {
		t.Error("Model should be nil by default")
	}

	if cfg.SystemPrompt != "" {
		t.Error("SystemPrompt should be empty by default")
	}

	if cfg.ThinkingLevel != "" {
		t.Error("ThinkingLevel should be empty by default")
	}
}

func TestStreamHandler(t *testing.T) {
	var received Event

	handler := StreamHandler(func(event Event) {
		received = event
	})

	handler(TurnStart{})

	if received == nil {
		t.Error("Handler should receive events")
	}
}

func TestMessageTimestamp(t *testing.T) {
	before := time.Now()
	msg := NewUserMessage("test")
	after := time.Now()

	if msg.Timestamp.Before(before) || msg.Timestamp.After(after) {
		t.Error("Timestamp should be set to current time")
	}
}

func TestToolExecutionCompatibilityAliases(t *testing.T) {
	// Verify that ToolExecution* types are identical to their non-compat counterparts
	start := ToolStart{ToolCallID: "test", Name: "tool", Args: nil}
	compatStart := ToolExecutionStart(start)
	if compatStart.ToolCallID != "test" {
		t.Error("ToolExecutionStart alias should work")
	}

	update := ToolUpdate{ToolCallID: "test", Name: "tool", Args: nil}
	compatUpdate := ToolExecutionUpdate(update)
	if compatUpdate.ToolCallID != "test" {
		t.Error("ToolExecutionUpdate alias should work")
	}

	end := ToolEnd{ToolCallID: "test", Result: &ToolResult{Content: "ok"}}
	compatEnd := ToolExecutionEnd(end)
	if compatEnd.ToolCallID != "test" {
		t.Error("ToolExecutionEnd alias should work")
	}
}

func TestErrorMessageSemantics(t *testing.T) {
	// Test that State.Error and State.ErrorMessage are populated correctly
	state := State{
		Error:        context.DeadlineExceeded,
		ErrorMessage: context.DeadlineExceeded.Error(),
	}

	if state.Error == nil {
		t.Error("Error should be set")
	}
	if state.ErrorMessage == "" {
		t.Error("ErrorMessage should be set")
	}
	if !errors.Is(state.Error, context.DeadlineExceeded) {
		t.Error("Error should be context.DeadlineExceeded")
	}
}

func TestPendingToolCallsReturnsSortedList(t *testing.T) {
	state := State{
		PendingToolIDs: map[string]struct{}{
			"call_z": {},
			"call_a": {},
			"call_m": {},
		},
	}

	calls := state.PendingToolCalls()
	if len(calls) != 3 {
		t.Fatalf("len(PendingToolCalls()) = %d, want 3", len(calls))
	}

	// Verify slice is sorted (map iteration is undefined order)
	sorted := make([]string, len(calls))
	copy(sorted, calls)
	slices.Sort(sorted)

	for i, expected := range sorted {
		if calls[i] != expected {
			t.Errorf("PendingToolCalls()[%d] = %q, want %q (sorted order)", i, calls[i], expected)
		}
	}
}

func TestPendingToolCallsReturnsNilWhenEmpty(t *testing.T) {
	state := State{PendingToolIDs: nil}
	if calls := state.PendingToolCalls(); calls != nil {
		t.Error("PendingToolCalls() should return nil when PendingToolIDs is nil")
	}

	state.PendingToolIDs = map[string]struct{}{}
	if calls := state.PendingToolCalls(); calls != nil {
		t.Error("PendingToolCalls() should return nil when PendingToolIDs is empty")
	}
}

// stubGrepOps is a test double for GrepOperations that returns a canned
// result without touching the filesystem. Used to verify the seam is
// actually invoked by GrepTool.
type stubGrepOps struct {
	called  bool
	pattern string
	path    string
	glob    string
	content string
	err     error
}

func (s *stubGrepOps) Grep(pattern, path, glob string, _ bool, _, _ int) (*ToolResult, error) {
	s.called = true
	s.pattern = pattern
	s.path = path
	s.glob = glob
	if s.err != nil {
		return nil, s.err
	}
	return &ToolResult{Content: s.content}, nil
}

func TestGrepToolUsesInjectedGrepOps(t *testing.T) {
	stub := &stubGrepOps{content: "stubbed grep output"}
	cfg := ToolConfig{GrepOps: stub}
	tool := GrepTool(cfg)

	result, err := tool.Execute(ToolContext{
		Args: map[string]any{
			"pattern": "needle",
			"path":    "/some/path",
			"glob":    "*.go",
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !stub.called {
		t.Fatal("injected GrepOps was not called")
	}
	if stub.pattern != "needle" || stub.path != "/some/path" || stub.glob != "*.go" {
		t.Errorf("ops received pattern=%q path=%q glob=%q, want needle /some/path *.go",
			stub.pattern, stub.path, stub.glob)
	}
	if result.Content != "stubbed grep output" {
		t.Errorf("Execute result = %q, want %q (from stub)", result.Content, "stubbed grep output")
	}
}

// stubFindOps is a test double for FindOperations.
type stubFindOps struct {
	called  bool
	matches []string
	err     error
}

func (s *stubFindOps) Find(_, _ string, _ string, _ int, _, _ bool, _ int) ([]string, error) {
	s.called = true
	if s.err != nil {
		return nil, s.err
	}
	return s.matches, nil
}

func TestFindToolUsesInjectedFindOps(t *testing.T) {
	stub := &stubFindOps{matches: []string{"/a/b/c.txt"}}
	cfg := ToolConfig{FindOps: stub}
	tool := FindTool(cfg)

	result, err := tool.Execute(ToolContext{Args: map[string]any{"query": "c.txt"}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !stub.called {
		t.Fatal("injected FindOps was not called")
	}
	if result.Content != "/a/b/c.txt" {
		t.Errorf("Execute result = %q, want /a/b/c.txt", result.Content)
	}
}

// stubLsOps is a test double for LsOperations.
type stubLsOps struct {
	called  bool
	lsErr   error
	statErr error
}

func (s *stubLsOps) Ls(_ string) ([]os.DirEntry, error) {
	s.called = true
	if s.lsErr != nil {
		return nil, s.lsErr
	}
	return nil, nil
}

func (s *stubLsOps) Stat(_ string) (fs.FileInfo, error) {
	return nil, s.statErr
}

func TestLSToolUsesInjectedLsOps(t *testing.T) {
	stub := &stubLsOps{}
	cfg := ToolConfig{LsOps: stub}
	tool := LSTool(cfg)

	_, err := tool.Execute(ToolContext{Args: map[string]any{"path": "/tmp"}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !stub.called {
		t.Fatal("injected LsOps.Ls was not called")
	}
}

func TestToolResultNormalizedContentPreservesExplicitEmptyBlocks(t *testing.T) {
	result := &ToolResult{Content: "stale", ContentBlocks: []ai.ContentBlock{}}
	if got := result.NormalizedContent(); got == nil || len(got) != 0 {
		t.Fatalf("explicit empty content replaced: %+v", got)
	}
}
