package extensions

import (
	"fmt"
	"maps"
	"slices"
	"sort"

	"github.com/losinggeneration/rovel/ui"

	"github.com/losinggeneration/tau/tui/keybindings"
)

type Placement string

const (
	Header  Placement = "header"
	Footer  Placement = "footer"
	Sidebar Placement = "sidebar"
)

type (
	Renderer func(payload any) []string
	// ToolRendererResolver selects or wraps a renderer, including for unregistered tools.
	ToolRendererResolver func(name string, next func() Renderer) Renderer
	Shortcut             struct {
		ID, Key, Description string
		Handler              func()
	}
)

type Widget struct {
	ID        string
	Placement Placement
	Render    func(width int) []string
}
type Theme struct {
	Name   string
	Values map[string]string
	// Colors resolves terminal-default values for inspection without changing Values.
	Colors     map[string]string
	Appearance string
	Dim        []string
}

type Registry struct {
	toolResolvers []ToolRendererResolver
	message       map[string]Renderer
	tool          map[string]Renderer
	shortcuts     map[string]Shortcut
	widgets       map[string]Widget
	theme         Theme
	editorText    string
	toolExpanded  map[string]bool
	pasteProvider PasteProvider
	pasteBuffer   string
	pasteHandled  int

	// onTheme is invoked after SetTheme mutates the stored theme.
	// Plugins wire this to mirror the new theme into the live UI
	// (palette swap, footer label, etc.). Nil-safe; the Registry
	// only calls it when set.
	onTheme func(Theme)
	// onEditorText is invoked after SetEditorText/InsertEditorText
	// mutate the stored editor text. The plugin wiring layer uses
	// this to push the latest text into the live TextArea so the
	// user sees plugin-driven edits without a separate reconcile
	// pass. Nil-safe.
	onEditorText func(text string)
	// onInsertEditorText is invoked after InsertEditorText with the
	// same arguments the plugin passed. It exists so wiring can
	// distinguish a wholesale replace from a positional insert
	// when both share the onEditorText callback. Nil-safe.
	onInsertEditorText func(pos int, text string)
	// onToolExpanded is invoked after SetToolExpanded with the new
	// expansion state. Wiring uses it to invalidate the chat log
	// so the affected tool row repaints. Nil-safe.
	onToolExpanded func(id string, expanded bool)
	// onPaste is invoked after InsertPaste/ReplacePaste record the
	// payload but before the live editor sees it. Plugins can use
	// it to inspect or rewrite the paste before it lands in the
	// editor buffer. Nil-safe.
	onPaste func(kind PasteKind, payload string) (string, bool)
}

func New() *Registry {
	return &Registry{message: map[string]Renderer{}, tool: map[string]Renderer{}, shortcuts: map[string]Shortcut{}, widgets: map[string]Widget{}, toolExpanded: map[string]bool{}}
}

func (r *Registry) RegisterMessageRenderer(kind string, fn Renderer) error {
	if kind == "" || fn == nil {
		return fmt.Errorf("extensions: message renderer requires kind and function")
	}
	if _, ok := r.message[kind]; ok {
		return fmt.Errorf("extensions: message renderer %q already registered", kind)
	}
	r.message[kind] = fn
	return nil
}

func (r *Registry) RegisterToolRenderer(name string, fn Renderer) error {
	if name == "" || fn == nil {
		return fmt.Errorf("extensions: tool renderer requires name and function")
	}
	if _, ok := r.tool[name]; ok {
		return fmt.Errorf("extensions: tool renderer %q already registered", name)
	}
	r.tool[name] = fn
	return nil
}

func (r *Registry) RenderMessage(kind string, payload any) ([]string, bool) {
	fn, ok := r.message[kind]
	if !ok {
		return nil, false
	}
	return fn(payload), true
}

func (r *Registry) RenderTool(name string, payload any) ([]string, bool) {
	fn := r.ToolRendererFor(name)
	if fn == nil {
		return nil, false
	}
	return fn(payload), true
}

