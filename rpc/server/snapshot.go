package server

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/rpc/coord"
	"github.com/losinggeneration/tau/sdk"
	"github.com/losinggeneration/tau/ui"
)

const (
	historyPageEntries = 16
	historyPageBytes   = 192 << 10
)

// Each field is bounded in bytes before encoding, which can expand it up to
// sixfold. An item holds a 256-byte ID and at most three fields, so the 24
// slots encode under 560KiB. With a 192KiB history page and 128KiB of dialogs
// a snapshot fits one frame; getSnapshot keeps only the live items that fit
// if fields bounded only by the frame, such as the history cursor, still
// push it over. get_entry_content serves up to liveContentBytes of a field.
const (
	liveFieldBytes   = 1536
	liveContentBytes = 1 << 20
	liveSlots        = 8
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
	// UsageLimitWait is set while the run sleeps out a provider usage limit.
	UsageLimitWait *liveUsageLimitWait `json:"usage_limit_wait,omitempty"`
}

type liveUsageLimitWait struct {
	Provider string `json:"provider,omitempty"`
	ResetsAt string `json:"resets_at"`
}

// liveText keeps the tail of one streamed field. It grows to twice
// liveContentBytes before dropping its head, so appends stay amortized O(1).
type liveText struct {
	buf     []byte
	dropped bool
}

func (t *liveText) set(s string) {
	t.buf, t.dropped = t.buf[:0], false
	t.add(s)
}

func (t *liveText) add(s string) {
	if len(s) > liveContentBytes {
		s, t.buf, t.dropped = s[len(s)-liveContentBytes:], t.buf[:0], true
	}
	t.buf = append(t.buf, s...)
	if len(t.buf) > 2*liveContentBytes {
		n := copy(t.buf, t.buf[len(t.buf)-liveContentBytes:])
		t.buf, t.dropped = t.buf[:n], true
	}
}

// tail returns at most limit bytes from the end, starting on a rune.
func (t *liveText) tail(limit int) (string, bool) {
	b, cut := t.buf, t.dropped
	if len(b) > limit {
		b, cut = b[len(b)-limit:], true
	}
	if cut {
		for len(b) > 0 && !utf8.RuneStart(b[0]) {
			b = b[1:]
		}
	}
	return string(b), cut
}

type liveRecord struct {
	id                                          string
	role, name, input, progress, text, thinking liveText
}

// item copies at most limit bytes of each field; Truncated reports any cut.
func (r *liveRecord) item(limit int) liveItem {
	it := liveItem{ID: r.id}
	for _, f := range []struct {
		dst *string
		src *liveText
	}{
		{&it.Role, &r.role},
		{&it.Name, &r.name},
		{&it.Input, &r.input},
		{&it.Progress, &r.progress},
		{&it.Text, &r.text},
		{&it.Thinking, &r.thinking},
	} {
		var cut bool
		*f.dst, cut = f.src.tail(limit)
		it.Truncated = it.Truncated || cut
	}
	return it
}

// liveState tracks in-flight messages, tools, bash runs, and compaction.
// Truncated reports dropped items, not dropped field bytes.
type liveState struct {
	messages, tools, bash []*liveRecord
	truncated             bool
	compacting            bool
	usageLimitWait        *liveUsageLimitWait
}

func livePreviews(records []*liveRecord) []liveItem {
	items := make([]liveItem, 0, len(records))
	for _, r := range records {
		items = append(items, r.item(liveFieldBytes))
	}
	return items
}

func (l *liveState) snapshot() liveSnapshot {
	return liveSnapshot{
		Messages: livePreviews(l.messages), Tools: livePreviews(l.tools), Bash: livePreviews(l.bash),
		Truncated: l.truncated, UsageLimitWait: l.usageLimitWait,
	}
}

// lookup returns the content of the live item id, bounded per field by
// liveContentBytes. kind is message, tool, or bash.
func (l *liveState) lookup(kind, id string) (liveItem, bool) {
	var records []*liveRecord
	switch kind {
	case "message":
		records = l.messages
	case "tool":
		records = l.tools
	case "bash":
		records = l.bash
	}
	for _, r := range records {
		if r.id == id {
			return r.item(liveContentBytes), true
		}
	}
	return liveItem{}, false
}

