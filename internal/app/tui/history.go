package tui

import (
	"strings"

	"github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
)

// Chat history beyond the rendered window lives in the session file, not in
// memory. Once more than a window's worth of components sits above the chat
// log's window, the conversation model drops everything before the first item
// of a persisted entry and remembers that entry; when the viewport reaches the
// top of what is held, the next older page is read back from the file.

// appendHistoryToConversation adds persisted history entries to conv, tagging
// each item with the entry that produced it so the model can drop and re-page
// history at entry boundaries.
func appendHistoryToConversation(conv *ConversationModel, entries []sdk.HistoryEntry) {
	conv.Batch(func() {
		for _, entry := range entries {
			before := len(conv.committed)
			appendSDKMessageToConversation(conv, entry.Message)
			for i := before; i < len(conv.committed); i++ {
				conv.committed[i].entryID = entry.EntryID
			}
		}
	})
}

// seedChatFromHistory installs the newest page of persisted history, reporting
// false when the session cannot page its history.
func (a *App) seedChatFromHistory(sess sdk.Session) bool {
	pager, ok := sess.(sdk.HistoryPager)
	if !ok {
		return false
	}
	entries, next, err := pager.History("", chatLogWindowComponents)
	if err != nil || len(entries) == 0 {
		return false
	}
	appendHistoryToConversation(a.conv, entries)
	a.historyNext = next
	return true
}

// dropOffscreenHistory releases held history above the chat window once it
// exceeds a window's worth, cutting at a persisted user message so the cut is
// exactly where paging resumes. Runs on the app loop.
func (a *App) dropOffscreenHistory() {
	if a == nil || a.chat == nil || a.conv == nil {
		return
	}
	if _, ok := a.sess.(sdk.HistoryPager); !ok {
		return
	}
	offset, base := a.chat.windowBounds()
	if offset-base <= chatLogWindowComponents {
		return
	}
	boundary, entryID, ok := a.conv.HistoryBoundary(offset)
	if !ok {
		return
	}
	a.conv.DropBefore(boundary)
	a.resetHistoryPaging()
	a.historyNext = entryID
}

// loadOlderHistory pages the history preceding the oldest held component back
// in from the session file. The read runs off the loop; a result is dropped if
// the conversation or the paging point moved while it ran.
func (a *App) loadOlderHistory() {
	if a == nil || a.historyLoading || a.historyNext == "" {
		return
	}
	pager, ok := a.sess.(sdk.HistoryPager)
	if !ok {
		return
	}
	a.historyLoading = true
	before, generation, conv := a.historyNext, a.historyGeneration, a.conv
	go func() {
		entries, next, err := pager.History(before, chatLogWindowComponents)
		a.post(func(*tauui.UpdateCtx) {
			if a.historyGeneration != generation || a.conv != conv {
				return
			}
			a.historyLoading = false
			if err != nil {
				a.historyNext = ""
				a.conv.AddError("history: " + strings.TrimPrefix(err.Error(), "read session history: "))
				return
			}
			older := NewConversationModel()
			appendHistoryToConversation(older, entries)
			a.historyNext = next
			a.historyGeneration++
			a.conv.PrependHistory(older)
		})
	}()
}

// resetHistoryPaging forgets the paging point when the transcript is replaced.
func (a *App) resetHistoryPaging() {
	a.historyNext = ""
	a.historyLoading = false
	a.historyGeneration++
}