// MessageKinds returns the registered message-renderer kinds in
// registration order. The chat viewport bridge uses this to walk the
// registry and install MessageRenderer adapters into the chat log so
// plugin-supplied renderers actually paint in the live UI rather than
// only being queryable through RenderMessage. Order matters because
// the chat log records the registration sequence for diagnostics.
func (r *Registry) MessageKinds() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.message))
	for k := range r.message {
		out = append(out, k)
	}
	// Stable sort so callers see deterministic diagnostics even
	// though the internal map is unordered.
	sort.Strings(out)
	return out
}

// ToolNames returns the registered tool-renderer names in alphabetical
// order. The bridge uses this for the same reason MessageKinds exists.
func (r *Registry) ToolNames() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.tool))
	for k := range r.tool {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// MessageRendererFor returns the registered message renderer for kind
// (or nil). It exists so the App's bridge can introspect a registry
// and reuse the same fn stored in the registry map without the bridge
// re-querying via RenderMessage for every component render.
func (r *Registry) MessageRendererFor(kind string) Renderer {
	if r == nil {
		return nil
	}
	return r.message[kind]
}

// RegisterToolRendererResolver appends a resolver in extension registration order.
func (r *Registry) RegisterToolRendererResolver(resolve ToolRendererResolver) {
	if resolve != nil {
		r.toolResolvers = append(r.toolResolvers, resolve)
	}
}

// ToolRendererFor resolves extension renderers before the registered tool renderer.
func (r *Registry) ToolRendererFor(name string) Renderer {
	if r == nil {
		return nil
	}
	var resolve func(int) Renderer
	resolve = func(index int) Renderer {
		if index == len(r.toolResolvers) {
			return r.tool[name]
		}
		return r.toolResolvers[index](name, func() Renderer { return resolve(index + 1) })
	}
	return resolve(0)
}

func (r *Registry) RegisterShortcut(s Shortcut) error {
	if s.ID == "" || s.Key == "" || s.Handler == nil {
		return fmt.Errorf("extensions: shortcut requires id key and handler")
	}
	if err := keybindings.CanRegisterExtensionBinding(s.ID); err != nil {
		return err
	}
	if _, ok := r.shortcuts[s.ID]; ok {
		return fmt.Errorf("extensions: shortcut %q already registered", s.ID)
	}
	for _, existing := range r.shortcuts {
		if existing.Key == s.Key {
			return fmt.Errorf("extensions: shortcut key %q conflicts with %q", s.Key, existing.ID)
		}
	}
	r.shortcuts[s.ID] = s
	return nil
}

func (r *Registry) TriggerShortcut(key string) bool {
	for _, s := range r.shortcuts {
		if s.Key == key {
			s.Handler()
			return true
		}
	}
	return false
}

// ShortcutForKeystroke returns the shortcut bound to k, comparing compiled
// keystrokes rather than raw strings so "ctrl+K", "Ctrl+k" and "ctrl+k" all
// match the key the terminal actually delivers.
//
// Shortcuts are held in a map, so a registration whose key fails to compile is
// skipped rather than matched by accident, and ties are resolved by taking the
// lowest id — RegisterShortcut already rejects duplicate keys, so a tie means
// two spellings of one key and either answer is the same handler in practice.
// Sorting only keeps the choice stable across runs.
func (r *Registry) ShortcutForKeystroke(k ui.Keystroke) (Shortcut, bool) {
	if r == nil {
		return Shortcut{}, false
	}
	ids := make([]string, 0, len(r.shortcuts))
	for id := range r.shortcuts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := r.shortcuts[id]
		compiled, err := keybindings.CompileKey(s.Key)
		if err != nil {
			continue
		}
		if keybindings.KeystrokesMatch(k, compiled) {
			return s, true
		}
	}
	return Shortcut{}, false
}

