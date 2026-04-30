package tui

import (
	"strings"

	"github.com/losinggeneration/tau/sdk"
)

type conversationEventAdapter struct {
	app *App

	finalizedAssistantMessages  map[string]struct{}
	finalizedUserMessages       map[string]struct{}
	finalizedAnonymousAssistant bool
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
			// away from the bottom, count it toward the "New messages" toast
			// so the running count increments on the same boundary the user
			// perceives as "a new message arriving".
			app.noteNewMessage()
			messageID, _ := plString(ev.Payload, "message_id")
			a.finalizedAnonymousAssistant = false
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
		app.conv.AddToolCallWithID(toolCallID, name, args)
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
		app.conv.AddToolResultWithDetails(toolCallID, "", output, isErr, details)
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
			app.conv.AddError(msg)
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
			app.conv.AddUserMessageWithImages(content, images)
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
	messageID := payloadMessageID(ev.Payload)
	if messageID != "" {
		if _, seen := a.finalizedAssistantMessages[messageID]; seen {
			return
		}
	} else if a.finalizedAnonymousAssistant && !app.conv.IsStreaming() {
		return
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
