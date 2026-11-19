package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/losinggeneration/tau/rpc/coord"
	"github.com/losinggeneration/tau/sdk"
	"github.com/losinggeneration/tau/ui"
)

const historyPageEntries = 16

// Each field is bounded in bytes before encoding, which can expand it up to
// sixfold. An item holds a 256-byte ID and at most three fields, so the 24
// slots encode under 560KiB. With a 192KiB history page and 128KiB of dialogs
// a snapshot fits one frame; getSnapshot drops live state if fields bounded
// only by the frame, such as the history cursor, still push it over.
const (
	liveFieldBytes = 1536
	liveSlots      = 8
)

type liveItem struct {
	ID        string `json:"id"`
	Role      string `json:"role,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     string `json:"input,omitempty"`
	Progress  string `json:"progress,omitempty"`
	Text      string `json:"text,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type liveSnapshot struct {
	Messages  []liveItem `json:"messages"`
	Tools     []liveItem `json:"tools"`
	Bash      []liveItem `json:"bash"`
	Truncated bool       `json:"truncated,omitempty"`
}

func liveTail(a, b string) (string, bool) {
	if len(a)+len(b) <= liveFieldBytes {
		return a + b, false
	}
	if len(b) >= liveFieldBytes {
		b = b[len(b)-liveFieldBytes:]
		for len(b) > 0 && !utf8.RuneStart(b[0]) {
			b = b[1:]
		}
		return strings.Clone(b), true
	}
	a = a[len(a)-(liveFieldBytes-len(b)):]
	for len(a) > 0 && !utf8.RuneStart(a[0]) {
		a = a[1:]
	}
	return a + b, true
}

