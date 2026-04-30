package session

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/losinggeneration/tau/agent/export"
)

// Options controls export rendering.
type Options struct {
	// NoCollapse expands all <details> sections (branch summaries, abandoned
	// branches) by default. Useful for side-by-side diffing.
	NoCollapse bool
}

// RenderHTML produces a self-contained HTML document for the given
// session, rendering only the active branch by default. It renders
// branch summaries inline at divergence points, and abandoned branches
// are collapsed by default. Bash tool output passes through AnsiToHTML;
// other code is HTML-escaped into plain <pre> blocks.
func RenderHTML(r *Reader, opts Options) ([]byte, error) {
	css, err := export.CSS()
	if err != nil {
		return nil, fmt.Errorf("load stylesheet: %w", err)
	}

	name := r.DisplayName()
	if name == "" {
		name = r.Header().ID[:8]
	}
	h := r.Header()

	// Build a map of ID -> Entry for the full session
	byID := make(map[string]Entry, len(r.entries))
	for _, e := range r.entries {
		byID[e.ID] = e
	}

	// Get the active branch and track which entries are on it
	activeBranch := r.ActiveBranch()
	onActiveBranch := make(map[string]bool, len(activeBranch))
	for _, id := range activeBranch {
		onActiveBranch[id] = true
	}

	// Collect active branch entries in chronological order
	var activeEntries []Entry
	for _, id := range activeBranch {
		if e, ok := byID[id]; ok {
			activeEntries = append(activeEntries, e)
		}
	}

	// Collect abandoned branch summaries (branch_summary entries NOT on the active branch)
	var abandonedSummaries []Entry
	for _, e := range r.entries {
		if e.Type == EntryTypeBranchSummary && !onActiveBranch[e.ID] {
			abandonedSummaries = append(abandonedSummaries, e)
		}
	}

	var body strings.Builder

	// Render active branch entries
	for _, e := range activeEntries {
		switch e.Type {
		case EntryTypeMessage:
			renderMessage(&body, e)
		case EntryTypeCustomMessage:
			renderCustomMessage(&body, e)
		case EntryTypeCompaction:
			renderCompaction(&body, e)
		case EntryTypeBranchSummary:
			// Inline branch summaries that are on the active branch
			renderBranchSummary(&body, e, opts.NoCollapse)
		case EntryTypeSessionInfo:
			renderSessionInfo(&body, e)
		case EntryTypeModelChange:
			renderModelChange(&body, e)
		case EntryTypeThinkingChange:
			renderThinkingChange(&body, e)
			// position_change and custom are intentionally skipped
		}
	}

	// Render abandoned branches as collapsed sections
	if len(abandonedSummaries) > 0 {
		if opts.NoCollapse {
			for _, e := range abandonedSummaries {
				renderBranchSummary(&body, e, true)
			}
		} else {
			fmt.Fprintf(&body, "<details class=\"abandoned-branches\"><summary>Abandoned branches (%d)</summary>\n", len(abandonedSummaries))
			for _, e := range abandonedSummaries {
				renderBranchSummary(&body, e, false)
			}
			fmt.Fprintf(&body, "</details>\n")
		}
	}

	doc := fmt.Sprintf(`<!DOCTYPE html>
<html><head>
<meta charset="utf-8">
<title>Session: %s</title>
<style>%s</style>
</head><body>
<h1>%s</h1>
<div class="metadata">Created: %s | CWD: %s</div>
%s</body></html>`,
		escapeHTML(name),
		css,
		escapeHTML(name),
		escapeHTML(h.Timestamp),
		escapeHTML(h.CWD),
		body.String(),
	)

	return []byte(doc), nil
}

// renderMessage renders an EntryTypeMessage entry.
func renderMessage(w *strings.Builder, e Entry) {
	if e.Message == nil {
		return
	}
	var msg map[string]any
	if err := json.Unmarshal(e.Message, &msg); err != nil {
		return
	}

	role, _ := msg["role"].(string)
	content := htmlMessageContent(msg["content"])

	fmt.Fprintf(w, "<div class=\"message %s\">\n", escapeHTML(role))
	fmt.Fprintf(w, "<div class=\"role\">%s</div>\n", escapeHTML(role))
	fmt.Fprintf(w, "<div class=\"content\">%s</div>\n", content)

	// Build a map of tool_call_id -> tool name for ANSI detection
	toolNames := extractToolNames(msg)

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
			fmt.Fprintf(w, "<details class=\"tool-call\"><summary>Tool: %s</summary><pre>%s</pre></details>\n",
				escapeHTML(tcName), escapeHTML(string(inputJSON)))
		}
	}

	// Tool results
	if toolResults, ok := msg["tool_results"].([]any); ok {
		for _, tr := range toolResults {
			trMap, ok := tr.(map[string]any)
			if !ok {
				continue
			}
			renderToolResult(w, trMap, toolNames)
		}
	}

	fmt.Fprintf(w, "</div>\n")
}

