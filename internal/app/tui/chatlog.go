package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"

	tauui "github.com/losinggeneration/tau/tui"
)

// chatPalette maps each conversation role to the full-row style used by the
// chat log. It is a projection of themeTokens, never a source of colour in its
// own right: see theme_tokens.go for the token vocabulary and the named theme
// sets. Every style carries a background because Paint fills each row with it
// before drawing text, so the provenance cue spans the full terminal width.
//
// bashMode and the diff colours stay as colours rather than styles: they are
// applied with WithFG over whatever background the surrounding component
// already established.
type chatPalette struct {
	user, assistant, thinking, toolCall, toolResult, toolError, errorRole, system, dim, spacer tauui.Style

	// custom is the background block a custom message renders on;
	// customLabel is its `[compaction]`/`[branch]`/`[skill]` tag.
	custom, customLabel tauui.Style

	bashMode, diffAdded, diffRemoved, diffContext tauui.Color

	// borderColors is the dynamic border's state→colour table, resolved when
	// the palette is built.
	borderColors [tauui.DynamicBorderStateCount]tauui.Color
}

// borderStyle returns the style a dynamic border draws in for state, over the
// background the surrounding component established.
func (p *chatPalette) borderStyle(state tauui.DynamicBorderState, bg tauui.Color) tauui.Style {
	if int(state) >= len(p.borderColors) {
		state = tauui.DynamicBorderNeutral
	}
	return tauui.Style{FG: p.borderColors[state], BG: bg}
}

// defaultHiddenThinkingLabel stands in for thinking content that is collapsed,
// still streaming with nothing visible yet, or absent. It is the single source
// for the label across the live chat log and exported transcripts.
const defaultHiddenThinkingLabel = "Thinking..."

type chatLogLook uint8

const (
	chatLogLookPi chatLogLook = iota
	chatLogLookTau
)

func defaultChatPalette() chatPalette {
	tokens := defaultThemeTokens()
	return tokens.chatPalette()
}

// renderLine is a single styled terminal row produced by wrapping message
// content to the chat width.
type renderLine struct {
	text string
	st   tauui.Style
}

type chatLogPosition struct {
	line int
	col  int
}

type chatLogSelectionMode uint8

const (
	chatLogSelectionModeRange chatLogSelectionMode = iota
	chatLogSelectionModeWord
	chatLogSelectionModeLine
)

type chatLogSelection struct {
	active bool
	anchor chatLogPosition
	cursor chatLogPosition
	mode   chatLogSelectionMode
}

type chatLogRenderRequest struct {
	revision   uint64
	width      int
	snapshot   ChatSnapshot
	components []ChatComponent
	wrap       chatLogWrapFunc
}

type chatLogMessageSpan struct {
	start     int
	end       int
	tailStart int
	tailByte  int
}

type renderCacheKey struct {
	componentID string
	revision    uint64
	width       int

	// expanded distinguishes the two render states of a component whose
	// preview can be clicked open (truncated user messages, capped tool
	// previews), so toggling cannot hit the other state's cached rows.
	expanded bool
}

type renderCacheEntry struct {
	lines     []renderLine
	tailStart int
	tailByte  int
}

type chatLogRenderState struct {
	width      int
	snapshot   ChatSnapshot
	components []ChatComponent
	messages   []Message
	lines      []renderLine
	spans      []chatLogMessageSpan
}

type ChatLogRenderMetrics struct {
	FullRenderCount        uint64
	IncrementalRenderCount uint64
	CancelledRenderCount   uint64
	CacheHits              uint64
	CacheMisses            uint64
	CacheEntries           int
	CacheEvictions         uint64
	RenderedComponentCount uint64
	RenderedLineCount      uint64
}

type chatLogRenderResult struct {
	revision uint64
	state    chatLogRenderState
}

type chatLogRenderUpdate struct {
	revision      uint64
	installed     bool
	current       bool
	rect          tauui.Rect
	layoutChanged bool
	oldLineCount  int
	newLineCount  int
}

// chatLogRenderNotifier must marshal ChatLog.applyPreparedRender onto the app
// loop. The production notifier uses tauui.App.Post; tests may provide a
// synchronous notifier because no toolkit loop owns their widget state.
type chatLogRenderNotifier func() error

type chatLogWrapFunc func(context.Context, []ChatComponent, int) chatLogRenderState

// ChatLog is a custom tauui.View designed to be wrapped by a ScrollView. It owns
// one render-preparation worker. Model and width changes replace the latest
// pending immutable request, so streaming deltas coalesce instead of queueing
// a full-transcript wrap for every mutation. The worker only prepares
// immutable results; applyPreparedRender installs them on the app loop before
// the owning app invalidates layout and paint.
type ChatLog struct {
	id   tauui.ID
	conv *ConversationModel

	// palette is read by the render worker on every component and by Paint on
	// the app loop, while SetLookName replaces it from whichever goroutine
	// changed the theme. An atomic pointer keeps the read side free (the wrap
	// path touches it once per line) without making theme changes racy.
	palette atomic.Pointer[chatPalette]

	// pluginRenderers, when non-nil, is consulted before falling back to the
	// built-in projection. The role-keyed and tool-name-keyed registries let
	// extensions render messages, tool executions, and custom payloads
	// without forking the chat pipeline. When a renderer returns
	// (rows, replace=true) the chat log adopts those rows; on (nil, false)
	// the default rendering path is used unchanged.
	pluginRenderers *pluginRenderers
	themeName       string
	look            chatLogLook

	// tokens is the resolved token set behind palette, kept so a values-only
	// override (a plugin restyling the active theme) can be re-applied without
	// re-deriving the theme name. Guarded by mu.
	tokens themeTokens

	// hideThinkingBlock drops thinking components from the rendered log
	// entirely; hiddenThinkingLabel stands in for thinking whose text is
	// collapsed or absent. Both guarded by mu.
	hideThinkingBlock   bool
	hiddenThinkingLabel string

	// hideImages renders image components as their text placeholder instead
	// of painting the picture. This is the showImages=false setting: the
	// image data still arrives and is still exported, it just isn't drawn.
	// Guarded by mu.
	hideImages bool

	// toolExpansionHandler is invoked when the user toggles a tool
	// execution's expanded/collapsed state via the chat log. The
	// App installs a handler that forwards the change to the
	// plugin extensions Registry so plugins see the same state
	// the chat log does. Nil is a valid value (no observer).
	toolExpansionHandler func(id string, expanded bool)

	// expanded records components whose truncated preview has been clicked
	// open: user messages render in full with a collapse hint instead of the
	// capped preview, and tool previews drop their line caps. Keyed by
	// component ID. Guarded by mu.
	expanded map[string]bool

	mu sync.Mutex

	// Completed render state. The production app only mutates these fields on
	// its loop, but the mutex also makes headless tests and Close race-safe.
	rect             tauui.Rect
	desiredWidth     int
	wrapWidth        int
	lines            []renderLine
	lineCount        int
	renderedState    chatLogRenderState
	renderedRevision uint64

	// Selection state is app-loop-owned in production. Tests may drive it
	// directly, so accesses are protected by mu like the render cache.
	selection chatLogSelection

	// visibleStart/visibleEnd are content-row bounds supplied by the owning
	// ScrollView wrapper immediately before painting. A zero/empty range means
	// no viewport hint is available, so Paint falls back to drawing all cached
	// rows for direct tests and non-scroll callers.
	visibleStart int
	visibleEnd   int

	// Cross-goroutine request/result handoff state.
	revision            uint64
	pending             *chatLogRenderRequest
	latestPrepared      *chatLogRenderResult
	notificationPending bool
	notifyRenderReady   chatLogRenderNotifier
	wrapMessagesFunc    chatLogWrapFunc
	renderCache         map[renderCacheKey]renderCacheEntry
	renderCacheOrder    []renderCacheKey
	metrics             ChatLogRenderMetrics
	closed              bool

	wake         chan struct{}
	cancel       context.CancelFunc
	activeCancel context.CancelFunc
	done         chan struct{}
	unsubscribe  func()
	closeOnce    sync.Once
}

// NewChatLog returns a ChatLog backed by conv and starts its single persistent
// preparation worker. Rendering is first scheduled by Layout, once a width is
// known. The owner must install a goroutine-safe notifier with
// setRenderNotifier before layout and must call Close during teardown.
func NewChatLog(conv *ConversationModel) *ChatLog {
	ctx, cancel := context.WithCancel(context.Background())
	c := &ChatLog{
		id:     tauui.NewID(),
		conv:   conv,
		tokens: defaultThemeTokens(),
		wake:   make(chan struct{}, 1),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	c.setPalette(c.tokens.chatPalette())
	c.wrapMessagesFunc = c.wrapComponents
	c.unsubscribe = conv.Subscribe(c.onChanged)
	go c.renderWorker(ctx)
	return c
}

// setRenderNotifier configures the goroutine-safe bridge used by the worker to
// ask the owner to apply its newest prepared result. Production callers must
// implement it with tauui.App.Post.
func (c *ChatLog) setRenderNotifier(notify chatLogRenderNotifier) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.notifyRenderReady = notify
	shouldNotify := notify != nil && c.latestPrepared != nil && !c.notificationPending
	if shouldNotify {
		c.notificationPending = true
	}
	c.mu.Unlock()

	if shouldNotify {
		c.sendRenderReady(notify)
	}
}

// MarkDirty schedules the latest conversation snapshot at the current width.
// It is kept for callers that explicitly request a refresh; normal model
// mutations are observed through the subscription installed by NewChatLog.
func (c *ChatLog) MarkDirty() {
	c.scheduleCurrentSnapshot()
}

// pal returns the active palette. It is safe to call from the render worker and
// from the app loop; the pointed-to palette is immutable, so callers may hold
// the pointer for the duration of a render.
func (c *ChatLog) pal() *chatPalette {
	if p := c.palette.Load(); p != nil {
		return p
	}
	// Only reachable for a zero-valued ChatLog built outside NewChatLog.
	p := defaultChatPalette()
	c.palette.CompareAndSwap(nil, &p)
	return c.palette.Load()
}

func (c *ChatLog) setPalette(p chatPalette) { c.palette.Store(&p) }

// SetLookName selects the chrome variant and the theme token set from a theme
// name. Both the look and the palette are derived here so a theme change is a
// single observable transition: the render cache is keyed by revision and
// width, not by theme, so it must be dropped whenever the colours move.
func (c *ChatLog) SetLookName(name string) {
	c.SetTheme(name, nil)
}

// SetTheme selects the theme by name and applies token overrides on top. Values
// are keyed by the spec's token names ("userMessageBg", "toolSuccessBg", ...);
// unknown keys and unparseable colours are ignored so a bad plugin theme costs
// one token rather than the whole UI. Safe to call from any goroutine.
func (c *ChatLog) SetTheme(name string, values map[string]string) {
	if c == nil {
		return
	}
	name = strings.TrimSpace(name)
	look := chatLogLookPi
	if strings.EqualFold(name, "tau") {
		look = chatLogLookTau
	}
	base := themeTokensFor(name)
	tokens := base.withOverrides(values)

	c.mu.Lock()
	changed := c.look != look || c.themeName != name || c.tokens != tokens
	c.look = look
	c.themeName = name
	c.tokens = tokens
	if changed {
		c.renderCache = nil
		c.renderCacheOrder = nil
	}
	c.mu.Unlock()
	if changed {
		c.setPalette(tokens.chatPalette())
		c.scheduleCurrentSnapshot()
	}
}

// ThemeTokens returns the active token set. The App reads it to style chrome it
// owns outside the chat log (currently the prompt editor's rules).
func (c *ChatLog) ThemeTokens() themeTokens {
	if c == nil {
		return defaultThemeTokens()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokens
}

// SetToolExpansionHandler installs a hook invoked when the user
// toggles a tool execution's expanded/collapsed state through the
// chat log. The handler is called with the tool's call id and the
// new expanded state. The App uses this to forward the change to
// the plugin extensions Registry so plugins can observe the same
// expanded/collapsed state the chat log renders.
//
// Pass nil to clear the handler. Safe to call from any goroutine.
func (c *ChatLog) SetToolExpansionHandler(handler func(id string, expanded bool)) {
	c.mu.Lock()
	c.toolExpansionHandler = handler
	c.mu.Unlock()
}

// SetToolExpanded forwards a tool-expansion toggle to the installed
// handler (if any). Returns true when a handler observed the
// change. The chat log calls this on user toggles; plugins that
// want to flip the state from outside the chat log should set the
// state through the extensions Registry and call refreshChat to
// repaint.
func (c *ChatLog) SetToolExpanded(id string, expanded bool) bool {
	c.mu.Lock()
	h := c.toolExpansionHandler
	c.mu.Unlock()
	if h == nil {
		return false
	}
	h(id, expanded)
	return true
}

// setPluginRenderers installs the plugin renderer registry consulted
// during render. Pass nil to disable plugin rendering. The renderer is
// read under mu so it is safe to swap from any goroutine.
func (c *ChatLog) setPluginRenderers(r *pluginRenderers) {
	c.mu.Lock()
	c.pluginRenderers = r
	c.mu.Unlock()
	c.scheduleCurrentSnapshot()
}

func (c *ChatLog) SetConversation(conv *ConversationModel) {
	c.setConversation(conv, true)
}

func (c *ChatLog) SetConversationPreserveRender(conv *ConversationModel) {
	c.setConversation(conv, false)
}

func (c *ChatLog) setConversation(conv *ConversationModel, clearRendered bool) {
	if c == nil || conv == nil {
		return
	}
	unsubscribe := func() {}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	if c.unsubscribe != nil {
		unsubscribe = c.unsubscribe
	}
	c.conv = conv
	c.unsubscribe = conv.Subscribe(c.onChanged)
	c.pending = nil
	c.latestPrepared = nil
	c.notificationPending = false
	if c.activeCancel != nil {
		c.activeCancel()
		c.activeCancel = nil
	}
	c.revision++
	if clearRendered {
		c.renderedRevision = c.revision
		c.renderedState = chatLogRenderState{width: c.desiredWidth, snapshot: conv.Snapshot()}
		c.lines = nil
		c.lineCount = 0
		c.wrapWidth = 0
		c.selection = chatLogSelection{}
		c.visibleStart = 0
		c.visibleEnd = 0
	}
	if clearRendered {
		c.renderCache = nil
		c.renderCacheOrder = nil
		c.expanded = nil
	}
	c.mu.Unlock()

	unsubscribe()
	c.scheduleCurrentSnapshot()
}

func (c *ChatLog) Revision() uint64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.revision
}