func (l *liveSnapshot) update(kind string, raw json.RawMessage) error {
	var items *[]liveItem
	switch kind {
	case "session_start", "session_end", "session_shutdown":
		*l = liveSnapshot{}
		return nil
	case "agent_end":
		l.Messages, l.Tools = nil, nil
		return nil
	case "message_start", "text_delta", "thinking_delta", "message_end":
		items = &l.Messages
	case "tool_call", "tool_progress", "tool_result":
		items = &l.Tools
	case "bash_execution_update", "bash_execution_end":
		items = &l.Bash
	default:
		return nil
	}
	var e struct {
		ID, Role, Delta, Name, Input, Chunk string
		MessageID                           string `json:"message_id"`
		ToolID                              string `json:"tool_call_id"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return fmt.Errorf("decode live %s: %w", kind, err)
	}
	id := e.ID
	if items == &l.Messages {
		id = e.MessageID
	}
	if items == &l.Tools {
		id = e.ToolID
	}
	if len(id) > 256 {
		l.Truncated = true
		return nil
	}
	i := 0
	for i < len(*items) && (*items)[i].ID != id {
		i++
	}
	switch kind {
	case "message_end", "tool_result", "bash_execution_end":
		if i < len(*items) {
			copy((*items)[i:], (*items)[i+1:])
			(*items)[len(*items)-1] = liveItem{}
			*items = (*items)[:len(*items)-1]
		}
		return nil
	}
	if i == len(*items) {
		if i == liveSlots {
			l.Truncated = true
			return nil
		}
		*items = append(*items, liveItem{ID: id})
	}
	item := &(*items)[i]
	set := func(field *string, previous, next string) {
		var truncated bool
		*field, truncated = liveTail(previous, next)
		item.Truncated = item.Truncated || truncated
	}
	switch kind {
	case "message_start":
		set(&item.Role, "", e.Role)
	case "text_delta", "bash_execution_update":
		set(&item.Text, item.Text, e.Delta)
	case "thinking_delta":
		set(&item.Thinking, item.Thinking, e.Delta)
	case "tool_call":
		set(&item.Name, "", e.Name)
		set(&item.Input, "", e.Input)
	case "tool_progress":
		set(&item.Progress, "", e.Chunk)
	}
	return nil
}

type snapshotResult struct {
	Position   coord.Position     `json:"position"`
	Instance   coord.Instance     `json:"instance"`
	History    historyPage        `json:"history"`
	State      map[string]any     `json:"state"`
	Live       liveSnapshot       `json:"live"`
	UIRequests []ui.SharedRequest `json:"ui_requests"`
}

type historyItem struct {
	EntryID string       `json:"entry_id"`
	Message *sdk.Message `json:"message,omitempty"`
	Omitted bool         `json:"omitted,omitempty"`
}

type historyPage struct {
	Entries []historyItem `json:"entries"`
	Before  string        `json:"before"`
}

func readHistory(sess sdk.Session, before string) (historyPage, error) {
	page := historyPage{Entries: []historyItem{}}
	pager, ok := sess.(sdk.HistoryPager)
	if !ok {
		return page, fmt.Errorf("session does not support paged history")
	}
	entries, next, err := pager.History(before, historyPageEntries)
	if err != nil {
		return page, err
	}
	if len(entries) > historyPageEntries {
		return page, fmt.Errorf("history page exceeds entry limit")
	}
	page.Before = next
	for _, entry := range entries {
		item := historyItem{EntryID: entry.EntryID, Message: &entry.Message}
		data, err := json.Marshal(item)
		if err != nil {
			return page, err
		}
		if len(data) > 12<<10 {
			item.Message, item.Omitted = nil, true
		}
		page.Entries = append(page.Entries, item)
	}
	return page, nil
}

func (w *workerWriter) getSnapshot(cmd *Command) error {
	s := w.server
	s.sessionMu.Lock()
	sess, generation := s.session, s.sessionGeneration.Load()
	s.sessionMu.Unlock()
	if sess == nil {
		return s.sendError(cmd.ID, coord.CodeResyncRequired, "active session is changing")
	}
	w.mu.Lock()
	position, instance, live := w.journal.position, w.instance, w.live
	live.Messages = append([]liveItem{}, live.Messages...)
	live.Tools = append([]liveItem{}, live.Tools...)
	live.Bash = append([]liveItem{}, live.Bash...)
	w.mu.Unlock()
	// Mutable durable state may be ahead of this publication watermark.
	// Events after it still arrive; history entries/dialogs are keyed by ID.
	s.sessionMu.Lock()
	thinking := s.thinkingLevel
	s.sessionMu.Unlock()
	state := sess.State()
	steer, follow := queueSizes(sess)
	dialogs := s.ui.Requests()
	instance.PendingInputs = len(dialogs)
	history, err := readHistory(sess, "")
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	if s.sessionGeneration.Load() != generation || instance.SessionID != sess.ID() {
		return s.sendError(cmd.ID, coord.CodeResyncRequired, "active session changed during snapshot")
	}
	result := snapshotResult{position, instance, history, map[string]any{
		"session_id": sess.ID(), "model": state.Model(), "thinking_level": thinking,
		"is_streaming": state.IsStreaming(), "tokens": state.TotalTokens(),
		"queue": map[string]int{"steering": steer, "follow_up": follow},
	}, live, dialogs}
	data, err := json.Marshal(result)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	// Leave room for the RPC and coordinator envelopes, including the
	// client's rewritten response ID and JSON escaping of the routing IDs.
	const limit = coord.MaxFrameBytes - 8<<10
	if len(data) > limit {
		// Dialogs and the history cursor are needed to resume; live state
		// is only a tail of the stream, so clients see it marked truncated.
		result.Live = liveSnapshot{Messages: []liveItem{}, Tools: []liveItem{}, Bash: []liveItem{}, Truncated: true}
		if data, err = json.Marshal(result); err != nil {
			return s.sendErrorResult(cmd.ID, err)
		}
	}
	return s.sendResult(cmd.ID, json.RawMessage(data))
}

func (w *workerWriter) getHistory(cmd *Command) error {
	s := w.server
	var args struct {
		SessionID string `json:"session_id"`
		Before    string `json:"before"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	s.sessionMu.Lock()
	sess, generation := s.session, s.sessionGeneration.Load()
	s.sessionMu.Unlock()
	if sess == nil || sess.ID() != args.SessionID || args.Before == "" {
		return s.sendError(cmd.ID, coord.CodeResyncRequired, "history cursor missing or not for the active session")
	}
	page, err := readHistory(sess, args.Before)
	if err != nil || s.sessionGeneration.Load() != generation {
		msg := "history changed during transfer"
		if err != nil {
			msg = err.Error()
		}
		return s.sendError(cmd.ID, coord.CodeResyncRequired, msg)
	}
	return s.sendResult(cmd.ID, page)
}
