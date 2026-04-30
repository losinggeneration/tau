package session

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ansiEscapeRe matches ANSI CSI escape sequences (e.g. \x1b[31m, \x1b[0m).
var ansiEscapeRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// stripANSI removes ANSI escape sequences from s.
func stripANSI(s string) string {
	return ansiEscapeRe.ReplaceAllString(s, "")
}

// RenderMarkdown produces a Markdown rendering of the
// active branch only (not abandoned branches). Walks the active entry
// chain so branch summaries, model changes,
// and other non-context entries appear in the output.
// NoCollapse is accepted for API symmetry but is a no-op (Markdown has no
// collapsible sections).
func RenderMarkdown(r *Reader, opts Options) ([]byte, error) {
	name := r.DisplayName()
	if name == "" {
		name = r.Header().ID[:8]
	}
	h := r.Header()

	var buf strings.Builder

	fmt.Fprintf(&buf, "# Session: %s\n\n", name)
	fmt.Fprintf(&buf, "**Created:** %s\n", h.Timestamp)
	fmt.Fprintf(&buf, "**CWD:** %s\n\n---\n", h.CWD)

	// Get active branch entry IDs
	branchIDs := r.ActiveBranch()

	// Build lookup map from all entries
	entriesByID := make(map[string]Entry)
	for _, e := range r.Entries() {
		entriesByID[e.ID] = e
	}

	// Render entries in active branch order
	for _, id := range branchIDs {
		e, ok := entriesByID[id]
		if !ok {
			continue
		}

		switch e.Type {
		case EntryTypeMessage:
			mdRenderMessage(&buf, e)
		case EntryTypeCustomMessage:
			mdRenderCustomMessage(&buf, e)
		case EntryTypeCompaction:
			mdRenderCompaction(&buf, e)
		case EntryTypeBranchSummary:
			mdRenderBranchSummary(&buf, e)
		case EntryTypeSessionInfo:
			mdRenderSessionInfo(&buf, e)
		case EntryTypeModelChange:
			mdRenderModelChange(&buf, e)
		case EntryTypeThinkingChange:
			mdRenderThinkingChange(&buf, e)
			// EntryTypePositionChange and EntryTypeCustom are intentionally skipped
		}
	}

	return []byte(buf.String()), nil
}

func mdRenderMessage(w *strings.Builder, e Entry) {
	if e.Message == nil {
		return
	}
	var msg map[string]any
	if err := json.Unmarshal(e.Message, &msg); err != nil {
		return
	}

	role, _ := msg["role"].(string)
	content := mdMessageContent(msg["content"])

	fmt.Fprintf(w, "### %s\n\n%s\n\n", strings.Title(role), content) //nolint:staticcheck

	// Tool calls
	if toolCalls, ok := msg["tool_calls"].([]any); ok {
		for _, tc := range toolCalls {
			tcMap, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			tcName, _ := tcMap["name"].(string)
			input, _ := tcMap["input"].(map[string]any)
			inputJSON, _ := json.MarshalIndent(input, "", "  ")
			fmt.Fprintf(w, "**Tool:** `%s`\n```json\n%s\n```\n", tcName, string(inputJSON))
		}
	}

	// Tool results
	if toolResults, ok := msg["tool_results"].([]any); ok {
		for _, tr := range toolResults {
			trMap, ok := tr.(map[string]any)
			if !ok {
				continue
			}
			content, _ := trMap["content"].(string)
			content = stripANSI(content)
			fmt.Fprintf(w, "**Tool Result:**\n```\n%s\n```\n", content)
		}
	}
}

func mdRenderCustomMessage(w *strings.Builder, e Entry) {
	if e.Message == nil {
		return
	}
	var msg map[string]any
	if err := json.Unmarshal(e.Message, &msg); err != nil {
		return
	}

	ct := "custom"
	if e.CustomType != nil && *e.CustomType != "" {
		ct = *e.CustomType
	}
	content := mdMessageContent(msg["content"])
	fmt.Fprintf(w, "### Custom (%s)\n\n%s\n\n", ct, content)
}

func mdRenderCompaction(w *strings.Builder, e Entry) {
	if e.Summary == nil {
		return
	}
	tokensBefore := 0
	tokensAfter := 0
	if e.TokensBefore != nil {
		tokensBefore = *e.TokensBefore
	}
	if e.TokensAfter != nil {
		tokensAfter = *e.TokensAfter
	}
	fmt.Fprintf(w, "### Compaction (%d → %d tokens)\n%s\n\n---\n", tokensBefore, tokensAfter, *e.Summary)
}

func mdRenderBranchSummary(w *strings.Builder, e Entry) {
	if e.Summary == nil {
		return
	}
	fromID := ""
	if e.FromID != nil {
		fromID = *e.FromID
		if len(fromID) > 8 {
			fromID = fromID[:8]
		}
	}
	fmt.Fprintf(w, "### Branch Summary (from %s)\n%s\n\n---\n", fromID, *e.Summary)
}

func mdRenderSessionInfo(w *strings.Builder, e Entry) {
	if e.Name == nil {
		return
	}
	fmt.Fprintf(w, "**Renamed:** %s\n\n", *e.Name)
}

func mdRenderModelChange(w *strings.Builder, e Entry) {
	if e.Provider == nil {
		return
	}
	parts := []string{*e.Provider}
	if e.Model != nil && *e.Model != "" {
		parts = append(parts, *e.Model)
	}
	fmt.Fprintf(w, "**Model:** %s\n\n", strings.Join(parts, "/"))
}

func mdRenderThinkingChange(w *strings.Builder, e Entry) {
	if e.Level == nil {
		return
	}
	fmt.Fprintf(w, "**Thinking level:** %s\n\n", *e.Level)
}

// mdMessageContent converts a message content value (string or content block
// array) into a plain-text String suitable for Markdown output.
func mdMessageContent(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, block := range c {
			if bMap, ok := block.(map[string]any); ok {
				switch bMap["type"] {
				case "text":
					if text, ok := bMap["text"].(string); ok {
						parts = append(parts, text)
					}
				case "tool_use":
					name, _ := bMap["name"].(string)
					parts = append(parts, fmt.Sprintf("[Tool call: %s]", name))
				case "tool_result":
					parts = append(parts, "[Tool result]")
				default:
					if t, ok := bMap["type"].(string); ok {
						parts = append(parts, fmt.Sprintf("[%s]", t))
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}