func (c *ChatLog) ID() tauui.ID { return c.id }

func (c *ChatLog) Rect() tauui.Rect {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rect
}

// Layout records the app-loop-owned rectangle and schedules a new
// width-dependent render when the wrapping width changes.
func (c *ChatLog) Layout(r tauui.Rect) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.rect = r
	if r.W <= 0 {
		// Make any in-flight positive-width result obsolete. A later
		// transition back to the same width must schedule a fresh snapshot,
		// including mutations that happened while the view was hidden.
		if c.desiredWidth > 0 {
			c.revision++
		}
		c.desiredWidth = 0
		c.pending = nil
		c.latestPrepared = nil
		c.mu.Unlock()
		return
	}
	widthChanged := r.W != c.desiredWidth
	c.mu.Unlock()

	if widthChanged {
		c.scheduleCurrentSnapshot()
	}
}

// MinSize reports only the latest completed content height. It never wraps or
// waits for a pending worker request.
func (c *ChatLog) MinSize() tauui.Size {
	c.mu.Lock()
	h := c.lineCount
	c.mu.Unlock()
	if h < 1 {
		h = 1
	}
	return tauui.Size{W: 1, H: h}
}

// Handle lets users select chat text with a left-button drag. The owning
// ScrollView adjusts Y coordinates by scroll offset before delegating here, so
// the positions below are in ChatLog content coordinates rather than viewport
// coordinates.
func (c *ChatLog) Handle(e tauui.Event, ctx *tauui.Ctx) bool {
	me, ok := e.(tauui.MouseEvent)
	if !ok {
		return false
	}
	if me.Button != tauui.MouseButtonLeft {
		return false
	}

	switch me.Action {
	case tauui.MousePress:
		pos := c.positionFromMouse(me.X, me.Y)
		c.mu.Lock()
		switch {
		case me.ClickCount >= 3:
			c.selection = c.lineSelectionAtLocked(pos)
		case me.ClickCount == 2:
			c.selection = c.wordSelectionAtLocked(pos)
		default:
			c.selection = chatLogSelection{active: true, anchor: pos, cursor: pos, mode: chatLogSelectionModeRange}
		}
		rect := c.rect
		c.mu.Unlock()
		invalidateRect(ctx, rect)
		return true
	case tauui.MouseDrag:
		c.mu.Lock()
		if !c.selection.active {
			c.mu.Unlock()
			return false
		}
		c.selection.cursor = c.positionFromMouseLocked(me.X, me.Y)
		rect := c.rect
		c.mu.Unlock()
		invalidateRect(ctx, rect)
		return true
	case tauui.MouseRelease:
		c.mu.Lock()
		if !c.selection.active {
			c.mu.Unlock()
			return false
		}
		if c.selection.mode == chatLogSelectionModeRange {
			c.selection.cursor = c.positionFromMouseLocked(me.X, me.Y)
		}
		selectionText := c.selectedTextLocked()
		if selectionText == "" {
			c.selection = chatLogSelection{}
		}
		rect := c.rect
		c.mu.Unlock()
		if selectionText != "" && ctx != nil && ctx.ClipboardWrite != nil {
			ctx.ClipboardWrite(selectionText)
		}
		// A single click that selected nothing (no drag; multi-clicks produce a
		// non-empty word/line selection) toggles a truncated user message between
		// its capped preview and the full text. Rovel only populates ClickCount on
		// press, so the release-side count is 0 for a plain click.
		if me.ClickCount <= 1 && selectionText == "" {
			c.toggleExpandedAt(me.X, me.Y)
		}
		invalidateRect(ctx, rect)
		return true
	default:
		return false
	}
}

// Compile-time check: ChatLog exposes the toolkit RowRenderer surface for
// transcript previews, diagnostics, and headless rendering helpers. The
// interactive app still paints styled rows directly through Paint.
var _ tauui.RowRenderer = (*ChatLog)(nil)

// Render returns the latest completed chat transcript as plain logical rows.
// It intentionally does not wrap, wait for, or schedule worker work: ChatLog's
// renderer remains the source of truth and this method is only a top-level
// toolkit RowRenderer view over the cached result. Paint keeps the
// styled/full-row rendering path; Render is for transcript review, golden
// helpers, and row-width diagnostics.
func (c *ChatLog) Render(width int) []string {
	if c == nil || width <= 0 {
		return nil
	}
	c.mu.Lock()
	lines := append([]renderLine(nil), c.lines...)
	c.mu.Unlock()
	if len(lines) == 0 {
		return nil
	}
	rows := make([]string, 0, len(lines))
	for _, line := range lines {
		rows = append(rows, chatLogRenderRow(tauui.StripANSI(line.text), width))
	}
	return rows
}

// chatLogRenderRow lays one logical row into the chat width: truncated to fit,
// then padded so the row's background fill spans the full width.
func chatLogRenderRow(row string, width int) string {
	if width <= 0 {
		return ""
	}
	row = expandChatLogTabs(row)
	row = tauui.TextTruncate(row, width, false)
	rowWidth := tauui.TextWidth(row)
	if rowWidth >= width {
		return row
	}
	return row + strings.Repeat(" ", width-rowWidth)
}

// expandChatLogTabs replaces tab characters with the toolkit's three-space
// expansion. Rovel's cell renderer gives control characters zero width, so a
// raw tab is dropped without advancing the cursor and indented content
// collapses flush left. Toolkit text components normalize tabs the same way
// ("tabs render as three spaces", see normalizeRows and markdownPreprocess);
// the chat log's raw-line paths — bash output, diffs, previews, fenced code,
// and plugin rows — must expand them at the same boundary or their
// indentation never reaches the screen.
func expandChatLogTabs(s string) string {
	return strings.ReplaceAll(s, "\t", "   ")
}

// Paint draws only the latest completed immutable lines. It never wraps or
// waits for fresher data; stale completed content is acceptable while a newer
// request is pending.
func (c *ChatLog) Paint(d tauui.Drawer, ctx *tauui.Ctx) {
	c.mu.Lock()
	rect := c.rect
	lines := c.lines
	palette := *c.pal()
	selection := c.selection
	start, end := c.visibleStart, c.visibleEnd
	c.mu.Unlock()

	if rect.W <= 0 || rect.H <= 0 {
		return
	}
	if start < 0 {
		start = 0
	}
	totalLines := len(lines)
	visibleEnd := end
	if visibleEnd <= start {
		visibleEnd = totalLines
	}
	if visibleEnd < start {
		visibleEnd = start
	}
	if end <= start || end > totalLines {
		end = totalLines
	}
	if start > end {
		start = end
	}

	base := palette.spacer
	if ctx != nil {
		base = ctx.Theme.Base
	}
	fillRect := rect
	if visibleEnd > start {
		fillRect.Y = rect.Y + start
		fillRect.H = visibleEnd - start
	}
	d.FillRect(fillRect, base)

	for i := start; i < end; i++ {
		if i < 0 || i >= len(lines) {
			continue
		}
		ln := lines[i]
		y := rect.Y + i
		st := ln.st
		if st.BG == tauui.ColorDefault {
			st.BG = base.BG
		}
		if st.FG == tauui.ColorDefault {
			st.FG = base.FG
		}
		row := tauui.Rect{X: rect.X, Y: y, W: rect.W, H: 1}
		d.FillRect(row, st)
		tauui.DrawANSIText(d, tauui.Point{X: rect.X, Y: y}, ln.text, st)
		c.paintSelectionForLine(d, rect, i, ln, selection, st, ctx)
	}
}

// setVisibleRows records the content rows the owning scroll viewport is about
// to show, so Paint can draw only that window instead of every cached row.
func (c *ChatLog) setVisibleRows(start, end int) {
	c.mu.Lock()
	c.visibleStart = start
	c.visibleEnd = end
	c.mu.Unlock()
}

// onChanged captures model state synchronously but never performs wrapping or
// invalidation. The worker's completed result is the only thing that wakes the
// app for a chat repaint.
func (c *ChatLog) onChanged() {
	c.scheduleCurrentSnapshot()
}

func (c *ChatLog) scheduleCurrentSnapshot() {
	// Snapshot already returns a freshly built slice that no one else retains,
	// so the request can reference it directly. Copying it here doubled the
	// per-mutation allocation for the whole transcript.
	snapshot := c.conv.Snapshot()
	components := snapshot.Components

	c.mu.Lock()
	if c.closed || c.rect.W <= 0 {
		c.mu.Unlock()
		return
	}
	width := c.rect.W
	if width < 1 {
		width = 1
	}
	c.revision++
	c.desiredWidth = width
	if c.activeCancel != nil {
		c.activeCancel()
	}
	c.pending = &chatLogRenderRequest{
		revision:   c.revision,
		width:      width,
		snapshot:   snapshot,
		components: components,
		wrap:       c.wrapMessagesFunc,
	}
	c.mu.Unlock()

	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *ChatLog) renderWorker(ctx context.Context) {
	defer close(c.done)

	var previous *chatLogRenderState
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
		}

		for {
			req, ok := c.takePendingRequest()
			if !ok {
				break
			}
			if ctx.Err() != nil {
				return
			}

			reqCtx, cancelReq := context.WithCancel(ctx)
			state, incremental := c.tryIncrementalComponentRender(previous, req.snapshot, req.components, req.width)
			if !incremental {
				c.setActiveRenderCancel(cancelReq)
				state = req.wrap(reqCtx, req.components, req.width)
				c.clearActiveRenderCancel(cancelReq)
				c.recordFullRender(reqCtx.Err() != nil)
			} else {
				c.recordIncrementalRender()
			}
			cancelReq()
			if ctx.Err() != nil {
				return
			}
			if reqCtx.Err() != nil && c.hasPendingRequest() {
				continue
			}
			previous = &state
			c.publishRenderResult(chatLogRenderResult{
				revision: req.revision,
				state:    state,
			})
		}
	}
}

func (c *ChatLog) takePendingRequest() (chatLogRenderRequest, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.pending == nil {
		return chatLogRenderRequest{}, false
	}
	req := *c.pending
	c.pending = nil
	return req, true
}

func (c *ChatLog) setActiveRenderCancel(cancel context.CancelFunc) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		return
	}
	c.activeCancel = cancel
	c.mu.Unlock()
}

func (c *ChatLog) clearActiveRenderCancel(cancel context.CancelFunc) {
	c.mu.Lock()
	if c.activeCancel != nil {
		c.activeCancel = nil
	}
	c.mu.Unlock()
}

func (c *ChatLog) hasPendingRequest() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pending != nil
}

func (c *ChatLog) recordFullRender(cancelled bool) {
	c.mu.Lock()
	c.metrics.FullRenderCount++
	if cancelled {
		c.metrics.CancelledRenderCount++
	}
	c.mu.Unlock()
}

func (c *ChatLog) recordIncrementalRender() {
	c.mu.Lock()
	c.metrics.IncrementalRenderCount++
	c.mu.Unlock()
}

func (c *ChatLog) publishRenderResult(result chatLogRenderResult) {
	c.mu.Lock()
	c.publishRenderResultLocked(result)
	c.mu.Unlock()
}

func (c *ChatLog) publishRenderResultLocked(result chatLogRenderResult) {
	if c.closed || result.state.width != c.desiredWidth {
		return
	}
	if result.revision <= c.renderedRevision {
		return
	}
	if c.latestPrepared == nil || result.revision > c.latestPrepared.revision {
		c.latestPrepared = &result
	}
	notify := c.notifyRenderReady
	shouldNotify := notify != nil && !c.notificationPending
	if shouldNotify {
		c.notificationPending = true
	}
	if shouldNotify {
		go c.sendRenderReady(notify)
	}
}

func (c *ChatLog) sendRenderReady(notify chatLogRenderNotifier) {
	if err := notify(); err != nil {
		c.mu.Lock()
		// A failed post did not consume the prepared result. Clear the
		// outstanding bit so a replacement notifier could retry it.
		c.notificationPending = false
		c.mu.Unlock()
	}
}

