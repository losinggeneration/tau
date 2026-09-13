package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
	claude "github.com/losinggeneration/claude-agent-sdk-go"
)

type claudeAgentRunner func(ctx context.Context, prompt any, opts claude.Options) (<-chan claude.Message, <-chan error)

type claudeAgentClient struct {
	runner claudeAgentRunner
	state  *claudeAgentState
}

type claudeAgentState struct {
	mu       sync.Mutex
	sessions map[string]*ClaudeAgentSessionState
}

// ClaudeAgentSessionState tracks Claude Code's persisted session separately from tau's session ID.
type ClaudeAgentSessionState struct {
	SessionID           string
	Generation          int
	LastMessageCount    int
	CompactionSignature string
	ModelID             string
}

var defaultClaudeAgentState = &claudeAgentState{sessions: make(map[string]*ClaudeAgentSessionState)}

func newClaudeAgentClient(_ *Model) (Client, error) {
	return &claudeAgentClient{runner: claude.Query, state: defaultClaudeAgentState}, nil
}

func (c *claudeAgentClient) Complete(ctx context.Context, model *Model, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	return completeViaStream(ctx, c, model, prompt, opts...)
}

func (c *claudeAgentClient) Stream(ctx context.Context, model *Model, prompt *Context, handler EventHandler, opts ...Option) error {
	if model == nil {
		return fmt.Errorf("claude agent stream: model is nil")
	}
	if handler == nil {
		return fmt.Errorf("claude agent stream: handler is nil")
	}

	o := applyOptions(opts)
	sdkPrompt, sdkOptions, requestState := c.buildRequest(model, prompt, o)
	output := AssistantMessage{
		Role:       "assistant",
		API:        model.API,
		Provider:   model.Provider,
		Model:      model.ID,
		Content:    []ContentBlock{},
		StopReason: StopReasonStop,
		Timestamp:  timestamp(),
	}
	handler(ctx, Start{Partial: output})

	runner := c.runner
	if runner == nil {
		runner = claude.Query
	}
	messages, errs := runner(ctx, sdkPrompt, sdkOptions)

	for messages != nil || errs != nil {
		select {
		case msg, ok := <-messages:
			if !ok {
				messages = nil
				continue
			}
			if done := c.handleMessage(ctx, msg, requestState, &output, handler); done {
				return nil
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err == nil {
				continue
			}
			reason := StopReasonError
			if ctx.Err() != nil {
				reason = StopReasonAborted
			}
			emitTerminalStreamError(ctx, handler, output, err, reason)
			return nil
		case <-ctx.Done():
			emitTerminalStreamError(ctx, handler, output, ctx.Err(), StopReasonAborted)
			return nil
		}
	}

	output.StopReason = StopReasonStop
	handler(ctx, Done{Reason: output.StopReason, Message: output})
	return nil
}

type claudeAgentRequestState struct {
	key                 string
	messageCount        int
	compactionSignature string
}

func (c *claudeAgentClient) handleMessage(ctx context.Context, msg claude.Message, requestState claudeAgentRequestState, output *AssistantMessage, handler EventHandler) bool {
	switch m := msg.(type) {
	case claude.AssistantMessage:
		for _, block := range m.Content {
			switch b := block.(type) {
			case claude.TextBlock:
				emitClaudeAgentText(ctx, output, handler, b.Text)
			case claude.ThinkingBlock:
				emitClaudeAgentThinking(ctx, output, handler, b.Thinking, b.Signature)
			case claude.ToolUseBlock:
				emitClaudeAgentToolCall(ctx, output, handler, b.ID, claudeAgentToolName(b.Name), b.Input)
			}
		}
	case claude.ResultMessage:
		if m.SessionID != "" {
			c.storeReturnedSession(requestState, m.SessionID)
		}
		// The CLI runs one model call plus its tool executions per turn (tau
		// sets MaxTurns: 1) and reports an exhausted budget as a terminal error
		// even though the call itself succeeded. tau executes the pending tool
		// calls itself and resumes the session, so treat it as a normal stop.
		isError := m.IsError && !claudeAgentReachedMaxTurns(m)
		output.StopReason = claudeAgentStopReason(m.StopReason, isError)
		output.Usage = claudeAgentUsage(m.Usage)
		output.ResponseID = m.UUID
		if isError && output.ErrorMessage == "" {
			output.ErrorMessage = strings.Join(m.Errors, "; ")
		}
		handler(ctx, Done{Reason: output.StopReason, Message: *output})
		return true
	}

	return false
}

func claudeAgentReachedMaxTurns(m claude.ResultMessage) bool {
	return m.Subtype == "error_max_turns" || m.TerminalReason == "max_turns"
}

func (c *claudeAgentClient) buildRequest(model *Model, prompt *Context, opts *options) (any, claude.Options, claudeAgentRequestState) {
	sdkOptions := claudeAgentBuildOptions(model, prompt, opts)
	sessionState := c.sessionState(opts, prompt, model)
	requestState := claudeAgentRequestState{messageCount: len(claudeAgentMessages(prompt)), compactionSignature: claudeAgentCompactionSignature(prompt)}
	if opts == nil || opts.sessionID == "" || sessionState == nil {
		return claudeAgentPromptContent(claudeAgentMessages(prompt)), sdkOptions, requestState
	}

	requestState.key = opts.sessionID
	if sessionState.SessionID != "" {
		sdkOptions.Resume = sessionState.SessionID
		return claudeAgentPromptContent(claudeAgentResumeMessages(claudeAgentNewMessages(prompt, sessionState.LastMessageCount))), sdkOptions, requestState
	}

	sdkOptions.SessionID = uuid.NewString()
	return claudeAgentPromptContent(claudeAgentMessages(prompt)), sdkOptions, requestState
}

func (c *claudeAgentClient) sessionState(opts *options, prompt *Context, model *Model) *ClaudeAgentSessionState {
	if opts == nil || opts.sessionID == "" {
		return nil
	}
	state := c.state
	if state == nil {
		state = defaultClaudeAgentState
		c.state = state
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	if state.sessions == nil {
		state.sessions = make(map[string]*ClaudeAgentSessionState)
	}
	session := state.sessions[opts.sessionID]
	if session == nil {
		session = &ClaudeAgentSessionState{}
		state.sessions[opts.sessionID] = session
	}
	signature := claudeAgentCompactionSignature(prompt)
	modelID := ""
	if model != nil {
		modelID = model.ID
	}
	if signature != session.CompactionSignature || (session.ModelID != "" && modelID != "" && modelID != session.ModelID) {
		session.SessionID = ""
		session.LastMessageCount = 0
		session.Generation++
		session.CompactionSignature = signature
	}
	if modelID != "" {
		session.ModelID = modelID
	}
	return session
}

func (c *claudeAgentClient) storeReturnedSession(requestState claudeAgentRequestState, sessionID string) {
	if requestState.key == "" {
		return
	}
	state := c.state
	if state == nil {
		state = defaultClaudeAgentState
	}
	state.mu.Lock()
	defer state.mu.Unlock()

	if state.sessions == nil {
		state.sessions = make(map[string]*ClaudeAgentSessionState)
	}
	session := state.sessions[requestState.key]
	if session == nil {
		session = &ClaudeAgentSessionState{}
		state.sessions[requestState.key] = session
	}
	session.SessionID = sessionID
	session.LastMessageCount = requestState.messageCount
	session.CompactionSignature = requestState.compactionSignature
}

func claudeAgentMessages(prompt *Context) []Message {
	if prompt == nil {
		return nil
	}
	return prompt.Messages
}

// claudeAgentPromptContent builds the CLI streaming-input prompt: a plain
// string unless some message carries an image, in which case the conversation
// becomes content blocks so the images reach the model.
func claudeAgentPromptContent(messages []Message) any {
	if claudeAgentHasImage(messages) {
		return claudeAgentBuildPromptBlocks(messages)
	}
	return claudeAgentBuildPromptFromMessages(messages)
}

func claudeAgentHasImage(messages []Message) bool {
	for _, msg := range messages {
		for _, block := range msg.ContentBlocks() {
			if block.Type == ContentBlockImage {
				return true
			}
		}
	}
	return false
}

func claudeAgentMessagePrefix(msg Message) string {
	switch msg.Role {
	case "user":
		return "User: "
	case "assistant":
		return "Assistant: "
	case messageRoleTool, messageRoleToolResult:
		name := msg.ToolName
		if name == "" {
			name = "unknown"
		}
		return "Tool result " + name + ": "
	}
	return ""
}

// claudeAgentBuildPromptBlocks mirrors claudeAgentBuildPromptFromMessages but
// keeps images as base64 image blocks instead of dropping them.
func claudeAgentBuildPromptBlocks(messages []Message) []map[string]any {
	blocks := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		prefix := claudeAgentMessagePrefix(msg)
		if prefix == "" {
			continue
		}
		if text, ok := msg.ContentString(); ok {
			if strings.TrimSpace(text) != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": prefix + text})
			}
			continue
		}
		var texts []string
		flush := func() {
			if len(texts) == 0 {
				return
			}
			if s := strings.Join(texts, "\n"); strings.TrimSpace(s) != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": prefix + s})
			}
			texts = nil
		}
		for _, block := range msg.ContentBlocks() {
			switch block.Type {
			case ContentBlockText, ContentBlockToolResult:
				if strings.TrimSpace(block.Text) != "" {
					texts = append(texts, block.Text)
				}
			case ContentBlockImage:
				if block.Data == "" {
					continue
				}
				flush()
				blocks = append(blocks, map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": block.MimeType,
						"data":       block.Data,
					},
				})
			}
		}
		flush()
	}
	return blocks
}