func (r *Registry) RegisterWidget(w Widget) error {
	if w.ID == "" || w.Render == nil {
		return fmt.Errorf("extensions: widget requires id and render")
	}
	if _, ok := r.widgets[w.ID]; ok {
		return fmt.Errorf("extensions: widget %q already registered", w.ID)
	}
	r.widgets[w.ID] = w
	return nil
}

func (r *Registry) Widgets(p Placement, width int) [][]string {
	var out [][]string
	for _, w := range r.widgets {
		if w.Placement == p {
			out = append(out, w.Render(width))
		}
	}
	return out
}

func (r *Registry) WidgetsByID(p Placement, width int) map[string][]string {
	out := map[string][]string{}
	for id, w := range r.widgets {
		if w.Placement == p {
			out[id] = w.Render(width)
		}
	}
	return out
}

func (r *Registry) WidgetIDs(p Placement) []string {
	ids := make([]string, 0, len(r.widgets))
	for id, w := range r.widgets {
		if w.Placement == p {
			ids = append(ids, id)
		}
	}
	return ids
}

// OnThemeChanged registers a callback invoked after SetTheme mutates
// the stored theme. Pass nil to clear. The wiring layer uses this so
// a plugin's theme change is reflected in the live UI palette/footer.
func (r *Registry) OnThemeChanged(fn func(Theme)) { r.onTheme = fn }

// OnEditorTextChanged registers a callback invoked after
// SetEditorText and InsertEditorText. The wiring layer uses it to push
// plugin edits into the live TextArea.
func (r *Registry) OnEditorTextChanged(fn func(text string)) { r.onEditorText = fn }

// OnInsertEditorTextChanged registers a callback invoked after
// InsertEditorText with the original (pos, text) args. Useful when
// the wiring wants to distinguish a positional insert from a wholesale
// replace.
func (r *Registry) OnInsertEditorTextChanged(fn func(pos int, text string)) {
	r.onInsertEditorText = fn
}

// OnToolExpandedChanged registers a callback invoked after
// SetToolExpanded. The wiring layer uses it to invalidate the chat log
// when a plugin toggles a tool's expansion.
func (r *Registry) OnToolExpandedChanged(fn func(id string, expanded bool)) {
	r.onToolExpanded = fn
}

// OnPaste registers a callback invoked before the live editor sees a
// paste payload. The callback may rewrite the payload by returning a
// non-empty string; returning ("", false) leaves the payload unchanged.
// Plugins use it to inspect or rewrite pastes (e.g. expand a
// paste-as-attachment into a real attachment reference).
func (r *Registry) OnPaste(fn func(kind PasteKind, payload string) (string, bool)) {
	r.onPaste = fn
}

func (r *Registry) SetTheme(t Theme) {
	t.Values = maps.Clone(t.Values)
	t.Colors = maps.Clone(t.Colors)
	t.Dim = slices.Clone(t.Dim)
	r.theme = t
	if r.onTheme != nil {
		r.onTheme(r.Theme())
	}
}

func (r *Registry) Theme() Theme {
	t := r.theme
	t.Values = maps.Clone(t.Values)
	t.Colors = maps.Clone(t.Colors)
	t.Dim = slices.Clone(t.Dim)
	return t
}

func (r *Registry) SetEditorText(s string) {
	r.editorText = s
	if r.onEditorText != nil {
		r.onEditorText(s)
	}
}
func (r *Registry) EditorText() string { return r.editorText }
func (r *Registry) InsertEditorText(pos int, s string) {
	if pos < 0 {
		pos = 0
	}
	if pos > len(r.editorText) {
		pos = len(r.editorText)
	}
	r.editorText = r.editorText[:pos] + s + r.editorText[pos:]
	if r.onInsertEditorText != nil {
		r.onInsertEditorText(pos, s)
	}
	if r.onEditorText != nil {
		r.onEditorText(r.editorText)
	}
}