// applyPreparedRender must run on the app loop in production. It commits the
// newest coalesced prepared result and reports the invalidation work the owner
// should perform after the commit.
func (c *ChatLog) applyPreparedRender() chatLogRenderUpdate {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.notificationPending = false
	result := c.latestPrepared
	c.latestPrepared = nil
	if c.closed || result == nil || result.state.width != c.desiredWidth {
		return chatLogRenderUpdate{}
	}
	if result.revision <= c.renderedRevision {
		return chatLogRenderUpdate{}
	}

	oldLineCount := c.lineCount
	oldWrapWidth := c.wrapWidth
	c.renderedState = result.state
	c.lines = result.state.lines
	c.lineCount = len(result.state.lines)
	c.wrapWidth = result.state.width
	c.renderedRevision = result.revision
	if c.selection.active {
		c.selection.anchor = clampChatLogPosition(c.selection.anchor, c.lines)
		c.selection.cursor = clampChatLogPosition(c.selection.cursor, c.lines)
	}

	newLineCount := c.lineCount
	return chatLogRenderUpdate{
		revision:      c.renderedRevision,
		installed:     true,
		current:       c.renderedRevision == c.revision,
		rect:          c.rect,
		layoutChanged: oldLineCount != newLineCount || oldWrapWidth != c.wrapWidth,
		oldLineCount:  oldLineCount,
		newLineCount:  newLineCount,
	}
}

// Close stops and joins the worker and unregisters the model listener. It is
// safe to call repeatedly and concurrently.
func (c *ChatLog) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.pending = nil
		c.latestPrepared = nil
		if c.activeCancel != nil {
			c.activeCancel()
			c.activeCancel = nil
		}
		c.mu.Unlock()

		c.unsubscribe()
		c.cancel()
	})
	<-c.done
	return nil
}

func (c *ChatLog) positionFromMouse(x, y int) chatLogPosition {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.positionFromMouseLocked(x, y)
}

func (c *ChatLog) positionFromMouseLocked(x, y int) chatLogPosition {
	line := y - c.rect.Y
	if line < 0 {
		line = 0
	}
	if len(c.lines) == 0 {
		return chatLogPosition{}
	}
	if line >= len(c.lines) {
		line = len(c.lines) - 1
	}

	col := x - c.rect.X
	if col < 0 {
		col = 0
	}
	// Selection positions are stored in visible cells so the cursor lands
	// where the user actually sees it, even when the rendered row carries
	// embedded ANSI SGR sequences for inline-code, bold, or italic spans.
	maxCol := tauui.TextWidth(c.lines[line].text)
	if col > maxCol {
		col = maxCol
	}
	return chatLogPosition{line: line, col: col}
}

func clampChatLogPosition(pos chatLogPosition, lines []renderLine) chatLogPosition {
	if len(lines) == 0 {
		return chatLogPosition{}
	}
	if pos.line < 0 {
		pos.line = 0
	}
	if pos.line >= len(lines) {
		pos.line = len(lines) - 1
	}
	maxCol := tauui.TextWidth(lines[pos.line].text)
	if pos.col < 0 {
		pos.col = 0
	}
	if pos.col > maxCol {
		pos.col = maxCol
	}
	return pos
}

func normalizedChatLogSelection(sel chatLogSelection) (chatLogPosition, chatLogPosition, bool) {
	if !sel.active || sel.anchor == sel.cursor {
		return chatLogPosition{}, chatLogPosition{}, false
	}
	start := sel.anchor
	end := sel.cursor
	if end.line < start.line || (end.line == start.line && end.col < start.col) {
		start, end = end, start
	}
	return start, end, true
}

func (c *ChatLog) wordSelectionAtLocked(pos chatLogPosition) chatLogSelection {
	pos = clampChatLogPosition(pos, c.lines)
	if len(c.lines) == 0 || pos.line < 0 || pos.line >= len(c.lines) {
		return chatLogSelection{}
	}

	lineText := c.lines[pos.line].text
	width := tauui.TextWidth(lineText)
	if width == 0 {
		return chatLogSelection{}
	}
	// Walk the visible runes for word-boundary detection so embedded ANSI
	// escapes (which mark styled spans) do not become part of the word.
	visible := []rune(tauui.StripANSI(lineText))
	col := pos.col
	if col >= width && col > 0 {
		col = width - 1
	}
	if col < 0 || col >= width || unicode.IsSpace(visible[col]) {
		return chatLogSelection{}
	}

	start := col
	for start > 0 && !unicode.IsSpace(visible[start-1]) {
		start--
	}
	end := col + 1
	for end < width && !unicode.IsSpace(visible[end]) {
		end++
	}
	return chatLogSelection{
		active: true,
		anchor: chatLogPosition{line: pos.line, col: start},
		cursor: chatLogPosition{line: pos.line, col: end},
		mode:   chatLogSelectionModeWord,
	}
}

func (c *ChatLog) lineSelectionAtLocked(pos chatLogPosition) chatLogSelection {
	pos = clampChatLogPosition(pos, c.lines)
	if len(c.lines) == 0 || pos.line < 0 || pos.line >= len(c.lines) {
		return chatLogSelection{}
	}
	lineText := c.lines[pos.line].text
	lineStart := 0
	if strings.HasPrefix(lineText, " ") {
		lineStart = 1
	}
	// lineEnd is the visible-cell length of the row, not the rune count, so
	// trailing inline-code/bold/italic ANSI sequences do not push the cursor
	// past the last visible character.
	lineEnd := tauui.TextWidth(lineText)
	return chatLogSelection{
		active: true,
		anchor: chatLogPosition{line: pos.line, col: lineStart},
		cursor: chatLogPosition{line: pos.line, col: lineEnd},
		mode:   chatLogSelectionModeLine,
	}
}

