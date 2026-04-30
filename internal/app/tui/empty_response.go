package tui

import "fmt"

const emptyResponseSummaryPrefix = "Empty response from the server:"

// noteEmptyAssistantResponse applies the TUI's empty-response presentation
// policy. Empty assistant messages are not committed to conversation history;
// instead consecutive empty provider responses are summarized in one system row.
func (a *App) noteEmptyAssistantResponse() {
	if a == nil || a.conv == nil {
		return
	}
	a.emptyResponseCount++
	a.conv.UpsertSystemByPrefix(emptyResponseSummaryPrefix, fmt.Sprintf("%s %d", emptyResponseSummaryPrefix, a.emptyResponseCount))
}

func (a *App) refreshEmptyAssistantResponseSummary() {
	if a == nil || a.conv == nil || a.emptyResponseCount <= 0 {
		return
	}
	a.conv.UpsertSystemByPrefix(emptyResponseSummaryPrefix, fmt.Sprintf("%s %d", emptyResponseSummaryPrefix, a.emptyResponseCount))
}

func (a *App) resetEmptyAssistantResponses() {
	if a != nil {
		a.emptyResponseCount = 0
	}
}
