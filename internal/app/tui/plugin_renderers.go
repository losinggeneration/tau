package tui

import (
	"sort"
	"strings"
	"sync"

	"github.com/losinggeneration/tau/tui/extensions"
)

// CustomMessageRenderer renders one custom chat component into terminal rows.
//
// Registration is keyed by custom message type, not by role: built-in user,
// assistant, thinking, system, error and tool rendering is fixed in the core
// app and is not extension-replaceable. A plugin that wants its own surface in
// the transcript emits a custom component and renders that.
type CustomMessageRenderer interface {
	// CustomType returns the CustomComponent.SourceID this renderer claims.
	CustomType() string
	// Render returns terminal rows for one component plus a flag telling the
	// chat viewport whether the rows fully replace the default projection.
	Render(component ChatComponent, width int) (rows []string, replace bool)
}

// ToolRenderer renders a tool execution's outer shell, replacing the built-in
// rendering entirely.
//
// Deviation from the visual spec, recorded deliberately: the spec carries
// render hooks on tool *definitions*. Tau's TUI is a client of the agent across
// the SDK boundary and never sees an agent.Tool — it observes executions by
// name. Moving hooks onto definitions would require the agent layer to carry
// terminal-rendering closures, inverting the dependency. Keying by name is the
// faithful translation of the spec's intent to this architecture; the part that
// does translate — the renderCall/renderResult/renderShell split, and built-in
// inheritance for the hooks a renderer leaves out — is implemented below.
type ToolRenderer interface {
	// ToolName returns the tool id this renderer applies to.
	ToolName() string
	// RenderShell returns the complete rendering for one execution. Returning
	// replace=false defers to the built-in shell.
	RenderShell(exec ToolExecutionComponent, width int) (rows []string, replace bool)
}

// ToolCallRenderer optionally overrides just the call/title portion of a tool
// execution, inheriting the built-in rendering for everything else.
type ToolCallRenderer interface {
	ToolRenderer
	RenderCall(exec ToolExecutionComponent, width int) (rows []string, replace bool)
}

// ToolResultRenderer optionally overrides just the output portion of a tool
// execution, inheriting the built-in rendering for everything else.
type ToolResultRenderer interface {
	ToolRenderer
	RenderResult(exec ToolExecutionComponent, width int) (rows []string, replace bool)
}

// toolRenderOverrides is what a tool's registered renderer contributed for one
// execution. An empty value means "render entirely built-in".
type toolRenderOverrides struct {
	shell  []string
	call   []string
	result []string

	hasShell  bool
	hasCall   bool
	hasResult bool
}

// partial reports whether a call- or result-level override applies without a
// full shell replacement. Partial overrides render through the generic tool
// box: the specialised built-in renderers (bash's dynamic border, edit's diff)
// are single compositions, so substituting one part of them would produce
// chrome that matches neither the plugin's intent nor the built-in layout.
func (o toolRenderOverrides) partial() bool {
	return !o.hasShell && (o.hasCall || o.hasResult)
}

// pluginRenderers holds the custom-type-keyed message renderers and the
// tool-name-keyed tool renderers. It is mutable from any goroutine (plugins
// register from background goroutines) so all access goes through a mutex.
type pluginRenderers struct {
	mu               sync.RWMutex
	messageByType    map[string]CustomMessageRenderer
	toolByName       map[string]ToolRenderer
	messageTypeOrder []string
	toolOrder        []string
}

func newPluginRenderers() *pluginRenderers {
	return &pluginRenderers{
		messageByType: map[string]CustomMessageRenderer{},
		toolByName:    map[string]ToolRenderer{},
	}
}

// RegisterMessage adds (or replaces) the renderer for a custom message type.
// The most recent registration wins.
func (r *pluginRenderers) RegisterMessage(renderer CustomMessageRenderer) {
	if renderer == nil {
		return
	}
	customType := renderer.CustomType()
	if customType == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.messageByType[customType]; !ok {
		r.messageTypeOrder = append(r.messageTypeOrder, customType)
	}
	r.messageByType[customType] = renderer
}

// RegisterTool adds (or replaces) the renderer for a tool name.
func (r *pluginRenderers) RegisterTool(renderer ToolRenderer) {
	if renderer == nil {
		return
	}
	name := renderer.ToolName()
	if name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.toolByName[name]; !ok {
		r.toolOrder = append(r.toolOrder, name)
	}
	r.toolByName[name] = renderer
}

// LookupMessage returns the renderer registered for a custom message type, or
// nil if none.
func (r *pluginRenderers) LookupMessage(customType string) CustomMessageRenderer {
	if r == nil || customType == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.messageByType[customType]
}

// LookupTool returns the renderer registered for tool, or nil if none.
func (r *pluginRenderers) LookupTool(tool string) ToolRenderer {
	if r == nil || tool == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.toolByName[tool]
}

// CustomTypes returns the registered custom message types in registration
// order; used by the help/inspector overlays so the user can see what plugins
// have augmented the chat viewport.
func (r *pluginRenderers) CustomTypes() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.messageTypeOrder...)
}

// Tools returns the registered tool names in registration order.
func (r *pluginRenderers) Tools() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.toolOrder...)
}

// renderCustomMessage returns rows for a custom component from its registered
// renderer. Built-in component kinds never reach a plugin renderer.
func (r *pluginRenderers) renderCustomMessage(component ChatComponent, width int) ([]string, bool) {
	if r == nil || component.Kind != ChatComponentCustom {
		return nil, false
	}
	r.mu.RLock()
	renderer := r.messageByType[component.Custom.SourceID]
	r.mu.RUnlock()
	if renderer == nil {
		return nil, false
	}
	return renderer.Render(component, width)
}