func (c *ChatLog) selectedTextLocked() string {
	start, end, ok := normalizedChatLogSelection(c.selection)
	if !ok || len(c.lines) == 0 {
		return ""
	}
	start = clampChatLogPosition(start, c.lines)
	end = clampChatLogPosition(end, c.lines)
	if start == end {
		return ""
	}

	var b strings.Builder
	for line := start.line; line <= end.line; line++ {
		lineStart := 0
		lineEnd := tauui.TextWidth(c.lines[line].text)
		if line == start.line {
			lineStart = start.col
		}
		if line == end.line {
			lineEnd = end.col
		}
		if lineStart < lineEnd {
			// ANSISliceStrict returns the visible-cell slice with any SGR
			// sequences that landed inside it preserved; StripANSI then
			// removes the escapes so the clipboard receives plain text.
			b.WriteString(tauui.StripANSI(tauui.ANSISliceStrict(c.lines[line].text, lineStart, lineEnd)))
		}
		if line != end.line {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (c *ChatLog) paintSelectionForLine(d tauui.Drawer, rect tauui.Rect, line int, ln renderLine, sel chatLogSelection, rowStyle tauui.Style, ctx *tauui.Ctx) {
	start, end, ok := normalizedChatLogSelection(sel)
	if !ok || line < start.line || line > end.line {
		return
	}
	from := 0
	to := tauui.TextWidth(ln.text)
	if line == start.line {
		from = start.col
	}
	if line == end.line {
		to = end.col
	}
	if from >= to {
		return
	}

	// ANSISliceStrict preserves the styled spans so the highlight keeps the
	// same colours as the underlying text rather than collapsing them to
	// plain reverse-video.
	selected := tauui.ANSISliceStrict(ln.text, from, to)
	selectionStyle := rowStyle.WithAttr(tauui.AttrReverse)
	if ctx != nil && ctx.Theme.Palette.Selection != (tauui.Style{}) {
		selectionStyle = ctx.Theme.Palette.Selection
	}
	// DrawANSIText advances x by visible cells, which keeps the highlight
	// aligned with the underlying text even when the slice carries embedded
	// SGR sequences that DrawText would otherwise render as literal cells.
	tauui.DrawANSIText(d, tauui.Point{X: rect.X + from, Y: rect.Y + line}, selected, selectionStyle)
}

func invalidateRect(ctx *tauui.Ctx, rect tauui.Rect) {
	if ctx != nil && ctx.Invalidate != nil && rect.W > 0 && rect.H > 0 {
		ctx.Invalidate(rect)
	}
}

const maxChatLogRenderCacheEntries = 4096

// Metrics returns a snapshot of render instrumentation for tests/debugging.
func (c *ChatLog) Metrics() ChatLogRenderMetrics {
	c.mu.Lock()
	defer c.mu.Unlock()
	metrics := c.metrics
	metrics.CacheEntries = len(c.renderCache)
	return metrics
}

// wrapComponents builds a structured render state for an immutable component
// snapshot at width. Spans let later updates replace only component rows instead
// of rewrapping the full transcript.
// wrapLineHint estimates the height of a wrap covering componentCount
// components, from the height and component count of the last completed wrap.
// It is only used to pre-size the destination slice, so being off just costs a
// reallocation. Scaling by component count matters because a chunk prepend
// wraps a small slice of the transcript and should not reserve room for all of
// it.
func (c *ChatLog) wrapLineHint(componentCount int) int {
	c.mu.Lock()
	lines := c.lineCount
	previous := len(c.renderedState.components)
	c.mu.Unlock()
	if lines <= 0 || previous <= 0 || componentCount <= 0 {
		return 0
	}
	if componentCount >= previous {
		return lines
	}
	return lines * componentCount / previous
}

func (c *ChatLog) wrapComponents(ctx context.Context, components []ChatComponent, width int) chatLogRenderState {
	if len(components) == 0 {
		// An empty transcript renders empty. The startup banner is the Header
		// region's job (app.md §9 region 1); it used to live here, which is
		// why it had to vanish on the first message rather than scroll away.
		return chatLogRenderState{width: width}
	}
	state := chatLogRenderState{
		width:      width,
		components: components,
		messages:   messagesFromComponents(components),
		spans:      make([]chatLogMessageSpan, len(components)),
		// Start from the height the last completed wrap produced. A transcript
		// re-wrap lands within a line or two of it, so this turns a chain of
		// growslice reallocations (each copying every line rendered so far)
		// into a single allocation.
		lines: make([]renderLine, 0, c.wrapLineHint(len(components))),
	}
	seenUser := false
	for i := range components {
		if ctx.Err() != nil {
			return chatLogRenderState{}
		}
		start := len(state.lines)
		messageLines, tailLine, tailByte := c.renderComponent(ctx, components[i], width)
		if components[i].Kind == ChatComponentUser {
			if seenUser {
				messageLines = append([]renderLine{{text: " ", st: c.pal().spacer}}, messageLines...)
				tailLine++
			}
			seenUser = true
		}
		if ctx.Err() != nil {
			return chatLogRenderState{}
		}
		state.lines = append(state.lines, messageLines...)
		state.spans[i] = chatLogMessageSpan{
			start:     start,
			end:       len(state.lines),
			tailStart: start + tailLine,
			tailByte:  tailByte,
		}
	}
	if len(state.lines) == 0 {
		state.lines = []renderLine{{text: "", st: c.pal().spacer}}
	}
	return state
}

// wrapMessages is a compatibility helper for tests and legacy callers that
// still construct flat message slices directly.
func (c *ChatLog) wrapMessages(ctx context.Context, messages []Message, width int) chatLogRenderState {
	return c.wrapComponents(ctx, componentsFromMessages(messages), width)
}

// tryIncrementalRender handles the common streaming case: one existing
// assistant message grew by appending text at the same width. It retains all
// completed logical lines and rewraps only the active final logical line plus
// newly appended text. Structural mutations, finalization, reordering, and
// width changes fall back to wrapMessages.
func messagesFromComponents(components []ChatComponent) []Message {
	messages := make([]Message, len(components))
	for i, component := range components {
		messages[i] = component.Message()
	}
	return messages
}

func componentsFromMessages(messages []Message) []ChatComponent {
	components := make([]ChatComponent, len(messages))
	for i, msg := range messages {
		components[i] = componentFromMessage(msg, uint64(i+1))
	}
	return components
}

func updatedComponentsForMessages(previous []ChatComponent, messages []Message) []ChatComponent {
	components := make([]ChatComponent, len(messages))
	for i, msg := range messages {
		revision := uint64(i + 1)
		if i < len(previous) {
			revision = previous[i].Revision
		}
		components[i] = componentFromMessage(msg, revision)
	}
	return components
}

func componentFromMessage(msg Message, revision uint64) ChatComponent {
	component := ChatComponent{ID: msg.ID, Kind: kindForRole(msg.Role), Role: msg.Role, Text: msg.Text, Streaming: msg.Streaming, Revision: revision, Images: append([]ImageComponent(nil), msg.Images...)}
	if msg.Role == RoleToolCall || msg.Role == RoleToolResult {
		state := toolExecutionExecuting
		if msg.Role == RoleToolResult {
			state = toolExecutionComplete
		}
		output := ""
		if msg.Role == RoleToolResult {
			output = msg.Text
		} else if _, rest, ok := strings.Cut(msg.Text, "\n"); ok {
			output = rest
		}
		component.Kind = ChatComponentToolExecution
		component.ToolExecution = ToolExecutionComponent{CallID: msg.ToolCallID, Name: msg.ToolName, Args: msg.ToolArgs, Output: output, State: state, IsError: msg.IsError}
	}
	if msg.Role == RoleError {
		component.ToolExecution.IsError = msg.IsError
	}
	return component
}

// renderComponent returns the component's wrapped lines, from the cache when
// possible. The returned slice must be treated as read-only: on a cache hit it
// aliases the cached entry so a re-wrap of an unchanged transcript costs no
// per-component copy. Callers may append it into another slice or prepend to a
// fresh one, but must never write through it.
func (c *ChatLog) renderComponent(ctx context.Context, component ChatComponent, width int) ([]renderLine, int, int) {
	if ctx.Err() != nil {
		return nil, 0, 0
	}
	key := renderCacheKey{componentID: component.ID, revision: component.Revision, width: width}
	if key.componentID != "" && key.revision != 0 {
		c.mu.Lock()
		key.expanded = c.expanded[component.ID]
		entry, ok := c.renderCache[key]
		if ok {
			c.metrics.CacheHits++
		} else {
			c.metrics.CacheMisses++
		}
		c.mu.Unlock()
		if ok {
			return entry.lines, entry.tailStart, entry.tailByte
		}
	}
	lines, tailLine, tailByte := c.renderComponentUncached(ctx, component, width)
	if ctx.Err() != nil {
		return nil, 0, 0
	}
	if key.componentID != "" && key.revision != 0 {
		c.mu.Lock()
		if c.renderCache == nil {
			c.renderCache = make(map[renderCacheKey]renderCacheEntry)
		}
		if _, exists := c.renderCache[key]; !exists {
			c.renderCacheOrder = append(c.renderCacheOrder, key)
		}
		c.renderCache[key] = renderCacheEntry{lines: append([]renderLine(nil), lines...), tailStart: tailLine, tailByte: tailByte}
		for len(c.renderCacheOrder) > maxChatLogRenderCacheEntries {
			evict := c.renderCacheOrder[0]
			c.renderCacheOrder = c.renderCacheOrder[1:]
			if _, ok := c.renderCache[evict]; ok {
				delete(c.renderCache, evict)
				c.metrics.CacheEvictions++
			}
		}
		c.metrics.CacheEntries = len(c.renderCache)
		c.metrics.RenderedComponentCount++
		c.metrics.RenderedLineCount += uint64(len(lines))
		c.mu.Unlock()
	}
	return lines, tailLine, tailByte
}

func (c *ChatLog) renderComponentUncached(ctx context.Context, component ChatComponent, width int) ([]renderLine, int, int) {
	if rows, ok, consumed := c.applyPluginComponent(component, width); ok {
		return rows, consumed, consumed
	}
	switch component.Kind {
	case ChatComponentAssistant:
		return c.renderAssistantComponent(ctx, component, width)
	case ChatComponentThinking:
		return c.renderThinkingComponent(ctx, component, width)
	case ChatComponentToolExecution:
		return c.renderToolExecutionComponent(ctx, component, width)
	case ChatComponentImage:
		return c.renderImageComponent(ctx, component, width)
	case ChatComponentError:
		return c.renderErrorComponent(ctx, component, width)
	case ChatComponentSystem:
		return c.renderSystemComponent(ctx, component, width)
	case ChatComponentUser:
		return c.renderUserComponent(ctx, component, width)
	case ChatComponentCompactionSummary:
		return c.renderLabelledSummary(ctx, component, compactionLabel, width)
	default:
		return c.renderCustomComponent(ctx, component, width)
	}
}

// compactionLabel is the provenance tag on a compaction summary. The chat
// palette has carried a customLabel style for `[compaction]`/`[branch]`/`[skill]`
// since the theme work; until a labelled component existed it styled nothing.
const compactionLabel = "[compaction]"

// renderLabelledSummary draws a summary block: a bold label row, the header
// line, then the body — all on the custom-message background, so it reads as
// app-generated provenance rather than as something the model said.
func (c *ChatLog) renderLabelledSummary(ctx context.Context, component ChatComponent, label string, width int) ([]renderLine, int, int) {
	body := component.Text
	if strings.TrimSpace(body) == "" {
		body = component.Custom.FallbackText()
	}
	rows := []string{label}
	if title := strings.TrimSpace(component.Custom.Title); title != "" {
		rows = append(rows, title, "")
	}
	rows = append(rows, strings.Split(strings.TrimRight(body, "\n"), "\n")...)

	lines := []renderLine{{text: " ", st: c.pal().spacer}}
	lines = c.appendPaddedTextBox(ctx, lines, rows, width, c.pal().custom)
	// The label row is the first content row inside the box: one leading blank
	// row from appendPaddedTextBox, plus the spacer this function opened with.
	if len(lines) > 2 {
		lines[2].st = c.pal().customLabel
	}
	return lines, len(lines), len(body)
}

// applyPluginComponent lets registered plugin renderers replace the
// default projection for one component. The replace flag means the
// returned rows fully describe the component's display; the chat log
// installs them and the built-in path is skipped.
func (c *ChatLog) applyPluginComponent(component ChatComponent, width int) ([]renderLine, bool, int) {
	c.mu.Lock()
	r := c.pluginRenderers
	c.mu.Unlock()
	if r == nil {
		return nil, false, 0
	}
	rows, replace := r.applyPluginRenderers(component, width)
	if !replace || len(rows) == 0 {
		return nil, false, 0
	}
	out := make([]renderLine, 0, len(rows))
	for _, raw := range rows {
		text := raw
		// Defensively pad each row to the visible width so plugin
		// renderers that don't pad themselves still render inside
		// the chat viewport. The chat log paints a background fill
		// per row so the trailing spaces carry the row's BG style.
		if width > 0 {
			text = chatLogRenderRow(text, width)
		}
		out = append(out, renderLine{text: text, st: c.pal().spacer})
	}
	tailByte := len(component.Message().Text)
	if tailByte == 0 {
		tailByte = len(out)
	}
	return out, true, tailByte
}

func (c *ChatLog) tryIncrementalComponentRender(previous *chatLogRenderState, snapshot ChatSnapshot, components []ChatComponent, width int) (chatLogRenderState, bool) {
	if previous == nil || previous.width != width || len(previous.spans) != len(previous.components) || len(previous.messages) != len(previous.components) {
		return chatLogRenderState{}, false
	}
	if len(components) == 0 && len(previous.components) == 0 {
		return chatLogRenderState{}, false
	}

	messages := messagesFromComponents(components)
	var state chatLogRenderState
	var ok bool
	switch {
	case componentPrefixEqual(previous.components, components, len(previous.components)) && len(components) > len(previous.components):
		state, ok = c.tryIncrementalComponentAppend(previous, snapshot, components, messages, width)
	case len(components) == len(previous.components):
		state, ok = c.tryIncrementalComponentUpdate(previous, snapshot, components, messages, width)
	case len(components)+1 == len(previous.components):
		state, ok = c.tryIncrementalComponentRemoval(previous, snapshot, components, messages, width)
	default:
		return chatLogRenderState{}, false
	}
	if !ok {
		return chatLogRenderState{}, false
	}
	state.snapshot = snapshot
	state.components = components
	state.messages = messages
	return state, true
}

func componentPrefixEqual(previous, next []ChatComponent, n int) bool {
	if n < 0 || len(previous) < n || len(next) < n {
		return false
	}
	for i := range n {
		if !sameComponentRevision(previous[i], next[i]) {
			return false
		}
	}
	return true
}

func sameComponentRevision(a, b ChatComponent) bool {
	if a.ID != "" || b.ID != "" {
		return a.ID == b.ID && a.Revision == b.Revision
	}
	return a.Revision == b.Revision && sameMessage(a.Message(), b.Message())
}

func sameMessage(a, b Message) bool {
	return a.ID == b.ID &&
		a.Role == b.Role &&
		a.Text == b.Text &&
		a.Streaming == b.Streaming &&
		a.ToolName == b.ToolName &&
		a.ToolArgs == b.ToolArgs &&
		a.ToolCallID == b.ToolCallID &&
		a.IsError == b.IsError &&
		slices.Equal(a.Images, b.Images)
}

func sameComponentIdentity(a, b ChatComponent) bool {
	if a.ID != "" || b.ID != "" {
		return a.ID == b.ID
	}
	return a.Kind == b.Kind && a.Role == b.Role && a.ToolExecution.CallID == b.ToolExecution.CallID
}

func (c *ChatLog) tryIncrementalComponentAppend(previous *chatLogRenderState, snapshot ChatSnapshot, components []ChatComponent, messages []Message, width int) (chatLogRenderState, bool) {
	state := chatLogRenderState{
		width:      width,
		snapshot:   snapshot,
		components: components,
		messages:   messages,
		lines:      previous.lines,
		spans:      append(make([]chatLogMessageSpan, 0, len(components)), previous.spans...),
	}
	if len(previous.components) == 0 {
		state.lines = nil
	}
	seenUser := componentsContainUser(previous.components)
	for i := len(previous.components); i < len(components); i++ {
		start := len(state.lines)
		messageLines, tailLine, tailByte := c.renderComponent(context.Background(), components[i], width)
		if components[i].Kind == ChatComponentUser {
			if seenUser {
				messageLines = append([]renderLine{{text: " ", st: c.pal().spacer}}, messageLines...)
				tailLine++
			}
			seenUser = true
		}
		state.lines = append(state.lines, messageLines...)
		state.spans = append(state.spans, chatLogMessageSpan{
			start:     start,
			end:       len(state.lines),
			tailStart: start + tailLine,
			tailByte:  tailByte,
		})
	}
	if len(state.lines) == 0 {
		state.lines = []renderLine{{text: "", st: c.pal().spacer}}
	}
	return state, true
}

func componentsContainUser(components []ChatComponent) bool {
	for _, component := range components {
		if component.Kind == ChatComponentUser {
			return true
		}
	}
	return false
}

func (c *ChatLog) tryIncrementalComponentUpdate(previous *chatLogRenderState, snapshot ChatSnapshot, components []ChatComponent, messages []Message, width int) (chatLogRenderState, bool) {
	changed := -1
	for i := range components {
		if sameComponentRevision(previous.components[i], components[i]) {
			continue
		}
		if !sameComponentIdentity(previous.components[i], components[i]) || changed >= 0 {
			return chatLogRenderState{}, false
		}
		changed = i
	}
	if changed < 0 {
		return chatLogRenderState{}, false
	}

	oldComponent := previous.components[changed]
	newComponent := components[changed]
	oldMessage := oldComponent.Message()
	newMessage := newComponent.Message()
	if oldMessage.Role == RoleAssistant && newMessage.Role == RoleAssistant &&
		oldMessage.Streaming && !newMessage.Streaming && oldMessage.Text == newMessage.Text && newMessage.Text != "" &&
		oldMessage.ToolName == newMessage.ToolName && oldMessage.ToolArgs == newMessage.ToolArgs && oldMessage.IsError == newMessage.IsError {
		state := *previous
		state.snapshot = snapshot
		state.components = components
		state.messages = messages
		state.spans = append([]chatLogMessageSpan(nil), previous.spans...)
		return state, true
	}

	if oldMessage.Role == RoleAssistant && newMessage.Role == RoleAssistant &&
		oldMessage.Streaming && newMessage.Streaming &&
		oldMessage.ToolName == newMessage.ToolName && oldMessage.ToolArgs == newMessage.ToolArgs && oldMessage.IsError == newMessage.IsError &&
		strings.HasPrefix(newMessage.Text, oldMessage.Text) {
		return c.tryIncrementalAssistantComponentAppend(previous, snapshot, components, messages, width, changed, oldMessage, newMessage)
	}

	return c.tryIncrementalComponentReplace(previous, snapshot, components, messages, width, changed)
}

func (c *ChatLog) tryIncrementalAssistantComponentAppend(previous *chatLogRenderState, snapshot ChatSnapshot, components []ChatComponent, messages []Message, width int, changed int, oldMessage, newMessage Message) (chatLogRenderState, bool) {
	span := previous.spans[changed]
	if span.start < 0 || span.tailStart < span.start || span.end < span.tailStart || span.end > len(previous.lines) ||
		span.tailByte < 0 || span.tailByte > len(oldMessage.Text) || span.tailByte > len(newMessage.Text) {
		return chatLogRenderState{}, false
	}

	tailText := newMessage.Text[span.tailByte:]
	tailLines, tailLine, tailByte := c.renderAssistantTextTail(context.Background(), tailText, newMessage.Streaming, width, chatLogFenceStateBefore(newMessage.Text, span.tailByte))
	delta := len(tailLines) - (span.end - span.tailStart)

	state := chatLogRenderState{
		width:      width,
		snapshot:   snapshot,
		components: components,
		messages:   messages,
		lines:      make([]renderLine, 0, len(previous.lines)+delta),
		spans:      append([]chatLogMessageSpan(nil), previous.spans...),
	}
	state.lines = append(state.lines, previous.lines[:span.tailStart]...)
	state.lines = append(state.lines, tailLines...)
	state.lines = append(state.lines, previous.lines[span.end:]...)
	state.spans[changed] = chatLogMessageSpan{
		start:     span.start,
		end:       span.end + delta,
		tailStart: span.tailStart + tailLine,
		tailByte:  span.tailByte + tailByte,
	}
	shiftSpans(state.spans, changed+1, delta)
	return state, true
}

func (c *ChatLog) tryIncrementalComponentReplace(previous *chatLogRenderState, snapshot ChatSnapshot, components []ChatComponent, messages []Message, width int, changed int) (chatLogRenderState, bool) {
	if changed < 0 || changed >= len(components) || changed >= len(previous.spans) {
		return chatLogRenderState{}, false
	}
	span := previous.spans[changed]
	if span.start < 0 || span.end < span.start || span.end > len(previous.lines) {
		return chatLogRenderState{}, false
	}

	messageLines, tailLine, tailByte := c.renderComponent(context.Background(), components[changed], width)
	delta := len(messageLines) - (span.end - span.start)
	state := chatLogRenderState{
		width:      width,
		snapshot:   snapshot,
		components: components,
		messages:   messages,
		lines:      make([]renderLine, 0, len(previous.lines)+delta),
		spans:      append([]chatLogMessageSpan(nil), previous.spans...),
	}
	state.lines = append(state.lines, previous.lines[:span.start]...)
	state.lines = append(state.lines, messageLines...)
	state.lines = append(state.lines, previous.lines[span.end:]...)
	state.spans[changed] = chatLogMessageSpan{start: span.start, end: span.start + len(messageLines), tailStart: span.start + tailLine, tailByte: tailByte}
	shiftSpans(state.spans, changed+1, delta)
	if len(state.lines) == 0 {
		state.lines = []renderLine{{text: "", st: c.pal().spacer}}
	}
	return state, true
}

func (c *ChatLog) tryIncrementalComponentRemoval(previous *chatLogRenderState, snapshot ChatSnapshot, components []ChatComponent, messages []Message, width int) (chatLogRenderState, bool) {
	removed := -1
	for i := range previous.components {
		if i < len(components) && sameComponentRevision(previous.components[i], components[i]) {
			continue
		}
		removed = i
		break
	}
	if removed < 0 {
		return chatLogRenderState{}, false
	}
	for i := removed; i < len(components); i++ {
		if !sameComponentRevision(previous.components[i+1], components[i]) {
			return chatLogRenderState{}, false
		}
	}
	span := previous.spans[removed]
	if span.start < 0 || span.end < span.start || span.end > len(previous.lines) {
		return chatLogRenderState{}, false
	}
	delta := span.start - span.end
	state := chatLogRenderState{
		width:      width,
		snapshot:   snapshot,
		components: components,
		messages:   messages,
		lines:      make([]renderLine, 0, len(previous.lines)+delta),
		spans:      make([]chatLogMessageSpan, 0, len(components)),
	}
	state.lines = append(state.lines, previous.lines[:span.start]...)
	state.lines = append(state.lines, previous.lines[span.end:]...)
	state.spans = append(state.spans, previous.spans[:removed]...)
	for _, shifted := range previous.spans[removed+1:] {
		shifted.start += delta
		shifted.end += delta
		shifted.tailStart += delta
		state.spans = append(state.spans, shifted)
	}
	if len(state.lines) == 0 {
		state.lines = []renderLine{{text: "", st: c.pal().spacer}}
	}
	return state, true
}

func shiftSpans(spans []chatLogMessageSpan, start, delta int) {
	for i := start; i < len(spans); i++ {
		spans[i].start += delta
		spans[i].end += delta
		spans[i].tailStart += delta
	}
}

func (c *ChatLog) tryIncrementalRender(previous *chatLogRenderState, messages []Message, width int) (chatLogRenderState, bool) {
	if previous == nil || previous.width != width || len(previous.spans) != len(previous.messages) || len(messages) < len(previous.messages) {
		return chatLogRenderState{}, false
	}

	for i := range previous.messages {
		if !sameMessage(previous.messages[i], messages[i]) {
			return c.tryIncrementalMessageUpdate(previous, messages, width)
		}
	}
	if len(messages) == len(previous.messages) {
		return chatLogRenderState{}, false
	}
	return c.tryIncrementalAppend(previous, messages, width)
}

func (c *ChatLog) tryIncrementalAppend(previous *chatLogRenderState, messages []Message, width int) (chatLogRenderState, bool) {
	state := chatLogRenderState{
		width:      width,
		components: updatedComponentsForMessages(previous.components, messages),
		messages:   messages,
		lines:      previous.lines,
		spans:      append(make([]chatLogMessageSpan, 0, len(messages)), previous.spans...),
	}
	if len(previous.messages) == 0 && len(state.lines) == 1 && state.lines[0].text == "" {
		state.lines = nil
	}
	for i := len(previous.messages); i < len(messages); i++ {
		start := len(state.lines)
		component := componentFromMessage(messages[i], uint64(i+1))
		messageLines, tailLine, tailByte := c.renderComponent(context.Background(), component, width)
		state.lines = append(state.lines, messageLines...)
		state.spans = append(state.spans, chatLogMessageSpan{
			start:     start,
			end:       len(state.lines),
			tailStart: start + tailLine,
			tailByte:  tailByte,
		})
	}
	if len(state.lines) == 0 {
		state.lines = []renderLine{{text: "", st: c.pal().spacer}}
	}
	return state, true
}

func (c *ChatLog) tryIncrementalMessageUpdate(previous *chatLogRenderState, messages []Message, width int) (chatLogRenderState, bool) {
	if len(previous.messages) != len(messages) || len(previous.spans) != len(messages) {
		return chatLogRenderState{}, false
	}

	changed := -1
	for i := range messages {
		if sameMessage(previous.messages[i], messages[i]) {
			continue
		}
		if changed >= 0 {
			return chatLogRenderState{}, false
		}
		changed = i
	}
	if changed < 0 {
		return chatLogRenderState{}, false
	}

	oldMessage := previous.messages[changed]
	newMessage := messages[changed]
	if oldMessage.Role == RoleAssistant && newMessage.Role == RoleAssistant &&
		oldMessage.Streaming && !newMessage.Streaming && oldMessage.Text == newMessage.Text && newMessage.Text != "" &&
		oldMessage.ToolName == newMessage.ToolName && oldMessage.ToolArgs == newMessage.ToolArgs && oldMessage.IsError == newMessage.IsError {
		state := *previous
		state.messages = messages
		state.spans = append([]chatLogMessageSpan(nil), previous.spans...)
		return state, true
	}

	if oldMessage.Role == RoleAssistant && newMessage.Role == RoleAssistant &&
		oldMessage.Streaming && newMessage.Streaming &&
		oldMessage.ToolName == newMessage.ToolName && oldMessage.ToolArgs == newMessage.ToolArgs && oldMessage.IsError == newMessage.IsError &&
		strings.HasPrefix(newMessage.Text, oldMessage.Text) {
		span := previous.spans[changed]
		if span.start < 0 || span.tailStart < span.start || span.end < span.tailStart || span.end > len(previous.lines) ||
			span.tailByte < 0 || span.tailByte > len(oldMessage.Text) || span.tailByte > len(newMessage.Text) {
			return chatLogRenderState{}, false
		}

		// Word wrapping can rewrite the previous visual row, so retain only rows
		// before the active message's final logical line. Rewrap that tail together
		// with all newly appended text; completed paragraphs never get touched.
		tailText := newMessage.Text[span.tailByte:]
		tailLines, tailLine, tailByte := c.renderAssistantTextTail(context.Background(), tailText, newMessage.Streaming, width, chatLogFenceStateBefore(newMessage.Text, span.tailByte))
		delta := len(tailLines) - (span.end - span.tailStart)

		state := chatLogRenderState{
			width:      width,
			components: updatedComponentsForMessages(previous.components, messages),
			messages:   messages,
			lines:      make([]renderLine, 0, len(previous.lines)+delta),
			spans:      append([]chatLogMessageSpan(nil), previous.spans...),
		}
		state.lines = append(state.lines, previous.lines[:span.tailStart]...)
		state.lines = append(state.lines, tailLines...)
		state.lines = append(state.lines, previous.lines[span.end:]...)
		state.spans[changed] = chatLogMessageSpan{
			start:     span.start,
			end:       span.end + delta,
			tailStart: span.tailStart + tailLine,
			tailByte:  span.tailByte + tailByte,
		}
		for i := changed + 1; i < len(state.spans); i++ {
			state.spans[i].start += delta
			state.spans[i].end += delta
			state.spans[i].tailStart += delta
		}
		return state, true
	}

	return c.tryIncrementalSingleMessageReplace(previous, messages, width, changed)
}

func (c *ChatLog) tryIncrementalSingleMessageReplace(previous *chatLogRenderState, messages []Message, width int, changed int) (chatLogRenderState, bool) {
	if changed < 0 || changed >= len(messages) || changed >= len(previous.spans) {
		return chatLogRenderState{}, false
	}
	span := previous.spans[changed]
	if span.start < 0 || span.end < span.start || span.end > len(previous.lines) {
		return chatLogRenderState{}, false
	}

	component := componentFromMessage(messages[changed], uint64(changed+1))
	messageLines, tailLine, tailByte := c.renderComponent(context.Background(), component, width)
	delta := len(messageLines) - (span.end - span.start)
	state := chatLogRenderState{
		width:      width,
		components: updatedComponentsForMessages(previous.components, messages),
		messages:   messages,
		lines:      make([]renderLine, 0, len(previous.lines)+delta),
		spans:      append([]chatLogMessageSpan(nil), previous.spans...),
	}
	state.lines = append(state.lines, previous.lines[:span.start]...)
	state.lines = append(state.lines, messageLines...)
	state.lines = append(state.lines, previous.lines[span.end:]...)
	state.spans[changed] = chatLogMessageSpan{
		start:     span.start,
		end:       span.start + len(messageLines),
		tailStart: span.start + tailLine,
		tailByte:  tailByte,
	}
	for i := changed + 1; i < len(state.spans); i++ {
		state.spans[i].start += delta
		state.spans[i].end += delta
		state.spans[i].tailStart += delta
	}
	if len(state.lines) == 0 {
		state.lines = []renderLine{{text: "", st: c.pal().spacer}}
	}
	return state, true
}

func (c *ChatLog) renderAssistantComponent(ctx context.Context, component ChatComponent, width int) ([]renderLine, int, int) {
	return c.renderAssistantText(ctx, component.Text, component.Streaming, width)
}

func (c *ChatLog) renderThinkingComponent(ctx context.Context, component ChatComponent, width int) ([]renderLine, int, int) {
	c.mu.Lock()
	hidden := c.hideThinkingBlock
	label := c.hiddenThinkingLabel
	c.mu.Unlock()
	if hidden {
		return nil, 0, len(component.Text)
	}
	if label == "" {
		label = defaultHiddenThinkingLabel
	}

	// Collapsed or empty thinking stands in as a plain label; it is chrome, not
	// model output, so it is not run through markdown.
	body := strings.TrimSpace(component.Text)
	if body == "" || (component.Collapsed && !component.Streaming) {
		lines := []renderLine{{text: " ", st: c.pal().spacer}}
		lines = c.appendWrapped(ctx, lines, label, width, c.pal().thinking)
		return lines, len(lines), len(component.Text)
	}

	lines := []renderLine{{text: " ", st: c.pal().spacer}}
	lines = c.appendMarkdown(ctx, lines, body, width, c.pal().thinking)
	return lines, len(lines), len(component.Text)
}

// SetThinkingDisplay configures how thinking components render. hide drops them
// from the log entirely; label stands in when the text is collapsed or absent
// (empty restores the default). Safe to call from any goroutine.
func (c *ChatLog) SetThinkingDisplay(hide bool, label string) {
	if c == nil {
		return
	}
	label = strings.TrimSpace(label)
	if label == "" {
		label = defaultHiddenThinkingLabel
	}
	c.mu.Lock()
	changed := c.hideThinkingBlock != hide || c.hiddenThinkingLabel != label
	c.hideThinkingBlock = hide
	c.hiddenThinkingLabel = label
	if changed {
		c.renderCache = nil
		c.renderCacheOrder = nil
	}
	c.mu.Unlock()
	if changed {
		c.scheduleCurrentSnapshot()
	}
}

// SetImageDisplay controls whether image components paint the picture or fall
// back to their text placeholder. Backs the showImages setting. Safe to call
// from any goroutine; drops the render cache because the setting changes the
// row count of every image already in the log.
func (c *ChatLog) SetImageDisplay(show bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	changed := c.hideImages == show
	c.hideImages = !show
	if changed {
		c.renderCache = nil
		c.renderCacheOrder = nil
	}
	c.mu.Unlock()
	if changed {
		c.scheduleCurrentSnapshot()
	}
}

func (c *ChatLog) renderToolExecutionComponent(ctx context.Context, component ChatComponent, width int) ([]renderLine, int, int) {
	lines := c.appendToolExecution(ctx, nil, component, width)
	return lines, len(lines), len(component.Message().Text)
}

func (c *ChatLog) renderErrorComponent(ctx context.Context, component ChatComponent, width int) ([]renderLine, int, int) {
	lines := []renderLine{{text: " ", st: c.pal().spacer}}
	lines = c.appendWrapped(ctx, lines, errorPrefix(component.Text), width, c.pal().errorRole)
	return lines, len(lines), len(component.Text)
}

func (c *ChatLog) renderSystemComponent(ctx context.Context, component ChatComponent, width int) ([]renderLine, int, int) {
	lines := []renderLine{{text: " ", st: c.pal().spacer}}
	lines = c.appendWrapped(ctx, lines, component.Text, width, c.pal().system)
	return lines, len(lines), len(component.Text)
}

const maxUserMessageRenderLines = 20

const (
	userMessageTruncatedText = "... (user message truncated)"
	bashTruncatedText        = "... (bash output truncated)"
	collapseHintText         = "... (click to collapse)"
)

func (c *ChatLog) renderUserComponent(ctx context.Context, component ChatComponent, width int) ([]renderLine, int, int) {
	body := strings.TrimRight(component.Text, "\n")
	opts := paddedBoxOptions{FooterText: collapseHintText}
	if !c.componentExpanded(component.ID) {
		opts = paddedBoxOptions{MaxContentLines: maxUserMessageRenderLines, TruncatedText: userMessageTruncatedText}
	}
	lines := c.appendPaddedMarkdownBox(ctx, nil, body, width, c.pal().user, opts)
	for _, image := range component.Images {
		imageLines, _, _ := c.renderImageComponent(ctx, ChatComponent{Image: image}, width)
		lines = append(lines, imageLines...)
	}
	return lines, len(lines), len(component.Text)
}

// toggleExpandedAt flips the render of the truncated component containing
// content row (x, y) between its capped preview and the full content. Plain
// clicks on a truncated user message or capped tool preview open it; clicking
// again collapses. Schedules a re-render only when a state actually changed.
func (c *ChatLog) toggleExpandedAt(x, y int) {
	c.mu.Lock()
	line := y - c.rect.Y
	var component ChatComponent
	if line >= 0 && line < len(c.lines) {
		for i := range c.renderedState.spans {
			if i < len(c.renderedState.components) {
				if span := c.renderedState.spans[i]; line >= span.start && line < span.end {
					component = c.renderedState.components[i]
					break
				}
			}
		}
	}
	expanded := c.expanded[component.ID]
	width := c.rect.W
	c.mu.Unlock()

	if component.ID == "" {
		return
	}
	switch component.Kind {
	case ChatComponentUser:
		// A message within the line cap has nothing hidden, so there is nothing
		// to expand; clicking it must not grow a collapse hint either.
		if !expanded && !c.userMessageExceedsRenderLimit(component, width) {
			return
		}
	case ChatComponentToolExecution:
		if !expanded && !c.toolExceedsRenderLimit(component) {
			return
		}
	default:
		return
	}

	expanded = !expanded
	c.mu.Lock()
	if expanded {
		if c.expanded == nil {
			c.expanded = make(map[string]bool)
		}
		c.expanded[component.ID] = true
	} else {
		delete(c.expanded, component.ID)
	}
	c.mu.Unlock()
	if component.Kind == ChatComponentToolExecution && component.ToolExecution.CallID != "" {
		// Plugins observe the same toggle the chat log renders.
		c.SetToolExpanded(component.ToolExecution.CallID, expanded)
	}
	c.scheduleCurrentSnapshot()
}

// componentExpanded reports whether the component's truncated preview has been
// clicked open. Safe from the render worker; guarded by mu like the map.
func (c *ChatLog) componentExpanded(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.expanded[id]
}

// toolExceedsRenderLimit reports whether the tool's built-in rendering caps
// its preview at this state, i.e. whether clicking it has anything to reveal.
// Mirrors the caps the renderers apply so the limits have one source of truth.
func (c *ChatLog) toolExceedsRenderLimit(component ChatComponent) bool {
	tool := component.ToolExecution
	switch tool.Name {
	case "bash":
		_, truncated := previewLines(tool.Output, bashToolPreviewLines)
		return truncated
	case "read":
		if !toolExecutionFailed(tool) {
			return false
		}
		_, truncated := previewLines(tool.Output, readToolPreviewLines)
		return truncated
	case "write":
		_, truncated := previewLines(writeContentFromArgs(tool.Args), writeContentPreviewLines)
		return truncated
	case "edit":
		diff := editDiffFromDetails(component.ToolDetails)
		return diff != "" && compactEditDiffTruncated(diff)
	default:
		return false
	}
}

// userMessageExceedsRenderLimit reports whether the user message's markdown
// rendering is capped by maxUserMessageRenderLines at width, i.e. whether its
// preview carries the truncation marker.
func (c *ChatLog) userMessageExceedsRenderLimit(component ChatComponent, width int) bool {
	if width <= 0 {
		return false
	}
	rows := c.newChatMarkdown(strings.TrimRight(component.Text, "\n")).Render(max(1, width-2))
	return len(rows) > maxUserMessageRenderLines
}

func (c *ChatLog) renderImageComponent(ctx context.Context, component ChatComponent, width int) ([]renderLine, int, int) {
	image := component.Image
	c.mu.Lock()
	hideImages := c.hideImages
	c.mu.Unlock()
	if !hideImages && tauui.GetTerminalCapabilities().Images == tauui.ImageProtocolKitty && strings.TrimSpace(image.Data) != "" {
		img := tauui.NewImageData(
			strings.TrimSpace(image.Data),
			strings.TrimSpace(image.MIMEType),
			tauui.ImageDimensions{WidthPx: image.Width, HeightPx: image.Height},
			tauui.ImageOptions{Filename: image.AltText, MaxWidthCells: max(1, width-1)},
		)
		rows := img.Render(width)
		if len(rows) > 0 {
			lines := make([]renderLine, 0, len(rows))
			for _, row := range rows {
				if ctx.Err() != nil {
					return lines, len(lines), len(image.Data)
				}
				if tauui.IsImageLine(row) {
					lines = append(lines, renderLine{text: row, st: c.pal().dim})
					continue
				}
				lines = append(lines, renderLine{text: " " + strings.TrimRight(row, " "), st: c.pal().dim})
			}
			return lines, len(lines), len(image.Data)
		}
	}
	text := component.Image.FallbackText()
	lines := c.appendWrapped(ctx, nil, text, width, c.pal().dim)
	return lines, len(lines), len(text)
}

func (c *ChatLog) renderCustomComponent(ctx context.Context, component ChatComponent, width int) ([]renderLine, int, int) {
	body := component.Text
	if strings.TrimSpace(body) == "" {
		body = component.Custom.FallbackText()
	}
	lines := []renderLine{{text: " ", st: c.pal().spacer}}
	lines = c.appendPaddedTextBox(ctx, lines, strings.Split(strings.TrimRight(body, "\n"), "\n"), width, c.pal().custom)
	return lines, len(lines), len(body)
}

func (c *ChatLog) renderAssistantText(ctx context.Context, body string, streaming bool, width int) ([]renderLine, int, int) {
	body = strings.TrimRight(body, "\n")
	if body == "" {
		if streaming {
			body = "▌"
		}
		lines := []renderLine{{text: " ", st: c.pal().spacer}}
		lines = c.appendMarkdown(ctx, lines, body, width, c.pal().assistant)
		return lines, 1, 0
	}

	lines := []renderLine{{text: " ", st: c.pal().spacer}}
	blocks := groupAssistantMarkdownBlocks(strings.Split(body, "\n"))
	inFence := false
	tailByte := 0
	// Every block but the last is committed; the last block is the streaming
	// tail later deltas re-render in place.
	for _, block := range blocks[:len(blocks)-1] {
		lines = c.appendAssistantMarkdownBlock(ctx, lines, block, width, c.pal().assistant, &inFence)
		tailByte += len(block) + 1 // include the newline separator
	}
	tailStart := len(lines)
	lines = c.appendAssistantMarkdownBlock(ctx, lines, blocks[len(blocks)-1], width, c.pal().assistant, &inFence)
	return lines, tailStart, tailByte
}

func (c *ChatLog) renderAssistantTextTail(ctx context.Context, body string, streaming bool, width int, initialInFence bool) ([]renderLine, int, int) {
	body = strings.TrimRight(body, "\n")
	if body == "" {
		if streaming {
			body = "▌"
		}
		lines := c.appendMarkdown(ctx, nil, body, width, c.pal().assistant)
		return lines, 0, 0
	}

	blocks := groupAssistantMarkdownBlocks(strings.Split(body, "\n"))
	var lines []renderLine
	inFence := initialInFence
	tailByte := 0
	for _, block := range blocks[:len(blocks)-1] {
		lines = c.appendAssistantMarkdownBlock(ctx, lines, block, width, c.pal().assistant, &inFence)
		tailByte += len(block) + 1
	}
	tailStart := len(lines)
	lines = c.appendAssistantMarkdownBlock(ctx, lines, blocks[len(blocks)-1], width, c.pal().assistant, &inFence)
	return lines, tailStart, tailByte
}

func chatLogFenceStateBefore(body string, byteOffset int) bool {
	if byteOffset <= 0 || body == "" {
		return false
	}
	if byteOffset > len(body) {
		byteOffset = len(body)
	}
	inFence := false
	prefix := body[:byteOffset]
	for line := range strings.SplitSeq(prefix, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
		}
	}
	return inFence
}

func (c *ChatLog) appendToolExecution(ctx context.Context, out []renderLine, component ChatComponent, width int) []renderLine {
	tool := component.ToolExecution
	boxStyle := c.toolStyle(tool)

	// A plugin that overrode only the call or only the result inherits the rest
	// from the built-in rendering, which is the generic tool box. Full shell
	// replacements never reach here — applyPluginComponent installs those.
	if overrides := c.toolRenderOverrides(tool, width); overrides.partial() {
		return c.appendGenericToolExecution(ctx, out, component, width, boxStyle, overrides)
	}

	if tool.Name == "bash" {
		return c.appendBashToolExecution(ctx, out, component, width)
	}
	if tool.Name == "read" {
		return c.appendReadToolExecution(ctx, out, component, width)
	}
	if tool.Name == "edit" {
		return c.appendEditToolExecution(ctx, out, component, width)
	}
	if tool.Name == "write" {
		return c.appendWriteToolExecution(ctx, out, component, width)
	}

	return c.appendGenericToolExecution(ctx, out, component, width, boxStyle, toolRenderOverrides{})
}

// toolRenderOverrides asks the plugin registry what it contributes for this
// execution. Returns the zero value when no registry is installed.
func (c *ChatLog) toolRenderOverrides(tool ToolExecutionComponent, width int) toolRenderOverrides {
	c.mu.Lock()
	r := c.pluginRenderers
	c.mu.Unlock()
	return r.toolOverrides(tool, width)
}

// appendGenericToolExecution renders the default tool shell: a spacer followed
// by a padded state-background block holding the call line and the output.
// Either half may be supplied by a plugin hook; whatever it does not supply is
// rendered built-in.
func (c *ChatLog) appendGenericToolExecution(ctx context.Context, out []renderLine, component ChatComponent, width int, boxStyle tauui.Style, overrides toolRenderOverrides) []renderLine {
	tool := component.ToolExecution

	var body []string
	if overrides.hasCall {
		body = append(body, overrides.call...)
	} else {
		status := renderToolStatusLine(tool, component.Text)
		if strings.TrimSpace(status) != "" {
			body = append(body, status)
		}
		call := renderToolCallComponent(tool, component.Text)
		if strings.TrimSpace(call) != "" && call != strings.TrimSpace(status) {
			body = append(body, call)
		}
	}

	if overrides.hasResult {
		body = append(body, overrides.result...)
	} else if tool.Output != "" {
		body = append(body, toolOutputLines(tool.Output)...)
	}

	if len(body) == 0 {
		body = append(body, "(no output yet)")
	}

	out = append(out, renderLine{text: " ", st: c.pal().spacer})
	return c.appendPaddedTextBox(ctx, out, body, width, boxStyle)
}

const (
	bashToolPreviewLines = 20
	readToolPreviewLines = 20
)

func (c *ChatLog) appendBashToolExecution(ctx context.Context, out []renderLine, component ChatComponent, width int) []renderLine {
	c.mu.Lock()
	look := c.look
	c.mu.Unlock()
	if look == chatLogLookTau {
		return c.appendBashToolExecutionChrome(ctx, out, component, width)
	}
	return c.appendBashToolExecutionPlain(ctx, out, component, width)
}

func (c *ChatLog) appendBashToolExecutionPlain(ctx context.Context, out []renderLine, component ChatComponent, width int) []renderLine {
	if width <= 0 {
		return out
	}
	tool := component.ToolExecution
	st := c.toolStyle(tool)
	commandStyle := st.WithFG(c.pal().bashMode).WithAttr(tauui.AttrBold)

	out = append(out, renderLine{text: " ", st: c.pal().spacer})
	cmd := bashCommandFromArgs(tool.Args)
	if cmd == "" {
		cmd = strings.TrimSpace(component.Text)
	}
	if cmd != "" {
		prefix := "$ " + expandChatLogTabs(cmd)
		for i, row := range tauui.WrapLines(prefix, tauui.WrapOptions{Width: max(1, width-1), Mode: tauui.WrapWord}) {
			if ctx.Err() != nil {
				return out
			}
			lineStyle := st
			if i == 0 {
				lineStyle = commandStyle
			}
			out = append(out, renderLine{text: " " + wrappedSegment(prefix, row), st: lineStyle})
		}
	}
	if status := bashToolStatusSuffix(tool); status != "" {
		out = append(out, renderLine{text: " " + status, st: c.pal().dim})
	}

	limit := bashToolPreviewLines
	if c.componentExpanded(component.ID) {
		limit = 0
	}
	outputLines, truncated := previewLines(tool.Output, limit)
	if len(outputLines) > 0 {
		for _, line := range outputLines {
			if ctx.Err() != nil {
				return out
			}
			cappedLine := tauui.TextTruncate(expandChatLogTabs(line), max(1, width-1), false)
			out = append(out, renderLine{text: " " + cappedLine, st: st})
		}
		if truncated {
			out = append(out, renderLine{text: " " + bashTruncatedText, st: c.pal().dim})
		} else if limit == 0 {
			out = append(out, renderLine{text: " " + collapseHintText, st: c.pal().dim})
		}
	} else if tool.State == toolExecutionComplete {
		out = append(out, renderLine{text: " (no output)", st: c.pal().dim})
	} else if tool.State == toolExecutionExecuting {
		out = append(out, renderLine{text: " running...", st: c.pal().dim})
	}
	return out
}

func (c *ChatLog) appendBashToolExecutionChrome(ctx context.Context, out []renderLine, component ChatComponent, width int) []renderLine {
	if width <= 0 {
		return out
	}
	tool := component.ToolExecution
	st := c.toolStyle(tool)
	commandStyle := st.WithFG(c.pal().bashMode).WithAttr(tauui.AttrBold)
	dimStyle := c.pal().dim.WithBG(st.BG)
	border := tauui.DynamicBorder{
		Title: " bash · " + bashToolStateLabel(tool) + " ",
		Width: width,
	}
	// The border carries execution state; the command line inside carries the
	// bash tint. Keeping those on separate colours means the box says "this
	// failed" without also recolouring the command you ran.
	borderStyle := c.pal().borderStyle(toolBorderState(tool), st.BG)

	out = append(out, renderLine{text: " ", st: c.pal().spacer})
	out = append(out, renderLine{text: border.Top(), st: borderStyle})
	cmd := bashCommandFromArgs(tool.Args)
	if cmd == "" {
		cmd = strings.TrimSpace(component.Text)
	}
	if cmd != "" {
		command := "$ " + expandChatLogTabs(cmd)
		for _, row := range tauui.WrapLines(command, tauui.WrapOptions{Width: border.InnerWidth(), Mode: tauui.WrapWord}) {
			if ctx.Err() != nil {
				return out
			}
			out = append(out, renderLine{text: border.Line(wrappedSegment(command, row)), st: commandStyle})
		}
	}
	limit := bashToolPreviewLines
	if c.componentExpanded(component.ID) {
		limit = 0
	}
	outputLines, truncated := previewLines(tool.Output, limit)
	if len(outputLines) > 0 {
		out = append(out, renderLine{text: border.Rule(), st: borderStyle})
		for _, rawLine := range outputLines {
			if ctx.Err() != nil {
				return out
			}
			line := expandChatLogTabs(rawLine)
			wrapped := tauui.WrapLines(line, tauui.WrapOptions{Width: border.InnerWidth(), Mode: tauui.WrapWord})
			if len(wrapped) == 0 {
				out = append(out, renderLine{text: border.Line(""), st: st})
				continue
			}
			for _, row := range wrapped {
				out = append(out, renderLine{text: border.Line(wrappedSegment(line, row)), st: st})
			}
		}
		if truncated {
			out = append(out, renderLine{text: border.Line(bashTruncatedText), st: dimStyle})
		} else if limit == 0 {
			out = append(out, renderLine{text: border.Line(collapseHintText), st: dimStyle})
		}
	} else if tool.State == toolExecutionComplete {
		out = append(out, renderLine{text: border.Rule(), st: borderStyle})
		out = append(out, renderLine{text: border.Line("(no output)"), st: dimStyle})
	} else if tool.State == toolExecutionExecuting {
		out = append(out, renderLine{text: border.Line("running..."), st: dimStyle})
	}
	out = append(out, renderLine{text: border.Bottom(), st: borderStyle})
	return out
}

func (c *ChatLog) appendReadToolExecution(ctx context.Context, out []renderLine, component ChatComponent, width int) []renderLine {
	if width <= 0 {
		return out
	}
	tool := component.ToolExecution
	st := c.toolStyle(tool)
	out = append(out, renderLine{text: " ", st: c.pal().spacer})
	call := expandChatLogTabs(renderReadToolCall(tool, component.Text))
	for i, row := range tauui.WrapLines(call, tauui.WrapOptions{Width: max(1, width-1), Mode: tauui.WrapWord}) {
		if ctx.Err() != nil {
			return out
		}
		lineStyle := st
		if i == 0 {
			lineStyle = st.WithAttr(tauui.AttrBold)
		}
		out = append(out, renderLine{text: " " + wrappedSegment(call, row), st: lineStyle})
	}
	// A successful read is fully described by its call line: echoing the file
	// back would bury the transcript in content the user already has on disk.
	// A failure is not — the reason lives in the output, so surface it under a
	// bounded preview.
	if !toolExecutionFailed(tool) {
		return out
	}
	limit := readToolPreviewLines
	if c.componentExpanded(component.ID) {
		limit = 0
	}
	lines, truncated := previewLines(tool.Output, limit)
	for _, line := range lines {
		if ctx.Err() != nil {
			return out
		}
		out = append(out, renderLine{text: chatLogRenderRow(" "+line, width), st: st})
	}
	if truncated {
		out = append(out, renderLine{text: chatLogRenderRow(" ... (read output truncated)", width), st: c.pal().dim})
	} else if limit == 0 && len(lines) > 0 {
		out = append(out, renderLine{text: chatLogRenderRow(" "+collapseHintText, width), st: c.pal().dim})
	}
	return out
}

// toolExecutionFailed reports whether a tool ended without a usable result.
func toolExecutionFailed(tool ToolExecutionComponent) bool {
	return tool.IsError || tool.State == toolExecutionError || tool.State == toolExecutionCancelled
}

// previewLines splits output and caps it at limit rows, reporting whether
// anything was dropped. It is the one place bounded previews are computed, so
// every built-in renderer truncates the same way.
func previewLines(output string, limit int) (lines []string, truncated bool) {
	output = strings.TrimRight(output, "\n")
	if output == "" {
		return nil, false
	}
	lines = strings.Split(output, "\n")
	if limit <= 0 || len(lines) <= limit {
		return lines, false
	}
	return lines[:limit], true
}

func (c *ChatLog) appendEditToolExecution(ctx context.Context, out []renderLine, component ChatComponent, width int) []renderLine {
	if width <= 0 {
		return out
	}
	tool := component.ToolExecution
	st := c.toolStyle(tool)
	out = append(out, renderLine{text: " ", st: c.pal().spacer})
	out = append(out, renderLine{text: chatLogRenderRow(" "+renderEditToolCall(tool, component.Text), width), st: st.WithAttr(tauui.AttrBold)})
	if diff := editDiffFromDetails(component.ToolDetails); diff != "" {
		out = append(out, renderLine{text: " ", st: st})
		expanded := c.componentExpanded(component.ID)
		diffLines := strings.Split(strings.TrimRight(diff, "\n"), "\n")
		if !expanded {
			diffLines = compactEditDiffLines(diff)
		}
		for _, line := range diffLines {
			if ctx.Err() != nil {
				return out
			}
			out = append(out, renderLine{text: chatLogRenderRow(" "+line, width), st: c.editDiffLineStyle(line, st)})
		}
		if expanded && compactEditDiffTruncated(diff) {
			out = append(out, renderLine{text: chatLogRenderRow(" "+collapseHintText, width), st: c.pal().dim})
		}
		return out
	}
	if strings.TrimSpace(tool.Output) != "" {
		for _, line := range strings.Split(strings.TrimRight(tool.Output, "\n"), "\n") {
			if ctx.Err() != nil {
				return out
			}
			out = append(out, renderLine{text: chatLogRenderRow(" "+line, width), st: st})
		}
	}
	return out
}

func (c *ChatLog) appendWriteToolExecution(ctx context.Context, out []renderLine, component ChatComponent, width int) []renderLine {
	if width <= 0 {
		return out
	}
	tool := component.ToolExecution
	st := c.toolStyle(tool)
	out = append(out, renderLine{text: " ", st: c.pal().spacer})
	out = append(out, renderLine{text: chatLogRenderRow(" "+renderWriteToolCall(tool, component.Text), width), st: st.WithAttr(tauui.AttrBold)})
	limit := writeContentPreviewLines
	if c.componentExpanded(component.ID) {
		limit = 0
	}
	if preview := writeContentPreviewFromArgs(tool.Args, limit); len(preview) > 0 {
		out = append(out, renderLine{text: " ", st: st})
		for _, line := range preview {
			if ctx.Err() != nil {
				return out
			}
			out = append(out, renderLine{text: chatLogRenderRow(" "+line, width), st: st})
		}
		if limit == 0 {
			out = append(out, renderLine{text: chatLogRenderRow(" "+collapseHintText, width), st: c.pal().dim})
		}
		return out
	}
	if strings.TrimSpace(tool.Output) != "" {
		for _, line := range strings.Split(strings.TrimRight(tool.Output, "\n"), "\n") {
			if ctx.Err() != nil {
				return out
			}
			out = append(out, renderLine{text: chatLogRenderRow(" "+line, width), st: st})
		}
	}
	return out
}

func renderEditToolCall(tool ToolExecutionComponent, fallback string) string {
	path := editPathFromArgs(tool.Args)
	if path == "" {
		call := renderToolCallComponent(tool, fallback)
		if strings.TrimSpace(call) != "" {
			return call
		}
		return "edit"
	}
	return "edit " + path
}

func editPathFromArgs(args string) string {
	if args == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		return ""
	}
	path, _ := m["path"].(string)
	return strings.TrimSpace(path)
}

const writeContentPreviewLines = 10

func renderWriteToolCall(tool ToolExecutionComponent, fallback string) string {
	path := editPathFromArgs(tool.Args)
	if path == "" {
		call := renderToolCallComponent(tool, fallback)
		if strings.TrimSpace(call) != "" {
			return call
		}
		return "write"
	}
	return "write " + path
}

// writeContentPreviewFromArgs renders the write call's content preview with at
// most limit content rows; limit <= 0 renders all of them. When rows were
// dropped, a "... (N more lines)" marker closes the preview.
func writeContentPreviewFromArgs(args string, limit int) []string {
	if args == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		return nil
	}
	content, _ := m["content"].(string)
	if content == "" {
		return nil
	}
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	total := len(lines)
	if limit > 0 && total > limit {
		lines = lines[:limit]
		out := make([]string, 0, len(lines)+2)
		out = append(out, "new file preview:")
		out = append(out, lines...)
		out = append(out, fmt.Sprintf("... (%d more lines)", total-limit))
		return out
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, "new file preview:")
	out = append(out, lines...)
	return out
}