func (r *Registry) SetToolExpanded(id string, expanded bool) {
	r.toolExpanded[id] = expanded
	if r.onToolExpanded != nil {
		r.onToolExpanded(id, expanded)
	}
}
func (r *Registry) ToolExpanded(id string) bool { return r.toolExpanded[id] }

// PasteProvider is the live-editor source the paste hooks (InsertPaste
// and ReplacePaste) consult to mutate the editor. It is intentionally
// narrow: the editor's bracketed-paste code path owns the buffer
// boundaries and the line-ending normalization, so a plugin shouldn't
// be able to bypass them. The Registry accepts any provider that can
// hand back the editor's current text and insert/replace a normalized
// paste payload, then forwards the change through the registered
// implementation.
type PasteProvider interface {
	// InsertPaste returns the new text the editor should hold after
	// inserting s at the current cursor, plus the cursor position
	// the editor should adopt.
	InsertPaste(s string) (text string, cursor int)
	// ReplacePaste returns the new text and cursor the editor
	// should adopt after replacing the current selection (or
	// everything if no selection) with s.
	ReplacePaste(s string) (text string, cursor int)
}

// PasteHandler is the optional function-style alternative to
// PasteProvider. It is invoked directly by the Registry with no
// editor round-trip; callers that just want to record the paste
// payload (audit, telemetry) can use this without touching the
// editor. The Registry's InsertPaste method delegates to whichever
// is registered.
type PasteHandler func(kind string, payload string)

// PasteKind identifies a paste source for handler dispatch. The
// editor sets the kind before calling InsertPaste so a single
// registry can route bracketed-paste, image-drop, and arbitrary
// future kinds through the same code path.
type PasteKind string

const (
	PasteKindBracketed PasteKind = "bracketed"
	PasteKindImage     PasteKind = "image"
	PasteKindPlain     PasteKind = "plain"
)

// pasteProvider is the live-editor implementation. nil means the
// Registry has no editor wired; InsertPaste is a no-op.
func (r *Registry) SetPasteProvider(p PasteProvider) { r.pasteProvider = p }
func (r *Registry) PasteProvider() PasteProvider     { return r.pasteProvider }

// InsertPaste delegates to the live PasteProvider. The Registry
// records the most recent payload in pasteBuffer so plugin code can
// query the last paste without holding a separate reference. Returns
// the new editor text and cursor, or ("", 0) if no provider is set.
func (r *Registry) InsertPaste(s string) (text string, cursor int) {
	r.pasteBuffer = s
	if r.onPaste != nil {
		if rewritten, ok := r.onPaste(PasteKindPlain, s); ok {
			s = rewritten
		}
	}
	if r.pasteProvider == nil {
		return "", 0
	}
	return r.pasteProvider.InsertPaste(s)
}

// ReplacePaste delegates to the live PasteProvider, recording the
// payload before the call. Returns the new editor text and cursor,
// or ("", 0) if no provider is set.
func (r *Registry) ReplacePaste(s string) (text string, cursor int) {
	r.pasteBuffer = s
	if r.onPaste != nil {
		if rewritten, ok := r.onPaste(PasteKindPlain, s); ok {
			s = rewritten
		}
	}
	if r.pasteProvider == nil {
		return "", 0
	}
	return r.pasteProvider.ReplacePaste(s)
}

// LastPaste returns the most recent paste payload the Registry has
// seen, regardless of whether a provider was wired. Useful for
// plugin audits that need to log pastes.
func (r *Registry) LastPaste() string { return r.pasteBuffer }

// NotifyPasteHandled records that a plugin handled the paste
// without routing it through the live editor. The Registry tracks
// the count so tests can verify the plugin path was taken.
func (r *Registry) NotifyPasteHandled() { r.pasteHandled++ }

// PastesHandled returns the number of pastes a plugin reported
// handled without touching the editor.
func (r *Registry) PastesHandled() int { return r.pasteHandled }
