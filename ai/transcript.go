package ai

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// NormalizeContext folds public prompt/tool shorthand into the transcript.
func NormalizeContext(context *Context) *Context {
	if context == nil {
		return &Context{}
	}
	messages := slices.Clone(context.Messages)
	if context.SystemPrompt != "" || len(context.Tools) > 0 {
		messages = append([]Message{{Role: "system", Content: context.SystemPrompt, ToolsAdded: slices.Clone(context.Tools)}}, messages...)
	}
	return &Context{Messages: messages}
}

// CurrentSystemMessage replays prompt sections and tool declarations into one baseline.
func CurrentSystemMessage(messages []Message) *Message {
	var text []string
	sections := make(map[string]*string)
	tools := make(map[string]Tool)
	var toolOrder []string
	found := false
	var timestamp int64
	for _, message := range messages {
		if message.Role != "system" {
			continue
		}
		if !found {
			timestamp = message.Timestamp
		}
		found = true
		if value := transcriptText(message.Content); value != "" {
			text = append(text, value)
		}
		for name, value := range message.Sections {
			if value == nil {
				delete(sections, name)
			} else {
				sections[name] = new(*value)
			}
		}
		for _, removed := range message.ToolsRemoved {
			delete(tools, removed.Name)
			toolOrder = slices.DeleteFunc(toolOrder, func(name string) bool { return name == removed.Name })
		}
		for _, tool := range message.ToolsAdded {
			if _, exists := tools[tool.Name]; !exists {
				toolOrder = append(toolOrder, tool.Name)
			}
			tools[tool.Name] = tool
		}
	}
	if !found {
		return nil
	}
	baseline := &Message{Role: "system", Content: strings.Join(text, "\n\n"), Sections: sections, Timestamp: timestamp}
	for _, name := range toolOrder {
		baseline.ToolsAdded = append(baseline.ToolsAdded, tools[name])
	}
	return baseline
}

// CurrentSystemPrompt renders the replayed system text and sections.
func CurrentSystemPrompt(messages []Message) string {
	return providerContext(&Context{Messages: messages}).SystemPrompt
}

// providerContext projects transcript state into each provider's native request.
func providerContext(context *Context) *Context {
	normalized := NormalizeContext(context)
	request := &Context{}
	for _, message := range normalized.Messages {
		if message.Role != "system" {
			request.Messages = append(request.Messages, message)
		}
	}
	if baseline := CurrentSystemMessage(normalized.Messages); baseline != nil {
		var text []string
		if value := transcriptText(baseline.Content); value != "" {
			text = append(text, value)
		}
		var sectionOrder []string
		for _, message := range normalized.Messages {
			if message.Role != "system" {
				continue
			}
			names := make([]string, 0, len(message.Sections))
			for name := range message.Sections {
				names = append(names, name)
			}
			slices.Sort(names)
			for _, name := range names {
				if message.Sections[name] == nil {
					sectionOrder = slices.DeleteFunc(sectionOrder, func(key string) bool { return key == name })
				} else if !slices.Contains(sectionOrder, name) {
					sectionOrder = append(sectionOrder, name)
				}
			}
		}
		for _, name := range sectionOrder {
			if value := *baseline.Sections[name]; value != "" {
				text = append(text, value)
			}
		}
		request.SystemPrompt = strings.Join(text, "\n\n")
		request.Tools = baseline.ToolsAdded
	}
	return request
}

func transcriptText(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []ContentBlock:
		var text []string
		for _, block := range value {
			if block.Type == ContentBlockText {
				text = append(text, block.Text)
			}
		}
		return strings.Join(text, "\n")
	case []any:
		var text []string
		for _, block := range value {
			if object, ok := block.(map[string]any); ok && object["type"] == "text" {
				if value, ok := object["text"].(string); ok {
					text = append(text, value)
				}
			}
		}
		return strings.Join(text, "\n")
	default:
		return ""
	}
}

// ValidateContextLimits checks declared model limits before provider admission.
func ValidateContextLimits(model *Model, prompt *Context) error {
	if model == nil || model.InputLimits == nil {
		return nil
	}
	limits := model.InputLimits
	request := NormalizeContext(prompt)
	if images := limits.Images; images != nil {
		var total int
		for _, message := range request.Messages {
			blocks := message.ContentBlocks()
			if blocks == nil {
				data, err := json.Marshal(message.Content)
				if err != nil {
					return fmt.Errorf("marshal message content: %w", err)
				}
				if len(data) == 0 || data[0] != '[' {
					continue
				}
				if err := json.Unmarshal(data, &blocks); err != nil {
					return fmt.Errorf("decode content blocks: %w", err)
				}
			}
			count := 0
			for _, block := range blocks {
				if block.Type == ContentBlockImage {
					count++
				}
			}
			if images.MaxPerMessage > 0 && count > images.MaxPerMessage {
				return fmt.Errorf("message exceeds model image limit %d", images.MaxPerMessage)
			}
			total += count
		}
		if images.MaxPerRequest > 0 && total > images.MaxPerRequest {
			return fmt.Errorf("request exceeds model image limit %d", images.MaxPerRequest)
		}
	}
	if limits.MaxRequestBytes > 0 {
		data, err := json.Marshal(request)
		if err != nil {
			return fmt.Errorf("marshal request context: %w", err)
		}
		if len(data) > limits.MaxRequestBytes {
			return fmt.Errorf("request context exceeds model byte limit %d", limits.MaxRequestBytes)
		}
	}
	return nil
}