// writeContentFromArgs returns the raw content a write call would preview.
func writeContentFromArgs(args string) string {
	if args == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		return ""
	}
	content, _ := m["content"].(string)
	return content
}

func editDiffFromDetails(details any) string {
	m, ok := details.(map[string]any)
	if !ok {
		return ""
	}
	diff, _ := m["diff"].(string)
	return strings.TrimRight(diff, "\n")
}

const editDiffContextLines = 4

func compactEditDiffLines(diff string) []string {
	lines := strings.Split(strings.TrimRight(diff, "\n"), "\n")
	changed := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
			changed = i
			break
		}
	}
	if changed < 0 {
		return lines
	}
	start := max(0, changed-editDiffContextLines)
	end := min(len(lines), changed+editDiffContextLines+1)
	out := make([]string, 0, end-start+2)
	if start > 0 {
		out = append(out, "     ...")
	}
	out = append(out, lines[start:end]...)
	if end < len(lines) {
		out = append(out, "     ...")
	}
	return out
}

// compactEditDiffTruncated reports whether compactEditDiffLines drops rows
// from diff, i.e. whether the compact preview hides anything.
func compactEditDiffTruncated(diff string) bool {
	lines := strings.Split(strings.TrimRight(diff, "\n"), "\n")
	changed := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
			changed = i
			break
		}
	}
	if changed < 0 {
		return false
	}
	return changed-editDiffContextLines > 0 || changed+editDiffContextLines+1 < len(lines)
}

