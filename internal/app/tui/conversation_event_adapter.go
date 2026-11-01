package tui

import (
	"encoding/json"
	"strings"

	"github.com/losinggeneration/tau/ai"

	"github.com/losinggeneration/tau/sdk"
)

type conversationEventAdapter struct {
	app *App

	finalizedAssistantMessages  map[string]struct{}
	finalizedUserMessages       map[string]struct{}
	finalizedAnonymousAssistant bool
	// lastErrorText dedupes the run-failure "error" event against the
	// error assistant message (message_end) that carries the same text.
	lastErrorText string
	// assistantEntry is the session entry of the last finished assistant
	// message; the tool executions it requested are tagged with it so the
	// conversation model can drop history at that message.
	assistantEntry string
}

func newConversationEventAdapter(app *App) *conversationEventAdapter {
	return &conversationEventAdapter{app: app}
}

func (a *conversationEventAdapter) beginRun() {
	if a == nil {
		return
	}
	a.finalizedAssistantMessages = nil
	a.finalizedUserMessages = nil
	a.finalizedAnonymousAssistant = false
	a.lastErrorText = ""
}

func (a *conversationEventAdapter) handle(ev sdk.Event) {
	if a == nil || a.app == nil || a.app.conv == nil {
		return
	}
	app := a.app
	switch ev.Type {
	case "message_start":
		role, _ := plString(ev.Payload, "role")
		switch role {
		case "assistant":
			// A new assistant message is starting. If the user has scrolled
			// away from the bottom, count it toward the "New messages" count
			// in the chat status line so the running count increments on the
			// same boundary the user perceives as "a new message arriving".
			app.noteNewMessage()
			messageID, _ := plString(ev.Payload, "message_id")
			a.finalizedAnonymousAssistant = false
			a.assistantEntry = ""
			app.clearUsageLimitWait()
			app.ensureAssistantMessage(messageID)
		case "user":
			// User message_start is a lifecycle marker; the committed row is
			// created from message_end so it has the canonical event payload.
		}
	case "text_delta":
		delta, _ := plString(ev.Payload, "delta")
		if delta != "" {
			app.resetEmptyAssistantResponses()
			app.conv.ClearThinking()
			app.ensureAssistantMessage(payloadMessageID(ev.Payload))
			app.conv.AppendAssistantText(delta)
		}
	case "message_update":
		text, _ := plString(ev.Payload, "text")
		if text == "" {
			text, _ = plString(ev.Payload, "content")
		}
		if text == "" {
			text, _ = plString(ev.Payload, "delta")
		}
		if text != "" {
			app.resetEmptyAssistantResponses()
			app.conv.ClearThinking()
			app.ensureAssistantMessage(payloadMessageID(ev.Payload))
			app.conv.SetAssistantText(text)
		}
	case "thinking_delta":
		if delta, _ := plString(ev.Payload, "delta"); delta != "" {
			app.resetEmptyAssistantResponses()
			app.conv.AddThinking(app.throbber.Frame())
		}
	case "tool_call":
		app.resetEmptyAssistantResponses()
		app.conv.ClearThinking()
		name, _ := plStringAny(ev.Payload, "name", "tool_name")
		args, _ := plStringAny(ev.Payload, "input", "arguments")
		toolCallID, _ := plStringAny(ev.Payload, "tool_call_id", "toolCallId")
		before := len(app.conv.committed)
		app.conv.AddToolCallWithID(toolCallID, name, args)
		if a.assistantEntry != "" && len(app.conv.committed) > before {
			app.conv.committed[len(app.conv.committed)-1].entryID = a.assistantEntry
		}
	case "tool_progress":
		app.resetEmptyAssistantResponses()
		app.conv.ClearThinking()
		toolCallID, _ := plStringAny(ev.Payload, "tool_call_id", "toolCallId")
		chunk, _ := plStringAny(ev.Payload, "chunk", "output", "result")
		app.conv.AddToolProgress(toolCallID, chunk)
	case "tool_result":
		app.resetEmptyAssistantResponses()
		app.conv.ClearThinking()
		output, _ := plStringAny(ev.Payload, "output", "result")
		isErr, _ := plBool(ev.Payload, "is_error")
		toolCallID, _ := plStringAny(ev.Payload, "tool_call_id", "toolCallId")
		details := payloadValue(ev.Payload, "details")
		excluded, _ := plBool(ev.Payload, "excludeFromContext")
		content, _ := payloadValue(ev.Payload, "content").([]ai.ContentBlock)
		structured, _ := payloadValue(ev.Payload, "structuredContent").(json.RawMessage)
		nested, _ := payloadValue(ev.Payload, "nestedCalls").([]ai.NestedToolCall)
		usage, _ := payloadValue(ev.Payload, "toolUsage").(*ai.Usage)
		app.conv.addToolResult(toolCallID, "", output, isErr, details, payloadDurationMs(ev.Payload), excluded, ToolExecutionComponent{Content: content, StructuredContent: structured, NestedCalls: nested, ToolUsage: usage})
	case "command_result":
		app.resetEmptyAssistantResponses()
		app.conv.ClearThinking()
		output, _ := plStringAny(ev.Payload, "output", "result")
		isErr, _ := plBool(ev.Payload, "is_error")
		if isErr {
			app.conv.AddError(output)
		} else if strings.TrimRight(output, "\n") != "" {
			app.conv.AddSystem(output)
		}
		if command, _ := plString(ev.Payload, "command"); command == "reload" {
			app.reapplySessionSettings()
			if !isErr {
				app.reloadKeybindings()
			}
		}
	case "tool_cancelled", "tool_canceled":
		app.resetEmptyAssistantResponses()
		app.conv.ClearThinking()
		toolCallID, _ := plStringAny(ev.Payload, "tool_call_id", "toolCallId")
		reason, _ := plStringAny(ev.Payload, "reason", "message", "output", "result")
		app.conv.CancelTool(toolCallID, reason)
	case "error":
		app.resetEmptyAssistantResponses()
		app.conv.ClearThinking()
		if msg, _ := plString(ev.Payload, "message"); msg != "" {
			a.showError(msg)
		}
	case "message_end":
		a.handleMessageEnd(ev)
	case "agent_end":
		a.handleAgentEnd()
	}
}