// toolOverrides collects whatever the registered renderer for this tool
// contributes. Hooks a renderer does not implement are left to the built-in
// rendering, which is the inheritance the spec asks for.
func (r *pluginRenderers) toolOverrides(exec ToolExecutionComponent, width int) toolRenderOverrides {
	if r == nil {
		return toolRenderOverrides{}
	}
	r.mu.RLock()
	renderer := r.toolByName[exec.Name]
	r.mu.RUnlock()
	if renderer == nil {
		return toolRenderOverrides{}
	}

	var out toolRenderOverrides
	if rows, ok := renderer.RenderShell(exec, width); ok && len(rows) > 0 {
		out.shell = rows
		out.hasShell = true
		// A full shell replacement subsumes the finer hooks; collecting them
		// too would just be work whose result is discarded.
		return out
	}
	if hook, ok := renderer.(ToolCallRenderer); ok {
		if rows, ok := hook.RenderCall(exec, width); ok && len(rows) > 0 {
			out.call = rows
			out.hasCall = true
		}
	}
	if hook, ok := renderer.(ToolResultRenderer); ok {
		if rows, ok := hook.RenderResult(exec, width); ok && len(rows) > 0 {
			out.result = rows
			out.hasResult = true
		}
	}
	return out
}

// applyPluginRenderers returns rows that fully replace the default projection
// for one component, or (nil, false) to render built-in. Only custom messages
// and full tool-shell replacements qualify; partial tool overrides are applied
// inside the tool rendering path so they can inherit the built-in shell.
func (r *pluginRenderers) applyPluginRenderers(component ChatComponent, width int) ([]string, bool) {
	if r == nil {
		return nil, false
	}
	if component.Kind == ChatComponentCustom {
		return r.renderCustomMessage(component, width)
	}
	if component.Kind == ChatComponentToolExecution {
		if ov := r.toolOverrides(component.ToolExecution, width); ov.hasShell {
			return ov.shell, true
		}
	}
	return nil, false
}

// PluginRenderersFor returns a snapshot of the registered renderers, suitable
// for help text.
func (r *pluginRenderers) PluginRenderersFor() string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	types := append([]string(nil), r.messageTypeOrder...)
	tools := append([]string(nil), r.toolOrder...)
	r.mu.RUnlock()

	sort.Strings(types)
	sort.Strings(tools)
	var b strings.Builder
	if len(types) > 0 {
		b.WriteString("Custom message renderers: ")
		b.WriteString(strings.Join(types, ", "))
		b.WriteByte('\n')
	}
	if len(tools) > 0 {
		b.WriteString("Tool renderers:  ")
		b.WriteString(strings.Join(tools, ", "))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// extensionsMessageAdapter bridges a Registry message renderer to the chat
// log's CustomMessageRenderer interface, so plugin code that calls
// extensions.Registry.RegisterMessageRenderer actually has its rows painted by
// the chat viewport rather than only being queryable through the Registry.
type extensionsMessageAdapter struct {
	customType string
	fn         extensions.Renderer
}

func (a *extensionsMessageAdapter) CustomType() string { return a.customType }
func (a *extensionsMessageAdapter) Render(component ChatComponent, _ int) ([]string, bool) {
	if a.fn == nil {
		return nil, false
	}
	rows := a.fn(component)
	if len(rows) == 0 {
		return nil, false
	}
	return rows, true
}

// extensionsToolAdapter bridges a Registry tool renderer into the chat log's
// ToolRenderer interface. The Registry's single-function renderer maps to the
// shell hook: it produces the whole rendering for an execution.
type extensionsToolAdapter struct {
	name string
	fn   extensions.Renderer
}

func (a *extensionsToolAdapter) ToolName() string { return a.name }
func (a *extensionsToolAdapter) RenderShell(exec ToolExecutionComponent, _ int) ([]string, bool) {
	if a.fn == nil {
		return nil, false
	}
	rows := a.fn(exec)
	if len(rows) == 0 {
		return nil, false
	}
	return rows, true
}

// builtInMessageKinds are the component roles the core app owns. A plugin
// registering one of these names is trying to restyle built-in chat, which the
// visual spec reserves for the app; the registration is dropped rather than
// silently taking over the transcript.
var builtInMessageKinds = map[string]struct{}{
	"user": {}, "assistant": {}, "system": {}, "error": {},
	"thinking": {}, "tool": {}, "tool_call": {}, "tool_result": {},
}

// ApplyExtensionsRegistry walks the given extensions.Registry and installs
// adapter entries into the plugin renderer registry. The chat viewport's
// applyPluginComponent path then consults these adapters before falling back to
// the built-in projection.
//
// Existing entries with the same custom type or tool name are replaced, so
// re-applying a registry after a plugin reload picks up the new renderers
// without leaking the old ones.
//
// Returns the number of adapters installed (handy for tests).
func (r *pluginRenderers) ApplyExtensionsRegistry(reg *extensions.Registry) int {
	if r == nil || reg == nil {
		return 0
	}
	installed := 0
	for _, kind := range reg.MessageKinds() {
		if _, builtIn := builtInMessageKinds[strings.ToLower(strings.TrimSpace(kind))]; builtIn {
			continue
		}
		fn := reg.MessageRendererFor(kind)
		if fn == nil {
			continue
		}
		r.RegisterMessage(&extensionsMessageAdapter{customType: kind, fn: fn})
		installed++
	}
	for _, name := range reg.ToolNames() {
		fn := reg.ToolRendererFor(name)
		if fn == nil {
			continue
		}
		r.RegisterTool(&extensionsToolAdapter{name: name, fn: fn})
		installed++
	}
	return installed
}
