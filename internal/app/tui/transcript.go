package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type TranscriptFormatOptions struct {
	IncludeThinking bool
}

type transcriptJSONComponent struct {
	ID            string                 `json:"id"`
	Kind          ChatComponentKind      `json:"kind"`
	Role          Role                   `json:"role,omitempty"`
	Text          string                 `json:"text,omitempty"`
	Streaming     bool                   `json:"streaming,omitempty"`
	Revision      uint64                 `json:"revision"`
	ToolExecution ToolExecutionComponent `json:"toolExecution,omitempty"`
	Image         ImageComponent         `json:"image,omitempty"`
	Custom        CustomComponent        `json:"custom,omitempty"`
}

func FormatTranscriptMarkdown(snapshot ChatSnapshot, opts TranscriptFormatOptions) string {
	var b strings.Builder
	for _, component := range snapshot.Components {
		text, ok := componentTranscriptMarkdown(component, opts)
		if !ok {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(text)
	}
	return strings.TrimRight(b.String(), "\n")
}

func FormatTranscriptJSON(snapshot ChatSnapshot, opts TranscriptFormatOptions) ([]byte, error) {
	components := make([]transcriptJSONComponent, 0, len(snapshot.Components))
	for _, component := range snapshot.Components {
		if component.Kind == ChatComponentThinking && component.Collapsed && !opts.IncludeThinking {
			continue
		}
		components = append(components, transcriptJSONComponent{ID: component.ID, Kind: component.Kind, Role: component.Role, Text: component.Text, Streaming: component.Streaming, Revision: component.Revision, ToolExecution: component.ToolExecution, Image: component.Image, Custom: component.Custom})
	}
	data, err := json.MarshalIndent(struct {
		Revision   uint64                    `json:"revision"`
		Components []transcriptJSONComponent `json:"components"`
	}{Revision: snapshot.Revision, Components: components}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal transcript json: %w", err)
	}
	return data, nil
}

func WriteTranscript(path, format string, snapshot ChatSnapshot, opts TranscriptFormatOptions) error {
	var data []byte
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "markdown", "md":
		data = []byte(FormatTranscriptMarkdown(snapshot, opts) + "\n")
	case "json":
		var err error
		data, err = FormatTranscriptJSON(snapshot, opts)
		if err != nil {
			return err
		}
		data = append(data, '\n')
	default:
		return fmt.Errorf("unsupported export format %q", format)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write transcript: %w", err)
	}
	return nil
}

func LastMeaningfulTranscriptComponent(snapshot ChatSnapshot, opts TranscriptFormatOptions) (string, bool) {
	for i := len(snapshot.Components) - 1; i >= 0; i-- {
		component := snapshot.Components[i]
		if component.Kind == ChatComponentThinking && !opts.IncludeThinking {
			continue
		}
		if component.Kind == ChatComponentAssistant && strings.TrimSpace(component.Text) != "" {
			return strings.TrimSpace(component.Text), true
		}
	}
	for i := len(snapshot.Components) - 1; i >= 0; i-- {
		if text, ok := componentTranscriptMarkdown(snapshot.Components[i], opts); ok && strings.TrimSpace(text) != "" {
			return text, true
		}
	}
	return "", false
}

func componentTranscriptMarkdown(component ChatComponent, opts TranscriptFormatOptions) (string, bool) {
	trimmed := strings.TrimRight(component.Text, "\n")
	switch component.Kind {
	case ChatComponentUser:
		return "**User**\n\n" + trimmed, strings.TrimSpace(trimmed) != ""
	case ChatComponentAssistant:
		return "**Assistant**\n\n" + trimmed, strings.TrimSpace(trimmed) != ""
	case ChatComponentSystem:
		return "**System**\n\n" + trimmed, strings.TrimSpace(trimmed) != ""
	case ChatComponentError:
		return "**Error**\n\n" + trimmed, strings.TrimSpace(trimmed) != ""
	case ChatComponentThinking:
		if component.Collapsed && !opts.IncludeThinking {
			return "", false
		}
		if strings.TrimSpace(trimmed) == "" {
			trimmed = defaultHiddenThinkingLabel
		}
		return "**Thinking**\n\n" + trimmed, true
	case ChatComponentToolExecution:
		tool := component.ToolExecution
		var b strings.Builder
		fmt.Fprintf(&b, "**Tool (%s): %s**", transcriptToolStatusLabel(tool.State), fallbackString(tool.Name, "tool"))
		if strings.TrimSpace(tool.Args) != "" {
			b.WriteString("\n\n```json\n")
			b.WriteString(strings.TrimSpace(tool.Args))
			b.WriteString("\n```")
		}
		if strings.TrimSpace(tool.Output) != "" {
			b.WriteString("\n\n")
			b.WriteString(strings.TrimRight(tool.Output, "\n"))
		}
		return b.String(), true
	case ChatComponentImage:
		return component.Image.FallbackText(), true
	case ChatComponentCompactionSummary:
		title := strings.TrimSpace(component.Custom.Title)
		if title == "" {
			title = "Context compacted"
		}
		return "◇ " + title + "\n" + trimmed, true
	case ChatComponentBranchSummary:
		return "◇ Branch\n" + trimmed, true
	case ChatComponentDiff:
		return "**Diff**\n\n" + trimmed, true
	case ChatComponentSkillInvocation:
		return "**Skill**\n\n" + trimmed, true
	default:
		if strings.TrimSpace(trimmed) == "" {
			trimmed = component.Custom.FallbackText()
		}
		return trimmed, strings.TrimSpace(trimmed) != ""
	}
}

func transcriptToolStatusLabel(state toolExecutionState) string {
	switch state {
	case toolExecutionExecuting:
		return "running"
	case toolExecutionError:
		return "error"
	case toolExecutionCancelled:
		return "cancelled"
	default:
		return "complete"
	}
}

func fallbackString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}