func claudeAgentBuildPromptFromMessages(messages []Message) string {
	parts := make([]string, 0, len(messages))
	for _, msg := range messages {
		text := claudeAgentMessageText(msg)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if prefix := claudeAgentMessagePrefix(msg); prefix != "" {
			parts = append(parts, prefix+text)
		}
	}

	return strings.Join(parts, "\n\n")
}

func claudeAgentNewMessages(prompt *Context, previousCount int) []Message {
	messages := claudeAgentMessages(prompt)
	if previousCount < 0 || previousCount > len(messages) {
		return messages
	}
	return messages[previousCount:]
}

// claudeAgentResumeMessages drops assistant messages: the resumed CLI session
// already contains the responses it produced, so tau only delivers what the
// CLI has not seen (tool results, steering, follow-ups).
func claudeAgentResumeMessages(messages []Message) []Message {
	kept := make([]Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Role == "assistant" {
			continue
		}
		kept = append(kept, msg)
	}
	return kept
}

func claudeAgentCompactionSignature(prompt *Context) string {
	if prompt == nil {
		return ""
	}
	parts := make([]string, 0, 1)
	for _, part := range strings.Split(prompt.SystemPrompt, "\n\n") {
		trimmed := strings.TrimSpace(part)
		if strings.HasPrefix(trimmed, "Session Summary:") {
			parts = append(parts, trimmed)
		}
	}
	return strings.Join(parts, "\n\n")
}