func (a *conversationEventAdapter) handleAgentEnd() {
	if a == nil || a.app == nil || a.app.conv == nil {
		return
	}
	a.app.conv.ClearThinking()
	if a.app.conv.IsStreaming() {
		a.app.conv.EndAssistantMessage()
	}
}

func (a *conversationEventAdapter) handleMessageEnd(ev sdk.Event) {
	app := a.app
	role, _ := plString(ev.Payload, "role")
	if role == "user" {
		messageID := payloadMessageID(ev.Payload)
		if messageID != "" {
			if _, seen := a.finalizedUserMessages[messageID]; seen {
				return
			}
		}
		content, _ := plStringAny(ev.Payload, "content", "text")
		content, images := app.displayForSubmittedPrompt(content)
		if strings.TrimRight(content, "\n") != "" || len(images) > 0 {
			// A user row is about to be committed. Count it if the user is
			// scrolled away — the message boundary is the same point the user
			// sees the new row appear.
			app.noteNewMessage()
			entryID, _ := plString(ev.Payload, "entry_id")
			app.conv.AddPersistedUserMessage(entryID, content, images)
		}
		if messageID != "" {
			if a.finalizedUserMessages == nil {
				a.finalizedUserMessages = make(map[string]struct{})
			}
			a.finalizedUserMessages[messageID] = struct{}{}
		}
		return
	}
	if role != "" && role != "assistant" {
		return
	}
	entryID, _ := plString(ev.Payload, "entry_id")
	if entryID == "" {
		a.finishAssistantMessage(ev)
		return
	}
	mark := app.conv.Revision()
	a.finishAssistantMessage(ev)
	app.conv.tagEntrySince(mark, entryID)
	a.assistantEntry = entryID
}

func (a *conversationEventAdapter) finishAssistantMessage(ev sdk.Event) {
	app := a.app
	messageID := payloadMessageID(ev.Payload)
	if messageID != "" {
		if _, seen := a.finalizedAssistantMessages[messageID]; seen {
			return
		}
	} else if a.finalizedAnonymousAssistant && !app.conv.IsStreaming() {
		return
	}
	// Error assistant messages (provider failures, usage-limit 429s) carry
	// their text in error_message with empty content; show that instead of
	// falling through to the empty-response counter below.
	if errMsg, _ := plString(ev.Payload, "error_message"); errMsg != "" {
		if content, _ := plStringAny(ev.Payload, "content", "text"); strings.TrimSpace(content) == "" {
			app.resetEmptyAssistantResponses()
			app.conv.ClearThinking()
			app.conv.EndAssistantMessage()
			a.showError(errMsg)
			a.markAssistantMessageFinalized(messageID)
			return
		}
	}
	content, _ := plString(ev.Payload, "content")
	if strings.TrimSpace(content) != "" {
		app.resetEmptyAssistantResponses()
		app.conv.ClearThinking()
		app.ensureAssistantMessage(messageID)
		app.conv.SetAssistantTextIfEmpty(content)
		app.conv.EndAssistantMessage()
		a.markAssistantMessageFinalized(messageID)
		return
	}
	if messageID != "" && !app.conv.IsStreaming() {
		a.markAssistantMessageFinalized(messageID)
		return
	}
	app.conv.ClearThinking()
	app.conv.EndAssistantMessage()
	app.noteEmptyAssistantResponse()
	a.markAssistantMessageFinalized(messageID)
}

func (a *conversationEventAdapter) markAssistantMessageFinalized(messageID string) {
	if messageID == "" {
		a.finalizedAnonymousAssistant = true
		return
	}
	if a.finalizedAssistantMessages == nil {
		a.finalizedAssistantMessages = make(map[string]struct{})
	}
	a.finalizedAssistantMessages[messageID] = struct{}{}
}

// showError appends an error row unless the identical text was just shown
// (a run failure surfaces both as an error assistant message and as the
// session's "error" event).
func (a *conversationEventAdapter) showError(text string) {
	if a == nil || a.app == nil || text == "" || text == a.lastErrorText {
		return
	}
	a.lastErrorText = text
	a.app.conv.AddError(text)
}

func payloadMessageID(payload any) string {
	messageID, _ := plString(payload, "message_id")
	if messageID == "" {
		messageID, _ = plString(payload, "messageId")
	}
	return messageID
}

func payloadValue(payload any, key string) any {
	m, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	return m[key]
}

func plStringAny(payload any, keys ...string) (string, bool) {
	for _, key := range keys {
		if value, ok := plString(payload, key); ok {
			return value, true
		}
	}
	return "", false
}

func payloadDurationMs(payload any) *float64 {
	switch value := payloadValue(payload, "durationMs").(type) {
	case float64:
		return new(value)
	case *float64:
		if value != nil {
			return new(*value)
		}
	}
	return nil
}
