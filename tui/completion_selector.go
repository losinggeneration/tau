package tui

import (
	"strings"

	"github.com/losinggeneration/tau/tui/autocomplete"
)

// CompletionSelector is the toolkit bridge between autocomplete suggestions and
// the generic Selector view. It owns the mechanical selector-row projection and
// completion application so application code only has to decide when to ask for
// suggestions and what to do with the resulting text.
type CompletionSelector struct {
	Prefix string
	Items  []autocomplete.Item
}

// NewCompletionSelector copies suggestions into selector-ready state.
func NewCompletionSelector(s *autocomplete.Suggestions) CompletionSelector {
	if s == nil {
		return CompletionSelector{}
	}
	items := append([]autocomplete.Item(nil), s.Items...)
	return CompletionSelector{Prefix: s.Prefix, Items: items}
}

// SelectorItems converts autocomplete entries into rows suitable for Selector.
func (c CompletionSelector) SelectorItems() []SelectorItem {
	items := make([]SelectorItem, 0, len(c.Items))
	for _, item := range c.Items {
		name := strings.TrimPrefix(item.Value, "@")
		description := strings.TrimSpace(item.Description)
		if description == "" {
			description = strings.TrimSpace(item.Scope)
		}
		if description == "" {
			description = "completion"
		}
		items = append(items, SelectorItem{Name: name, Description: description})
	}
	return items
}

// Apply applies the focused selector row to the editor text. selectedName is a
// fallback lookup key used when focus has drifted or tests construct selector
// rows directly. The returned line slice/cursor are autocomplete.ApplyCompletion
// results.
func (c CompletionSelector) Apply(lines []string, cursorLine, cursorCol, focused int, selectedName string) (autocomplete.ApplyResult, bool) {
	if c.Prefix == "" || len(c.Items) == 0 {
		return autocomplete.ApplyResult{}, false
	}
	item, ok := c.focusedItem(focused, selectedName)
	if !ok {
		return autocomplete.ApplyResult{}, false
	}
	return autocomplete.ApplyCompletion(lines, cursorLine, cursorCol, item, c.Prefix), true
}

func (c CompletionSelector) focusedItem(focused int, selectedName string) (autocomplete.Item, bool) {
	if focused >= 0 && focused < len(c.Items) {
		return c.Items[focused], true
	}
	for _, candidate := range c.Items {
		candidateName := strings.TrimPrefix(candidate.Value, "@")
		if candidateName == selectedName || candidate.Value == selectedName || candidate.Label == selectedName {
			return candidate, true
		}
	}
	return autocomplete.Item{}, false
}

// ReplaceActiveAtReference replaces the active @ token on the current line. It
// is a selector fallback for callers that have a selected path row but no full
// autocomplete suggestion metadata available.
func ReplaceActiveAtReference(text, path string) string {
	lineStart := strings.LastIndexAny(text, "\n") + 1
	at := strings.LastIndex(text[lineStart:], "@")
	if at < 0 {
		return text
	}
	at += lineStart
	return text[:at+1] + path
}