func claudeAgentMessageText(msg Message) string {
	if text, ok := msg.ContentString(); ok {
		return text
	}

	blocks := msg.ContentBlocks()
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case ContentBlockText:
			if strings.TrimSpace(block.Text) != "" {
				parts = append(parts, block.Text)
			}
		case ContentBlockImage:
			mimeType := block.MimeType
			if mimeType == "" {
				mimeType = "unknown"
			}
			parts = append(parts, "[image omitted: "+mimeType+"]")
		case ContentBlockToolResult:
			if strings.TrimSpace(block.Text) != "" {
				parts = append(parts, block.Text)
			}
		}
	}

	return strings.Join(parts, "\n")
}

func claudeAgentBuildOptions(model *Model, prompt *Context, opts *options) claude.Options {
	sdkOptions := claude.Options{
		Model:    model.ID,
		MaxTurns: 1,
		Tools:    []string{},
	}
	if prompt != nil {
		sdkOptions.SystemPrompt = prompt.SystemPrompt
		if len(prompt.Tools) > 0 {
			configureClaudeAgentTools(&sdkOptions, prompt.Tools)
		}
	}
	if opts == nil {
		return sdkOptions
	}
	if effort := claudeAgentEffort(opts); effort != "" {
		sdkOptions.Effort = effort
	}

	return sdkOptions
}

func configureClaudeAgentTools(sdkOptions *claude.Options, tools []Tool) {
	server := claude.NewServer("tau", "0.0.0")
	allowed := make([]string, 0, len(tools))
	for _, tool := range tools {
		if !claudeAgentValidToolName(tool.Name) {
			continue
		}
		schema, ok := claudeAgentToolSchema(tool.Parameters)
		if !ok {
			continue
		}
		name := tool.Name
		description := tool.Description
		sdkTool, err := claude.NewToolFromSchema(name, description, schema, func(_ context.Context, _ map[string]any) (claude.ToolResult, error) {
			// The CLI executes MCP tools inside its own turn, but tau runs
			// every tool through its agent loop (plugins, UI events, transcript)
			// and resumes the session with the real results. Acknowledge without
			// executing so tools never run twice.
			return claude.TextResult("Tool call accepted; it executes in the host session and its result arrives in the next message."), nil
		})
		if err != nil {
			continue
		}
		if err := server.Register(sdkTool); err != nil {
			continue
		}
		allowed = append(allowed, "mcp__tau__"+tool.Name)
	}
	if len(allowed) == 0 {
		return
	}

	sdkOptions.Tools = allowed
	sdkOptions.MCPServers = map[string]any{"tau": claude.CreateSdkMCPServer("tau", server)}
	sdkOptions.AllowedTools = allowed
}