func (c *ChatLog) editDiffLineStyle(line string, base tauui.Style) tauui.Style {
	if strings.HasPrefix(line, "+") {
		return base.WithFG(c.pal().diffAdded)
	}
	if strings.HasPrefix(line, "-") {
		return base.WithFG(c.pal().diffRemoved)
	}
	// Context lines get their own token so added/removed stand out against
	// them rather than against whatever the surrounding block happens to be.
	return base.WithFG(c.pal().diffContext)
}

// toolStyle resolves the state-background a tool execution renders on, from the
// per-state tool tokens. Cancelled shares the error background: both are
// "this did not produce a result", and the state label distinguishes them.
func (c *ChatLog) toolStyle(tool ToolExecutionComponent) tauui.Style {
	p := c.pal()
	if tool.IsError || tool.State == toolExecutionError || tool.State == toolExecutionCancelled {
		return p.toolError
	}
	if tool.State == toolExecutionExecuting {
		return p.toolCall
	}
	return p.toolResult
}

func bashToolStatusSuffix(tool ToolExecutionComponent) string {
	switch tool.State {
	case toolExecutionExecuting:
		return "(running)"
	case toolExecutionError:
		return "(failed)"
	case toolExecutionCancelled:
		return "(cancelled)"
	default:
		return ""
	}
}