func (l *liveState) update(kind string, raw json.RawMessage) error {
	var records *[]*liveRecord
	switch kind {
	case "session_start", "session_end", "session_shutdown":
		*l = liveState{}
		return nil
	case "agent_end":
		l.messages, l.tools, l.usageLimitWait = nil, nil, nil
		return nil
	case "usage_limit_wait":
		var wait liveUsageLimitWait
		if err := json.Unmarshal(raw, &wait); err != nil {
			return fmt.Errorf("decode live %s: %w", kind, err)
		}
		l.usageLimitWait = &wait
		return nil
	case "compaction_start", "compaction_end":
		l.compacting = kind == "compaction_start"
		return nil
	case "message_start", "text_delta", "thinking_delta", "message_end":
		records = &l.messages
	case "tool_call", "tool_progress", "tool_result":
		records = &l.tools
	case "bash_execution_update", "bash_execution_end":
		records = &l.bash
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
	if records == &l.messages {
		id = e.MessageID
	}
	if records == &l.tools {
		id = e.ToolID
	}
	if len(id) > 256 {
		l.truncated = true
		return nil
	}
	i := slices.IndexFunc(*records, func(r *liveRecord) bool { return r.id == id })
	switch kind {
	case "message_end", "tool_result", "bash_execution_end":
		if i >= 0 {
			*records = slices.Delete(*records, i, i+1)
		}
		return nil
	}
	if i < 0 {
		if len(*records) == liveSlots {
			l.truncated = true
			return nil
		}
		*records = append(*records, &liveRecord{id: id})
		i = len(*records) - 1
	}
	r := (*records)[i]
	switch kind {
	case "message_start":
		r.role.set(e.Role)
		if e.Role == "assistant" {
			l.usageLimitWait = nil
		}
	case "text_delta", "bash_execution_update":
		r.text.add(e.Delta)
	case "thinking_delta":
		r.thinking.add(e.Delta)
	case "tool_call":
		r.name.set(e.Name)
		r.input.set(e.Input)
	case "tool_progress":
		r.progress.set(e.Chunk)
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

// historyItem is one saved entry. Images carry only their type and MIME
// type; get_entry_content serves their data. A Truncated item holds a
// bounded preview of an entry too large for a page; Omitted ones hold none.
type historyItem struct {
	EntryID   string       `json:"entry_id"`
	Message   *sdk.Message `json:"message,omitempty"`
	Omitted   bool         `json:"omitted,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`
}

// A preview keeps this many bytes of the message text and as many again of
// its content blocks, so even escaped sixfold it stays well under a page.
const (
	historyPreviewBytes     = 8 << 10
	historyPreviewArgsBytes = 1 << 10
)

func isImageBlock(b ai.ContentBlock) bool { return b.Type == ai.ContentBlockImage }

// withoutImageData drops image bytes but keeps each image's type and MIME
// type. Images that duplicate images in Content are dropped.
func withoutImageData(msg sdk.Message) sdk.Message {
	if slices.ContainsFunc(msg.Content, isImageBlock) {
		msg.Images = nil
		msg.Content = slices.Clone(msg.Content)
		for i := range msg.Content {
			if isImageBlock(msg.Content[i]) {
				msg.Content[i].Data = ""
			}
		}
		return msg
	}
	if len(msg.Images) > 0 {
		msg.Images = slices.Clone(msg.Images)
		for i := range msg.Images {
			msg.Images[i].Data = ""
		}
	}
	return msg
}

// entryImage returns the base64 data of the index-th image, counting the
// images in Content, or in Images when Content has none.
func entryImage(msg sdk.Message, index int) (string, bool) {
	if slices.ContainsFunc(msg.Content, isImageBlock) {
		for _, b := range msg.Content {
			if !isImageBlock(b) {
				continue
			}
			if index == 0 {
				return b.Data, true
			}
			index--
		}
		return "", false
	}
	if index < 0 || index >= len(msg.Images) {
		return "", false
	}
	return msg.Images[index].Data, true
}

// clipText keeps at most budget bytes of s, ending on a rune boundary, and
// returns the budget left.
func clipText(s string, budget int) (string, int) {
	if len(s) <= budget {
		return s, budget - len(s)
	}
	n := budget
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], 0
}

// historyPreview keeps a message's identity, tool pairing, and status with
// the start of its text, so an oversized entry still renders in place.
func historyPreview(msg sdk.Message) sdk.Message {
	p := sdk.Message{
		Role: msg.Role, ToolCallID: msg.ToolCallID, ToolName: msg.ToolName, IsError: msg.IsError,
		DurationMs: msg.DurationMs, ToolUsage: msg.ToolUsage, CustomType: msg.CustomType,
		Display: msg.Display, ExcludeFromContext: msg.ExcludeFromContext,
	}
	p.Text, _ = clipText(msg.Text, historyPreviewBytes)
	p.ErrorMessage, _ = clipText(msg.ErrorMessage, historyPreviewBytes)
	budget, argsBudget := historyPreviewBytes, historyPreviewArgsBytes
	for _, b := range msg.Content {
		if isImageBlock(b) {
			p.Content = append(p.Content, ai.ContentBlock{Type: b.Type, MimeType: b.MimeType})
			continue
		}
		b.Data, b.ThoughtSignature, b.TextSignature = "", "", ""
		if len(b.Arguments) > argsBudget {
			b.Arguments = nil
		}
		argsBudget -= len(b.Arguments)
		b.Text, budget = clipText(b.Text, budget)
		b.Thinking, budget = clipText(b.Thinking, budget)
		p.Content = append(p.Content, b)
	}
	for _, b := range msg.Images {
		p.Images = append(p.Images, ai.ContentBlock{Type: b.Type, MimeType: b.MimeType})
	}
	return p
}

type historyPage struct {
	Entries []historyItem `json:"entries"`
	Before  string        `json:"before"`
}

type historyFormat bool

const (
	previewHistory historyFormat = true
)

func readHistory(sess sdk.Session, before string, format historyFormat) (historyPage, error) {
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
	var used int
	for _, entry := range slices.Backward(entries) {
		msg := entry.Message
		if format == previewHistory {
			msg = withoutImageData(msg)
		}
		item := historyItem{EntryID: entry.EntryID, Message: &msg}
		data, err := json.Marshal(item)
		if err != nil {
			return page, err
		}
		if len(data)+1 > historyPageBytes {
			// A preview still closes a tool call and shows where content is.
			if format == previewHistory {
				short := historyPreview(msg)
				item.Message, item.Truncated = &short, true
			} else if msg.Role == "tool" || msg.Role == "toolResult" {
				item.Message = &sdk.Message{Role: msg.Role, ToolCallID: msg.ToolCallID, ToolName: msg.ToolName, IsError: msg.IsError, Text: "(tool output too large to display)"}
			}
			if data, err = json.Marshal(item); err != nil {
				return page, err
			}
			if len(data)+1 > historyPageBytes {
				item.Message, item.Truncated, item.Omitted = nil, false, true
				if data, err = json.Marshal(item); err != nil {
					return page, err
				}
			}
		}
		if used+len(data)+1 > historyPageBytes {
			if len(page.Entries) == 0 {
				return page, fmt.Errorf("history entry ID exceeds page limit")
			}
			page.Before = page.Entries[len(page.Entries)-1].EntryID
			break
		}
		used += len(data) + 1
		page.Entries = append(page.Entries, item)
	}
	slices.Reverse(page.Entries)
	return page, nil
}

func snapshotQueue(sess sdk.Session) map[string]any {
	steerPaused, followPaused := queuesPaused(sess)
	queue, ok := sess.(sdk.QueueEditor)
	if !ok {
		steer, follow := queueSizes(sess)
		return map[string]any{"steering": steer, "follow_up": follow, "steering_paused": steerPaused, "follow_up_paused": followPaused}
	}
	items := queue.QueuedPrompts()
	steer := 0
	for _, item := range items {
		if item.Kind == "steer" {
			steer++
		}
	}
	// The snapshot must fit one frame. Messages too long to preview in full
	// show their start and are not editable from the preview.
	// ponytail: page the queue if this budget becomes limiting.
	remaining := 48 << 10
	visible := make([]sdk.QueuedPrompt, 0, min(len(items), 32))
	for _, item := range items {
		if len(visible) == 32 || remaining <= 0 {
			break
		}
		shorten := func() {
			item.Editable = false
			if len(item.Text) > 256 {
				n := 256
				for !utf8.RuneStart(item.Text[n]) {
					n--
				}
				item.Text = item.Text[:n] + "…"
			}
		}
		if !item.Editable {
			shorten()
		}
		// JSON escaping can expand bytes up to sixfold. Account for the actual encoding.
		data, err := json.Marshal(item)
		if err == nil && len(data)+1 > remaining && item.Editable {
			shorten()
			data, err = json.Marshal(item)
		}
		if err != nil || len(data)+1 > remaining {
			break
		}
		remaining -= len(data) + 1
		visible = append(visible, item)
	}
	return map[string]any{"steering": steer, "follow_up": len(items) - steer, "steering_paused": steerPaused, "follow_up_paused": followPaused, "items": visible}
}

func (w *workerWriter) getSnapshot(cmd *Command) error {
	s := w.server
	var args struct {
		Preview bool `json:"preview"`
	}
	if len(cmd.Args) > 0 {
		if err := json.Unmarshal(cmd.Args, &args); err != nil {
			return s.sendErrorResult(cmd.ID, err)
		}
	}
	s.sessionMu.Lock()
	sess, generation, thinking := s.session, s.sessionGeneration.Load(), s.thinkingLevel
	s.sessionMu.Unlock()
	if sess == nil {
		return s.sendError(cmd.ID, coord.CodeResyncRequired, "active session is changing")
	}
	w.mu.Lock()
	position, instance, live := w.journal.position, w.instance, w.live.snapshot()
	compacting := w.live.compacting
	w.mu.Unlock()
	// Mutable durable state may be ahead of this publication watermark.
	// Events after it still arrive; history entries/dialogs are keyed by ID.
	state := sess.State()
	stats := sessionStats(sess, thinking)
	queue := snapshotQueue(sess)
	dialogs := s.ui.Requests()
	instance.PendingInputs = len(dialogs)
	history, err := readHistory(sess, "", historyFormat(args.Preview))
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	if s.sessionGeneration.Load() != generation || instance.SessionID != sess.ID() {
		return s.sendError(cmd.ID, coord.CodeResyncRequired, "active session changed during snapshot")
	}
	result := snapshotResult{position, instance, history, map[string]any{
		"session_id": sess.ID(), "model": state.Model(), "thinking_level": stats.ThinkingLevel, "stats": stats,
		"is_streaming": state.IsStreaming(), "tokens": state.TotalTokens(),
		"is_compacting": compacting, "queue": queue,
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
		result.Live = liveSnapshot{Messages: []liveItem{}, Tools: []liveItem{}, Bash: []liveItem{}, Truncated: true, UsageLimitWait: live.UsageLimitWait}
		if _, ok := queue["items"]; ok {
			queue["items"] = []sdk.QueuedPrompt{}
		}
		if data, err = json.Marshal(result); err != nil {
			return s.sendErrorResult(cmd.ID, err)
		}
		kept, err := keepLive(&result.Live, live, limit-len(data))
		if err != nil {
			return s.sendErrorResult(cmd.ID, err)
		}
		if kept {
			retry, err := json.Marshal(result)
			if err != nil {
				return s.sendErrorResult(cmd.ID, err)
			}
			if len(retry) <= limit {
				data = retry
			}
		}
	}
	return s.sendResult(cmd.ID, json.RawMessage(data))
}

// keepLive copies into dst the newest previews of each kind of live item
// whose encodings fit budget bytes, and reports whether any did.
func keepLive(dst *liveSnapshot, src liveSnapshot, budget int) (bool, error) {
	var kept bool
	for _, kind := range []struct{ dst, src *[]liveItem }{
		{&dst.Messages, &src.Messages}, {&dst.Tools, &src.Tools}, {&dst.Bash, &src.Bash},
	} {
		start := len(*kind.src)
		for start > 0 {
			data, err := json.Marshal((*kind.src)[start-1])
			if err != nil {
				return false, fmt.Errorf("encode live preview: %w", err)
			}
			if len(data)+1 > budget {
				break
			}
			budget -= len(data) + 1
			start--
		}
		if start < len(*kind.src) {
			*kind.dst = append(*kind.dst, (*kind.src)[start:]...)
			kept = true
		}
	}
	return kept, nil
}

func (w *workerWriter) getHistory(cmd *Command) error {
	s := w.server
	var args struct {
		SessionID string `json:"session_id"`
		Before    string `json:"before"`
		Preview   bool   `json:"preview"`
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
	page, err := readHistory(sess, args.Before, historyFormat(args.Preview))
	if err != nil || s.sessionGeneration.Load() != generation {
		msg := "history changed during transfer"
		if err != nil {
			msg = err.Error()
		}
		return s.sendError(cmd.ID, coord.CodeResyncRequired, msg)
	}
	return s.sendResult(cmd.ID, page)
}

// entryContentChunkBytes bounds one get_entry_content reply's raw data; its
// base64 encoding stays well within a frame.
const entryContentChunkBytes = 192 << 10

type entryContentArgs struct {
	SessionID  string `json:"session_id"`
	Revision   string `json:"revision"`
	EntryID    string `json:"entry_id"`
	LiveKind   string `json:"live_kind"`
	Offset     int64  `json:"offset"`
	ImageIndex *int   `json:"image_index"`
}

type entryContent struct {
	Data     []byte `json:"data"`
	Revision string `json:"revision,omitempty"`
	More     bool   `json:"more"`
}

// entryContentCache holds the content being paged out, so later offsets of
// a live item continue the bytes the first chunk came from.
type entryContentCache struct {
	key      string
	revision string
	data     []byte
}

const (
	entryContentReads = 4
	entryContentBytes = 24 << 20
)

// getEntryContent pages out content that history pages and snapshots only
// preview: a saved message as JSON without image data, one saved image's
// base64 data, or a live item's JSON. Offset 0 always reads it afresh.
func (w *workerWriter) getEntryContent(cmd *Command) error {
	s := w.server
	var args entryContentArgs
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendError(cmd.ID, coord.CodeInvalidFrame, "invalid get_entry_content arguments: "+err.Error())
	}
	switch {
	case args.EntryID == "":
		return s.sendError(cmd.ID, coord.CodeInvalidFrame, "entry_id required")
	case args.Offset < 0:
		return s.sendError(cmd.ID, coord.CodeInvalidFrame, "offset must not be negative")
	case args.ImageIndex != nil && *args.ImageIndex < 0:
		return s.sendError(cmd.ID, coord.CodeInvalidFrame, "image_index must not be negative")
	case args.ImageIndex != nil && args.LiveKind != "":
		return s.sendError(cmd.ID, coord.CodeInvalidFrame, "image_index applies only to saved entries")
	}
	switch args.LiveKind {
	case "", "message", "tool", "bash":
	default:
		return s.sendError(cmd.ID, coord.CodeInvalidFrame, fmt.Sprintf("unknown live_kind %q", args.LiveKind))
	}
	s.sessionMu.Lock()
	sess, generation := s.session, s.sessionGeneration.Load()
	s.sessionMu.Unlock()
	if sess == nil || sess.ID() != args.SessionID {
		return s.sendError(cmd.ID, coord.CodeResyncRequired, "content request is not for the active session")
	}
	image := -1
	if args.ImageIndex != nil {
		image = *args.ImageIndex
	}
	key := fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%d", args.SessionID, generation, args.LiveKind, args.EntryID, image)

	var data []byte
	revision := args.Revision
	w.mu.Lock()
	contentGeneration := w.contentGeneration
	if args.Offset > 0 && revision != "" {
		for _, read := range w.content {
			if read.revision == revision && read.key == key {
				data = read.data
				break
			}
		}
	}
	w.mu.Unlock()
	if args.Offset == 0 {
		var problem *coord.Error
		data, problem = w.loadEntryContent(sess, args)
		if problem != nil {
			return s.sendError(cmd.ID, problem.Code, problem.Message)
		}
		if len(data) > entryContentBytes {
			return s.sendError(cmd.ID, coord.CodeLimitExceeded, "entry content exceeds the transfer limit")
		}
		revision = rand.Text()
	} else if data == nil {
		return s.sendError(cmd.ID, coord.CodeResyncRequired, "content read expired; start again at offset zero")
	}
	if args.Offset > int64(len(data)) {
		return s.sendError(cmd.ID, coord.CodeInvalidFrame, fmt.Sprintf("offset %d is past the end of %d content bytes", args.Offset, len(data)))
	}
	end := min(int64(len(data)), args.Offset+entryContentChunkBytes)
	result := entryContent{Data: data[args.Offset:end:end], Revision: revision, More: end < int64(len(data))}
	if result.Data == nil {
		result.Data = []byte{}
	}
	w.mu.Lock()
	changed := w.contentGeneration != contentGeneration || s.sessionGeneration.Load() != generation
	if !changed && args.Offset == 0 && result.More {
		w.rememberContent(entryContentCache{key: key, revision: revision, data: data})
	}
	w.mu.Unlock()
	if changed {
		return s.sendError(cmd.ID, coord.CodeResyncRequired, "session changed during content read")
	}
	return s.sendResult(cmd.ID, result)
}

// rememberContent retains recent reads within both limits. The caller holds w.mu.
func (w *workerWriter) rememberContent(read entryContentCache) {
	used, first := len(read.data), len(w.content)
	for first > 0 && len(w.content)-first < entryContentReads-1 {
		n := len(w.content[first-1].data)
		if used+n > entryContentBytes {
			break
		}
		used += n
		first--
	}
	w.content = append(slices.Delete(w.content, 0, first), read)
}

func (w *workerWriter) loadEntryContent(sess sdk.Session, args entryContentArgs) ([]byte, *coord.Error) {
	if args.LiveKind != "" {
		w.mu.Lock()
		var (
			item  liveItem
			found bool
		)
		if w.instance.SessionID == args.SessionID {
			item, found = w.live.lookup(args.LiveKind, args.EntryID)
		}
		w.mu.Unlock()
		if !found {
			return nil, &coord.Error{Code: coord.CodeNotFound, Message: fmt.Sprintf("live %s %q is not in progress", args.LiveKind, args.EntryID)}
		}
		data, err := json.Marshal(item)
		if err != nil {
			return nil, &coord.Error{Code: "error", Message: err.Error()}
		}
		return data, nil
	}
	reader, ok := sess.(sdk.HistoryEntryReader)
	if !ok {
		return nil, &coord.Error{Code: coord.CodeUnavailable, Message: "session cannot read saved entries"}
	}
	entry, err := reader.HistoryEntry(args.EntryID)
	if errors.Is(err, sdk.ErrHistoryEntryNotFound) {
		return nil, &coord.Error{Code: coord.CodeNotFound, Message: fmt.Sprintf("saved entry %q not found", args.EntryID)}
	}
	if err != nil {
		return nil, &coord.Error{Code: "error", Message: err.Error()}
	}
	if args.ImageIndex != nil {
		data, ok := entryImage(entry.Message, *args.ImageIndex)
		if !ok {
			return nil, &coord.Error{Code: coord.CodeNotFound, Message: fmt.Sprintf("saved entry %q has no image %d", args.EntryID, *args.ImageIndex)}
		}
		return []byte(data), nil
	}
	data, err := json.Marshal(withoutImageData(entry.Message))
	if err != nil {
		return nil, &coord.Error{Code: "error", Message: err.Error()}
	}
	return data, nil
}