func claudeAgentValidToolName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func claudeAgentToolSchema(raw json.RawMessage) (map[string]any, bool) {
	if len(raw) == 0 {
		return map[string]any{"type": "object", "properties": map[string]any{}}, true
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil || schema == nil {
		return nil, false
	}
	if schema["type"] != "object" {
		return nil, false
	}
	return schema, true
}

func claudeAgentEffort(opts *options) string {
	if opts.effort != "" {
		return opts.effort
	}

	switch opts.reasoning {
	case ReasoningLow:
		return "low"
	case ReasoningMedium:
		return "medium"
	case ReasoningHigh:
		return "high"
	case ReasoningXHigh:
		return "xhigh"
	default:
		return ""
	}
}

func emitClaudeAgentText(ctx context.Context, output *AssistantMessage, handler EventHandler, text string) {
	if text == "" {
		return
	}
	index := len(output.Content)
	output.Content = append(output.Content, ContentBlock{Type: ContentBlockText})
	handler(ctx, TextStart{ContentIndex: index, Partial: *output})
	output.Content[index].Text = text
	handler(ctx, TextDelta{ContentIndex: index, Delta: text, Partial: *output})
	handler(ctx, TextEnd{ContentIndex: index, Content: text, Partial: *output})
}

func emitClaudeAgentThinking(ctx context.Context, output *AssistantMessage, handler EventHandler, thinking, signature string) {
	if thinking == "" {
		return
	}
	index := len(output.Content)
	output.Content = append(output.Content, ContentBlock{Type: ContentBlockThinking})
	handler(ctx, ThinkingStart{ContentIndex: index, Partial: *output})
	output.Content[index].Thinking = thinking
	output.Content[index].ThoughtSignature = signature
	handler(ctx, ThinkingDelta{ContentIndex: index, Delta: thinking, Partial: *output})
	handler(ctx, ThinkingEnd{ContentIndex: index, Content: thinking, ContentSignature: signature, Partial: *output})
}

// The CLI advertises tools registered in the tau MCP server as
// mcp__tau__<name>, while tau's tool registry and transcript use the plain
// name. Strip the prefix so the agent loop can resolve the tool.
func claudeAgentToolName(name string) string {
	return strings.TrimPrefix(name, "mcp__tau__")
}

func emitClaudeAgentToolCall(ctx context.Context, output *AssistantMessage, handler EventHandler, id, name string, input map[string]any) {
	arguments, err := json.Marshal(input)
	if err != nil {
		arguments = json.RawMessage(`{}`)
	}
	index := len(output.Content)
	output.Content = append(output.Content, ContentBlock{Type: ContentBlockToolCall, ID: id, Name: name})
	handler(ctx, ToolCallStart{ContentIndex: index, Partial: *output})
	output.Content[index].Arguments = arguments
	handler(ctx, ToolCallDelta{ContentIndex: index, Delta: string(arguments), Partial: *output})
	handler(ctx, ToolCallEnd{ContentIndex: index, ToolCall: ToolCall{ID: id, Name: name, Arguments: arguments}, Partial: *output})
}

func claudeAgentStopReason(reason string, isError bool) StopReason {
	if isError {
		return StopReasonError
	}

	switch reason {
	case "", "end_turn":
		return StopReasonStop
	case "max_tokens":
		return StopReasonLength
	case "tool_use":
		return StopReasonToolUse
	default:
		return StopReasonStop
	}
}

func claudeAgentUsage(raw map[string]any) Usage {
	usage := Usage{
		Input:      intFromAny(raw["input_tokens"]),
		Output:     intFromAny(raw["output_tokens"]),
		CacheRead:  intFromAny(raw["cache_read_input_tokens"]),
		CacheWrite: intFromAny(raw["cache_creation_input_tokens"]),
	}
	return normalizeUsage(usage)
}

func intFromAny(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0
		}
		return int(i)
	default:
		return 0
	}
}