// bashToolStateLabel names the execution state in the dynamic border's title.
func bashToolStateLabel(tool ToolExecutionComponent) string {
	switch {
	case tool.State == toolExecutionExecuting:
		return "running"
	case tool.State == toolExecutionCancelled:
		return "cancelled"
	case tool.State == toolExecutionError || tool.IsError:
		return "error"
	default:
		return "complete"
	}
}

func renderReadToolCall(tool ToolExecutionComponent, fallback string) string {
	path, suffix := readPathAndRange(tool.Args)
	if path == "" {
		call := renderToolCallComponent(tool, fallback)
		if strings.TrimSpace(call) != "" {
			return call
		}
		return "read"
	}
	return "read " + path + suffix
}

func readPathAndRange(args string) (string, string) {
	if args == "" {
		return "", ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		return "", ""
	}
	path, _ := m["path"].(string)
	path = strings.TrimSpace(path)
	if path == "" {
		return "", ""
	}
	start := jsonIntValue(m["start_line"])
	if start <= 0 {
		start = jsonIntValue(m["offset"])
	}
	end := jsonIntValue(m["end_line"])
	if limit := jsonIntValue(m["limit"]); limit > 0 {
		if start <= 0 {
			start = 1
		}
		end = start + limit - 1
	}
	if start > 0 && end > 0 {
		return path, fmt.Sprintf(":%d-%d", start, end)
	}
	if start > 0 {
		return path, fmt.Sprintf(":%d", start)
	}
	return path, ""
}