// renderCustomMessage renders a custom_message entry like a regular message
// but with a custom role label.
func renderCustomMessage(w *strings.Builder, e Entry) {
	if e.Message == nil {
		return
	}
	var msg map[string]any
	if err := json.Unmarshal(e.Message, &msg); err != nil {
		return
	}

	customType := ""
	if e.CustomType != nil {
		customType = *e.CustomType
	}
	roleLabel := "custom"
	if customType != "" {
		roleLabel = "Custom (" + customType + ")"
	}

	content := htmlMessageContent(msg["content"])

	fmt.Fprintf(w, "<div class=\"message custom\">\n")
	fmt.Fprintf(w, "<div class=\"role\">%s</div>\n", escapeHTML(roleLabel))
	fmt.Fprintf(w, "<div class=\"content\">%s</div>\n", content)
	fmt.Fprintf(w, "</div>\n")
}

// renderCompaction renders an EntryTypeCompaction entry (open by default).
func renderCompaction(w *strings.Builder, e Entry) {
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
	fmt.Fprintf(w, "<details class=\"compaction\" open>\n")
	fmt.Fprintf(w, "<summary>Compaction Summary (%d → %d tokens)</summary>\n", tokensBefore, tokensAfter)
	fmt.Fprintf(w, "<pre>%s</pre>\n", escapeHTML(*e.Summary))
	fmt.Fprintf(w, "</details>\n")
}

// renderBranchSummary renders an EntryTypeBranchSummary entry.
// Collapsed by default; open if noCollapse is true.
func renderBranchSummary(w *strings.Builder, e Entry, noCollapse bool) {
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

	openAttr := ""
	if noCollapse {
		openAttr = " open"
	}

	fmt.Fprintf(w, "<details class=\"branch-summary\"%s>\n", openAttr)
	fmt.Fprintf(w, "<summary>Branched from %s</summary>\n", escapeHTML(fromID))
	fmt.Fprintf(w, "<pre>%s</pre>\n", escapeHTML(*e.Summary))
	fmt.Fprintf(w, "</details>\n")
}

// renderSessionInfo renders an EntryTypeSessionInfo entry.
func renderSessionInfo(w *strings.Builder, e Entry) {
	if e.Name == nil {
		return
	}
	fmt.Fprintf(w, "<div class=\"session-info\">Renamed to &#34;%s&#34;</div>\n", escapeHTML(*e.Name))
}

// renderModelChange renders an EntryTypeModelChange entry.
func renderModelChange(w *strings.Builder, e Entry) {
	if e.Provider == nil {
		return
	}
	parts := []string{*e.Provider}
	if e.Model != nil && *e.Model != "" {
		parts = append(parts, *e.Model)
	}
	fmt.Fprintf(w, "<div class=\"model-change\">Switched to %s</div>\n",
		escapeHTML(strings.Join(parts, "/")))
}

// renderThinkingChange renders an EntryTypeThinkingChange entry.
func renderThinkingChange(w *strings.Builder, e Entry) {
	if e.Level == nil {
		return
	}
	fmt.Fprintf(w, "<div class=\"thinking-change\">Thinking level → %s</div>\n",
		escapeHTML(*e.Level))
}

// renderToolResult renders a single tool_result object. It applies AnsiToHTML
// for bash/shell tool calls, or HTML-escapes for others. If the tool name is
// unknown but the content contains ANSI escapes, AnsiToHTML is used as a
// fallback.
func renderToolResult(w *strings.Builder, trMap map[string]any, toolNames map[string]string) {
	content, _ := trMap["content"].(string)
	toolCallID, _ := trMap["tool_call_id"].(string)

	toolName := toolNames[toolCallID]
	var rendered string
	if isBashTool(toolName) {
		rendered = export.AnsiToHTML(content)
	} else if toolName == "" && strings.Contains(content, "\x1b[") {
		// Unknown tool but ANSI content: use AnsiToHTML as fallback
		rendered = export.AnsiToHTML(content)
	} else {
		rendered = escapeHTML(content)
	}

	fmt.Fprintf(w, "<div class=\"tool-result\"><pre>%s</pre></div>\n", rendered)
}

// isBashTool returns true if the tool name is a bash/shell tool.
func isBashTool(name string) bool {
	return name == "bash" || name == "shell"
}

// extractToolNames builds a map of tool_call_id -> tool name from tool_calls.
func extractToolNames(msg map[string]any) map[string]string {
	m := make(map[string]string)
	toolCalls, ok := msg["tool_calls"].([]any)
	if !ok {
		return m
	}
	for _, tc := range toolCalls {
		tcMap, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		id, _ := tcMap["id"].(string)
		name, _ := tcMap["name"].(string)
		if id != "" {
			m[id] = name
		}
	}
	return m
}

// escapeHTML escapes the five HTML special characters.
func escapeHTML(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}

// htmlMessageContent converts a message content value (string or content block
// array) into an HTML-escaped string suitable for display.
func htmlMessageContent(content any) string {
	switch c := content.(type) {
	case string:
		return escapeHTML(c)
	case []any:
		var parts []string
		for _, block := range c {
			if bMap, ok := block.(map[string]any); ok {
				switch bMap["type"] {
				case "text":
					if text, ok := bMap["text"].(string); ok {
						parts = append(parts, escapeHTML(text))
					}
				case "tool_use":
					name, _ := bMap["name"].(string)
					parts = append(parts, fmt.Sprintf("[Tool call: %s]", escapeHTML(name)))
				case "tool_result":
					parts = append(parts, "[Tool result]")
				default:
					// Emit type label for unknown block types
					if t, ok := bMap["type"].(string); ok {
						parts = append(parts, fmt.Sprintf("[%s]", escapeHTML(t)))
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}