func jsonIntValue(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0
		}
		return int(i)
	default:
		return 0
	}
}

// toolBorderState maps a tool execution onto the shared border's state so the
// border colour is resolved the same way as every other bordered surface.
func toolBorderState(tool ToolExecutionComponent) tauui.DynamicBorderState {
	switch {
	case tool.State == toolExecutionExecuting:
		return tauui.DynamicBorderRunning
	case tool.State == toolExecutionCancelled:
		return tauui.DynamicBorderCancelled
	case tool.State == toolExecutionError || tool.IsError:
		return tauui.DynamicBorderError
	default:
		return tauui.DynamicBorderSuccess
	}
}

func renderToolStatusLine(tool ToolExecutionComponent, fallback string) string {
	name := strings.TrimSpace(tool.Name)
	args := strings.TrimSpace(tool.Args)
	if name == "" {
		name, args, _ = strings.Cut(strings.TrimSpace(fallback), " ")
	}
	if name == "" {
		name = "tool"
	}
	prefix := "✓ Completed"
	switch tool.State {
	case toolExecutionExecuting:
		prefix = "● Running"
	case toolExecutionError:
		prefix = "✗ Failed"
	case toolExecutionCancelled:
		prefix = "⊘ Cancelled"
	}
	if name == "bash" {
		if cmd := bashCommandFromArgs(args); cmd != "" {
			return prefix + " bash: " + cmd
		}
	}
	return prefix + " " + name
}

func renderToolCallComponent(tool ToolExecutionComponent, fallback string) string {
	name := tool.Name
	args := strings.TrimSpace(tool.Args)
	if name == "" {
		name, args, _ = strings.Cut(strings.TrimSpace(fallback), " ")
	}
	return renderToolCall(Message{ToolName: name, ToolArgs: args, Text: fallback})
}

// appendAssistantMarkdownBlock renders one grouped assistant block. A
// single-line block keeps the historic per-line path (including fence
// toggling); a multi-line block is a grouped markdown construct — currently a
// table — that must be parsed as one unit to render at all.
func (c *ChatLog) appendAssistantMarkdownBlock(ctx context.Context, out []renderLine, body string, width int, st tauui.Style, inFence *bool) []renderLine {
	if strings.Contains(body, "\n") {
		return c.appendMarkdown(ctx, out, body, width, st)
	}
	return c.appendAssistantMarkdownLine(ctx, out, body, width, st, inFence)
}

func (c *ChatLog) appendAssistantMarkdownLine(ctx context.Context, out []renderLine, body string, width int, st tauui.Style, inFence *bool) []renderLine {
	if strings.HasPrefix(strings.TrimSpace(body), "```") {
		out = c.appendWrapped(ctx, out, body, width, st)
		if inFence != nil {
			*inFence = !*inFence
		}
		return out
	}
	if inFence != nil && *inFence {
		return c.appendWrapped(ctx, out, body, width, st)
	}
	return c.appendMarkdown(ctx, out, body, width, st)
}

// groupAssistantMarkdownBlocks partitions assistant logical lines into render
// blocks. Ordinary lines stay individual so streaming keeps re-rendering only
// the final line; GitHub-style markdown tables are grouped with their header
// and delimiter rows because goldmark only recognizes a table when it sees the
// whole thing. Fence lines and lines inside a fence never join a table group,
// mirroring how fences interrupt a table paragraph.
func groupAssistantMarkdownBlocks(lines []string) []string {
	var blocks []string
	inFence := false
	for i := 0; i < len(lines); {
		if !inFence && assistantTableStart(lines, i) {
			end := assistantTableEnd(lines, i)
			blocks = append(blocks, strings.Join(lines[i:end], "\n"))
			i = end
			continue
		}
		blocks = append(blocks, lines[i])
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
			inFence = !inFence
		}
		i++
	}
	return blocks
}

// assistantTableStart reports whether lines[i] begins a markdown table: a
// header row containing a pipe followed by a delimiter row.
func assistantTableStart(lines []string, i int) bool {
	if i+1 >= len(lines) {
		return false
	}
	header := strings.TrimSpace(lines[i])
	if header == "" || !strings.Contains(header, "|") || strings.HasPrefix(header, "```") {
		return false
	}
	return isAssistantTableDelimiter(lines[i+1])
}

// isAssistantTableDelimiter reports whether line is a GitHub-style table
// delimiter row such as |---|, |:--|--:|, or ---|---. Requiring a pipe keeps
// setext underlines and thematic breaks out of the table path.
func isAssistantTableDelimiter(line string) bool {
	if !strings.Contains(line, "|") {
		return false
	}
	for _, b := range []byte(line) {
		switch b {
		case ' ', '\t', '-', ':', '|':
		default:
			return false
		}
	}
	cells := strings.Split(line, "|")
	if len(cells) > 0 && strings.TrimSpace(cells[0]) == "" {
		cells = cells[1:]
	}
	if len(cells) > 0 && strings.TrimSpace(cells[len(cells)-1]) == "" {
		cells = cells[:len(cells)-1]
	}
	if len(cells) == 0 {
		return false
	}
	for _, cell := range cells {
		if !assistantTableDelimCell(cell) {
			return false
		}
	}
	return true
}

// assistantTableDelimCell reports whether cell is a single alignment cell:
// dashes optionally wrapped in colons (---, :--, --:, :--:).
func assistantTableDelimCell(cell string) bool {
	cell = strings.TrimSpace(cell)
	if cell == "" {
		return false
	}
	if cell[0] == ':' {
		cell = cell[1:]
	}
	if cell != "" && cell[len(cell)-1] == ':' {
		cell = cell[:len(cell)-1]
	}
	return strings.Trim(cell, "-") == "" && strings.Contains(cell, "-")
}

// assistantTableEnd returns the exclusive index past the last line of the
// table starting at lines[start]. Rows continue until a blank line, a line
// without a pipe, or a fence delimiter ends the table.
func assistantTableEnd(lines []string, start int) int {
	end := start + 2 // header + delimiter
	for end < len(lines) {
		line := strings.TrimSpace(lines[end])
		if line == "" || !strings.Contains(line, "|") || strings.HasPrefix(line, "```") {
			break
		}
		end++
	}
	return end
}

// newChatMarkdown builds the markdown renderer for chat content. The tau
// look encloses tables in rounded chrome so they read as bordered surfaces
// like the tool chrome; other looks keep the plain grid.
func (c *ChatLog) newChatMarkdown(body string) *tauui.Markdown {
	md := tauui.NewMarkdown(body)
	c.mu.Lock()
	tauLook := c.look == chatLogLookTau
	c.mu.Unlock()
	if tauLook {
		md.SetTableChrome(true)
	}
	return md
}

func (c *ChatLog) appendMarkdown(ctx context.Context, out []renderLine, body string, width int, st tauui.Style) []renderLine {
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return append(out, renderLine{text: " ", st: st})
	}
	textWidth := width - 1
	if textWidth < 1 {
		textWidth = 1
	}
	rows := c.newChatMarkdown(body).Render(textWidth)
	if len(rows) == 0 {
		return append(out, renderLine{text: " ", st: st})
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return out
		}
		out = append(out, renderLine{text: " " + strings.TrimRight(row, " "), st: st})
	}
	return out
}

type paddedBoxOptions struct {
	MaxContentLines int
	TruncatedText   string
	FooterText      string
}

func (c *ChatLog) appendPaddedMarkdownBox(ctx context.Context, out []renderLine, body string, width int, st tauui.Style, opts ...paddedBoxOptions) []renderLine {
	if width <= 0 {
		return out
	}
	out = append(out, renderLine{text: strings.Repeat(" ", width), st: st})
	innerWidth := max(1, width-2)
	rows := c.newChatMarkdown(body).Render(innerWidth)
	if len(rows) == 0 {
		rows = []string{""}
	}
	var opt paddedBoxOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	if opt.MaxContentLines > 0 && len(rows) > opt.MaxContentLines {
		rows = append(append([]string(nil), rows[:opt.MaxContentLines]...), opt.TruncatedText)
	}
	if opt.FooterText != "" {
		rows = append(rows, opt.FooterText)
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return out
		}
		row = tauui.StripANSI(row)
		text := " " + strings.TrimRight(row, " ")
		if tauui.TextWidth(text) < width {
			text += strings.Repeat(" ", width-tauui.TextWidth(text))
		}
		out = append(out, renderLine{text: text, st: st})
	}
	out = append(out, renderLine{text: strings.Repeat(" ", width), st: st})
	return out
}

func (c *ChatLog) appendPaddedTextBox(ctx context.Context, out []renderLine, body []string, width int, st tauui.Style) []renderLine {
	if width <= 0 {
		return out
	}
	out = append(out, renderLine{text: strings.Repeat(" ", width), st: st})
	innerWidth := max(1, width-2)
	for _, line := range body {
		if ctx.Err() != nil {
			return out
		}
		rows := tauui.Wrap(line, innerWidth)
		if len(rows) == 0 {
			rows = []string{""}
		}
		for _, row := range rows {
			text := " " + strings.TrimRight(row, " ")
			if tauui.TextWidth(text) < width {
				text += strings.Repeat(" ", width-tauui.TextWidth(text))
			}
			out = append(out, renderLine{text: text, st: st})
		}
	}
	out = append(out, renderLine{text: strings.Repeat(" ", width), st: st})
	return out
}

func (c *ChatLog) appendWrapped(ctx context.Context, out []renderLine, body string, width int, st tauui.Style) []renderLine {
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return append(out, renderLine{text: " ", st: st})
	}
	textWidth := width - 1 // one-cell left padding, matching Pi's chat rows
	if textWidth < 1 {
		textWidth = 1
	}
	for logicalLine := range strings.SplitSeq(body, "\n") {
		if ctx.Err() != nil {
			return out
		}
		logicalLine = expandChatLogTabs(logicalLine)
		if logicalLine == "" {
			out = append(out, renderLine{text: " ", st: st})
			continue
		}
		for _, wl := range tauui.WrapLines(logicalLine, tauui.WrapOptions{Width: textWidth, Mode: tauui.WrapWord}) {
			if ctx.Err() != nil {
				return out
			}
			out = append(out, renderLine{text: " " + wrappedSegment(logicalLine, wl), st: st})
		}
	}
	return out
}

func renderToolCall(msg Message) string {
	name := msg.ToolName
	args := strings.TrimSpace(msg.ToolArgs)
	if name == "" {
		name, args, _ = strings.Cut(strings.TrimSpace(msg.Text), " ")
	}
	if name == "" {
		return "tool call"
	}
	if name == "bash" {
		if cmd := bashCommandFromArgs(args); cmd != "" {
			return "$ " + cmd
		}
	}
	if args == "" {
		return "tool " + name
	}
	return "tool " + name + " " + args
}

func bashCommandFromArgs(args string) string {
	if args == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		return ""
	}
	cmd, _ := m["command"].(string)
	return strings.TrimSpace(cmd)
}

// toolOutputLines splits a tool's output for the generic fallback shell. It
// deliberately does not truncate: bounded preview/expand is a property of the
// specific built-in renderers (bash, read, write), not of every tool the model
// happens to call. A generic tool that returns 500 lines returns 500 lines.
func toolOutputLines(output string) []string {
	output = strings.TrimRight(output, "\n")
	if output == "" {
		return nil
	}
	return strings.Split(output, "\n")
}

func errorPrefix(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "error"
	}
	if strings.HasPrefix(strings.ToLower(s), "error") {
		return s
	}
	return "error: " + s
}

// wrappedSegment returns the substring [StartByte, EndByte) of s, used to
// extract a wrapped line from its byte range.
func wrappedSegment(s string, wl tauui.WrappedLine) string {
	if wl.StartByte < 0 || wl.EndByte > len(s) || wl.StartByte > wl.EndByte {
		return ""
	}
	return s[wl.StartByte:wl.EndByte]
}

// LineCount returns only the latest completed wrapped line count. It never
// wraps or waits for a pending request.
func (c *ChatLog) LineCount() int {
	c.mu.Lock()
	n := c.lineCount
	c.mu.Unlock()
	return n
}
