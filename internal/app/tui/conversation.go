package tui

import (
	"strconv"
	"strings"
	"sync"
)

// Role labels a projected chat component's origin.
type Role string

const (
	RoleUser       Role = "user"
	RoleAssistant  Role = "assistant"
	RoleThinking   Role = "thinking"
	RoleToolCall   Role = "tool_call"
	RoleToolResult Role = "tool_result"
	RoleError      Role = "error"
	RoleSystem     Role = "system"
)

// Message is the render projection consumed by ChatLog. It is deliberately not
// the authoritative conversation store; ConversationModel keeps committed
// history, a single assistant draft, and transient progress state separately.
type Message struct {
	ID        string
	Role      Role
	Text      string
	Streaming bool
	Images    []ImageComponent

	ToolName   string
	ToolArgs   string
	ToolCallID string
	IsError    bool
}

// ChatSnapshot is the immutable render projection consumed by ChatLog. The
// conversation model owns richer state; snapshots expose stable components with
// revisions so the renderer can cache wrapping per component instead of
// reasoning about the whole transcript as one flat value.
type ChatSnapshot struct {
	Revision   uint64
	Components []ChatComponent
}

// ChatComponentKind identifies the structured payload carried by a chat
// component.
type ChatComponentKind string

const (
	ChatComponentUser              ChatComponentKind = "user"
	ChatComponentAssistant         ChatComponentKind = "assistant"
	ChatComponentThinking          ChatComponentKind = "thinking"
	ChatComponentToolExecution     ChatComponentKind = "tool_execution"
	ChatComponentError             ChatComponentKind = "error"
	ChatComponentSystem            ChatComponentKind = "system"
	ChatComponentImage             ChatComponentKind = "image"
	ChatComponentCompactionSummary ChatComponentKind = "compaction_summary"
	ChatComponentBranchSummary     ChatComponentKind = "branch_summary"
	ChatComponentSkillInvocation   ChatComponentKind = "skill_invocation"
	ChatComponentDiff              ChatComponentKind = "diff"
	ChatComponentCustom            ChatComponentKind = "custom"
)

// ToolExecutionComponent is the structured render payload for one logical tool
// invocation. It is updated in place as call/progress/result events arrive.
type ToolExecutionComponent struct {
	CallID  string
	Name    string
	Args    string
	Output  string
	State   toolExecutionState
	IsError bool
}

type ImageComponent struct {
	Source         string `json:"source,omitempty"`
	MIMEType       string `json:"mimeType,omitempty"`
	AltText        string `json:"altText,omitempty"`
	Width          int    `json:"width,omitempty"`
	Height         int    `json:"height,omitempty"`
	ContentBlockID string `json:"contentBlockId,omitempty"`
	Data           string `json:"data,omitempty"`
}

func (i ImageComponent) FallbackText() string {
	alt := strings.TrimSpace(i.AltText)
	if strings.HasPrefix(alt, "[Image #") {
		return alt
	}
	if alt == "" {
		alt = strings.TrimSpace(i.Source)
	}
	if alt == "" {
		return "[image]"
	}
	return "[image: " + alt + "]"
}

type CustomComponent struct {
	Type     string `json:"type,omitempty"`
	Name     string `json:"name,omitempty"`
	Title    string `json:"title,omitempty"`
	Summary  string `json:"summary,omitempty"`
	SourceID string `json:"sourceId,omitempty"`
}

func (c CustomComponent) FallbackText() string {
	for _, value := range []string{c.Title, c.Summary, c.Name, c.Type} {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return "[custom content]"
}

// ChatComponent is one renderable conversation component. It is the structured
// projection consumed by ChatLog; Message() provides the legacy flat projection
// for older callers.
type ChatComponent struct {
	ID        string
	Kind      ChatComponentKind
	Role      Role
	Text      string
	Streaming bool

	ToolExecution ToolExecutionComponent
	ToolDetails   any
	Image         ImageComponent
	Images        []ImageComponent
	Custom        CustomComponent
	Collapsed     bool
	Revision      uint64
	// Fallback marks a compaction summary the model did not write — the model
	// call failed and the SDK substituted a local summary.
	Fallback bool
}

// toolComponentRole resolves the role a tool execution renders as: errors and
// cancellations present as errors regardless of the call/result role.
func toolComponentRole(c ChatComponent) Role {
	role := c.Role
	if role == "" {
		role = RoleToolCall
	}
	if c.ToolExecution.IsError || c.ToolExecution.State == toolExecutionError || c.ToolExecution.State == toolExecutionCancelled {
		role = RoleError
	}
	return role
}

func (c ChatComponent) Message() Message {
	msg := Message{ID: c.ID, Role: c.Role, Text: c.Text, Streaming: c.Streaming, Images: append([]ImageComponent(nil), c.Images...)}
	if c.Kind == ChatComponentToolExecution {
		role := toolComponentRole(c)
		msg.Role = role
		// c.Text is built by exactly this call when the component comes from
		// the model projection, so reuse it instead of re-concatenating the
		// tool's args and output. Rebuilding here made every wrap of the
		// transcript re-derive the display text of every tool execution.
		if c.Text != "" {
			msg.Text = c.Text
		} else {
			msg.Text = toolProjectionText(role, c.ToolExecution.Name, c.ToolExecution.Args, c.ToolExecution.Output)
		}
		msg.ToolName = c.ToolExecution.Name
		msg.ToolArgs = c.ToolExecution.Args
		msg.ToolCallID = c.ToolExecution.CallID
		msg.IsError = c.ToolExecution.IsError
	}
	if c.Kind == ChatComponentError {
		msg.IsError = true
	}
	return msg
}

type conversationItemKind uint8

const (
	conversationItemUser conversationItemKind = iota
	conversationItemAssistant
	conversationItemToolExecution
	conversationItemError
	conversationItemSystem
	conversationItemThinking
	conversationItemImage
	conversationItemCustom
	conversationItemCompactionSummary
)

type toolExecutionState uint8

const (
	toolExecutionExecuting toolExecutionState = iota
	toolExecutionComplete
	toolExecutionError
	toolExecutionCancelled
)

type conversationItem struct {
	id       string
	kind     conversationItemKind
	role     Role
	revision uint64
	text     string

	toolCallID  string
	toolName    string
	toolArgs    string
	toolOutput  string
	toolDetails any
	toolState   toolExecutionState
	isError     bool
	collapsed   bool
	image       ImageComponent
	images      []ImageComponent
	// tokensBefore is the context size a compaction reclaimed. Zero when the
	// session did not report one.
	tokensBefore int
	// fallback marks a compaction whose model call failed and degraded to a
	// local summary; the chat log renders it on the fallback tint.
	fallback bool

	// projection memoizes component(). Every mutation of an item assigns a
	// fresh revision, so a memo tagged with the revision it was built from is
	// self-invalidating. Without it, projecting the model rebuilt every
	// component — including re-concatenating each tool call's args and output
	// into its display text — on every single conversation change.
	projection    *ChatComponent
	projectionRev uint64
}

type assistantDraft struct {
	id       string
	revision uint64
	text     string
}

type transientItem struct {
	id       string
	role     Role
	revision uint64
	text     string
}

// ConversationModel is the frontend conversation state required by the app
// spec: committed history is separate from one in-flight assistant draft and
// transient progress UI. ChatLog receives only a projection of this state.
type ConversationModel struct {
	mu             sync.Mutex
	committed      []conversationItem
	draft          *assistantDraft
	draftAfter     int
	transient      []transientItem
	listeners      map[uint64]func()
	nextListenerID uint64
	nextIDValue    uint64
	revision       uint64
	notifyDepth    int
	dirty          bool
}

func NewConversationModel() *ConversationModel { return &ConversationModel{} }

func (m *ConversationModel) AddListener(fn func()) { _ = m.Subscribe(fn) }

func (m *ConversationModel) Subscribe(fn func()) func() {
	if fn == nil {
		return func() {}
	}
	m.mu.Lock()
	if m.listeners == nil {
		m.listeners = make(map[uint64]func())
	}
	id := m.nextListenerID
	m.nextListenerID++
	m.listeners[id] = fn
	m.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			delete(m.listeners, id)
			m.mu.Unlock()
		})
	}
}

func (m *ConversationModel) notify() {
	m.mu.Lock()
	if m.notifyDepth > 0 {
		m.dirty = true
		m.mu.Unlock()
		return
	}
	listeners := make([]func(), 0, len(m.listeners))
	for _, fn := range m.listeners {
		listeners = append(listeners, fn)
	}
	m.mu.Unlock()
	for _, fn := range listeners {
		fn()
	}
}

func (m *ConversationModel) Batch(fn func()) {
	if fn == nil {
		return
	}
	m.mu.Lock()
	m.notifyDepth++
	m.mu.Unlock()

	fn()

	m.mu.Lock()
	m.notifyDepth--
	dirty := m.notifyDepth == 0 && m.dirty
	if dirty {
		m.dirty = false
	}
	listeners := make([]func(), 0, len(m.listeners))
	if dirty {
		for _, listener := range m.listeners {
			listeners = append(listeners, listener)
		}
	}
	m.mu.Unlock()

	for _, listener := range listeners {
		listener()
	}
}

func (m *ConversationModel) nextID(prefix string) string {
	m.nextIDValue++
	return prefix + "-" + strconv.FormatUint(m.nextIDValue, 10)
}

func (m *ConversationModel) nextRevision() uint64 {
	m.revision++
	return m.revision
}

func (m *ConversationModel) Messages() []Message { return legacyMessagesFromSnapshot(m.Snapshot()) }

func (m *ConversationModel) MessagesSnapshot() []Message {
	return legacyMessagesFromSnapshot(m.Snapshot())
}

func legacyMessagesFromSnapshot(snapshot ChatSnapshot) []Message {
	out := make([]Message, 0, len(snapshot.Components))
	for _, component := range snapshot.Components {
		if component.Kind == ChatComponentThinking && component.Collapsed && !component.Streaming {
			continue
		}
		out = append(out, component.Message())
	}
	return out
}

// IsEmpty reports whether the transcript has no content.
//
// Cheaper than len(Messages()): the header's visibility is reconciled on every
// layout pass, and Messages() projects and copies the whole transcript.
func (m *ConversationModel) IsEmpty() bool {
	if m == nil {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.committed) == 0 && len(m.transient) == 0 && m.draft == nil
}

func (m *ConversationModel) Snapshot() ChatSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	components := m.projectComponentsLocked()
	return ChatSnapshot{Revision: m.revision, Components: components}
}

func (m *ConversationModel) projectComponentsLocked() []ChatComponent {
	// Index rather than range by value: component() memoizes onto the item, so
	// a copy would rebuild the projection every time and throw the memo away.
	split := min(m.draftAfter, len(m.committed))
	out := make([]ChatComponent, 0, len(m.committed)+len(m.transient)+1)
	for i := range split {
		out = append(out, m.committed[i].component())
	}
	if m.draft != nil {
		out = append(out, ChatComponent{ID: m.draft.id, Kind: ChatComponentAssistant, Role: RoleAssistant, Text: m.draft.text, Streaming: true, Revision: m.draft.revision})
	}
	for i := split; i < len(m.committed); i++ {
		out = append(out, m.committed[i].component())
	}
	for _, item := range m.transient {
		out = append(out, ChatComponent{ID: item.id, Kind: kindForRole(item.role), Role: item.role, Text: item.text, Streaming: true, Revision: item.revision})
	}
	return out
}

// component projects the item for rendering, reusing the memo when the item
// has not changed since it was built. The pointer receiver lets the memo be
// stored back onto the item; callers must therefore index into the slice
// (m.committed[i]) rather than ranging by value.
func (i *conversationItem) component() ChatComponent {
	if i.projection != nil && i.projectionRev == i.revision {
		return *i.projection
	}
	component := i.buildComponent()
	i.projection = &component
	i.projectionRev = i.revision
	return component
}

func (i *conversationItem) buildComponent() ChatComponent {
	component := ChatComponent{ID: i.id, Revision: i.revision}
	switch i.kind {
	case conversationItemUser:
		component.Kind = ChatComponentUser
		component.Role = RoleUser
		component.Text = i.text
		component.Images = append([]ImageComponent(nil), i.images...)
	case conversationItemAssistant:
		component.Kind = ChatComponentAssistant
		component.Role = RoleAssistant
		component.Text = i.text
	case conversationItemToolExecution:
		component.Kind = ChatComponentToolExecution
		component.Role = i.role
		if component.Role == "" {
			component.Role = RoleToolCall
		}
		component.ToolExecution = ToolExecutionComponent{CallID: i.toolCallID, Name: i.toolName, Args: i.toolArgs, Output: i.toolOutput, State: i.toolState, IsError: i.isError}
		component.ToolDetails = i.toolDetails
		// Message() derives the effective role and then builds the same
		// display text; calling it here built that text twice per tool
		// component. Resolve the role directly instead.
		component.Text = toolProjectionText(toolComponentRole(component), i.toolName, i.toolArgs, i.toolOutput)
	case conversationItemError:
		component.Kind = ChatComponentError
		component.Role = RoleError
		component.Text = i.text
	case conversationItemSystem:
		component.Kind = ChatComponentSystem
		component.Role = RoleSystem
		component.Text = i.text
	case conversationItemThinking:
		component.Kind = ChatComponentThinking
		component.Role = RoleThinking
		component.Text = i.text
		component.Collapsed = i.collapsed
	case conversationItemImage:
		component.Kind = ChatComponentImage
		component.Role = i.role
		component.Text = i.image.FallbackText()
		component.Image = i.image
		component.Images = []ImageComponent{i.image}
	case conversationItemCompactionSummary:
		component.Kind = ChatComponentCompactionSummary
		component.Role = RoleSystem
		component.Text = i.text
		component.Fallback = i.fallback
		title := compactionTitle(i.tokensBefore)
		if i.fallback {
			title = fallbackCompactionTitle(i.tokensBefore)
		}
		component.Custom = CustomComponent{
			Type:    compactionCustomType,
			Title:   title,
			Summary: i.text,
		}
	default:
		component.Kind = kindForRole(i.role)
		component.Role = i.role
		component.Text = i.text
		// A message under a role the app does not own is a custom component.
		// Carrying the role through as the custom type is what makes it
		// addressable: plugin renderers are keyed by custom type, so without
		// this the payload has no identity and can only ever render as
		// fallback text.
		if component.Kind == ChatComponentCustom {
			component.Custom = CustomComponent{
				Type:     string(i.role),
				SourceID: string(i.role),
				Summary:  i.text,
			}
		}
	}
	return component
}

func kindForRole(role Role) ChatComponentKind {
	switch role {
	case RoleUser:
		return ChatComponentUser
	case RoleAssistant:
		return ChatComponentAssistant
	case RoleThinking:
		return ChatComponentThinking
	case RoleToolCall, RoleToolResult:
		return ChatComponentToolExecution
	case RoleError:
		return ChatComponentError
	case RoleSystem:
		return ChatComponentSystem
	default:
		return ChatComponentCustom
	}
}

func (m *ConversationModel) AddMessage(role Role, text string) int {
	m.committed = append(m.committed, conversationItem{id: m.nextID("item"), kind: conversationItemCustom, role: role, revision: m.nextRevision(), text: strings.TrimRight(text, "\n")})
	idx := len(m.committed) - 1
	m.notify()
	return idx
}

func (m *ConversationModel) AddUserMessage(text string) int {
	return m.AddUserMessageWithImages(text, nil)
}

func (m *ConversationModel) AddUserMessageWithImages(text string, images []ImageComponent) int {
	m.committed = append(m.committed, conversationItem{id: m.nextID("user"), kind: conversationItemUser, revision: m.nextRevision(), text: strings.TrimRight(text, "\n"), images: append([]ImageComponent(nil), images...)})
	idx := len(m.committed) - 1
	m.notify()
	return idx
}

func (m *ConversationModel) AddAssistantMessage(text string) int {
	m.committed = append(m.committed, conversationItem{id: m.nextID("assistant"), kind: conversationItemAssistant, revision: m.nextRevision(), text: strings.TrimRight(text, "\n")})
	idx := len(m.committed) - 1
	m.notify()
	return idx
}

func (m *ConversationModel) BeginAssistantMessage() int { return m.BeginAssistantMessageWithID("") }

func (m *ConversationModel) BeginAssistantMessageWithID(id string) int {
	if m.draft != nil {
		return len(m.committed)
	}
	if id == "" {
		id = m.nextID("assistant")
	}
	m.draft = &assistantDraft{id: id, revision: m.nextRevision()}
	m.draftAfter = len(m.committed)
	m.notify()
	return len(m.committed)
}

func (m *ConversationModel) AppendAssistantText(delta string) {
	if m.draft == nil {
		return
	}
	m.draft.text += delta
	m.draft.revision = m.nextRevision()
	m.notify()
}

func (m *ConversationModel) SetAssistantTextIfEmpty(text string) {
	if text == "" || m.draft == nil || m.draft.text != "" {
		return
	}
	m.ClearThinking()
	m.draft.text = text
	m.draft.revision = m.nextRevision()
	m.notify()
}

func (m *ConversationModel) SetAssistantText(text string) {
	if m.draft == nil {
		return
	}
	text = strings.TrimRight(text, "\n")
	if m.draft.text == text {
		return
	}
	m.ClearThinking()
	m.draft.text = text
	m.draft.revision = m.nextRevision()
	m.notify()
}

func (m *ConversationModel) EndAssistantMessage() {
	if m.draft == nil {
		return
	}
	draft := m.draft
	m.draft = nil
	if strings.TrimRight(draft.text, "\n") != "" {
		item := conversationItem{id: draft.id, kind: conversationItemAssistant, revision: m.nextRevision(), text: strings.TrimRight(draft.text, "\n")}
		idx := len(m.committed)
		if m.draftAfter < len(m.committed) && !m.hasTransientThinking() {
			idx = m.draftAfter
		}
		m.committed = append(m.committed, conversationItem{})
		copy(m.committed[idx+1:], m.committed[idx:])
		m.committed[idx] = item
	} else {
		m.nextRevision()
	}
	m.notify()
}

func (m *ConversationModel) UpsertSystemByPrefix(prefix, text string) {
	text = strings.TrimRight(text, "\n")
	for i := len(m.committed) - 1; i >= 0; i-- {
		if m.committed[i].kind == conversationItemSystem && strings.HasPrefix(m.committed[i].text, prefix) {
			if m.committed[i].text == text {
				return
			}
			m.committed[i].text = text
			m.committed[i].revision = m.nextRevision()
			m.notify()
			return
		}
	}
	m.AddSystem(text)
}

func (m *ConversationModel) AddHistoricalThinking(id, text string) int {
	if id == "" {
		id = m.nextID("thinking")
	}
	m.committed = append(m.committed, conversationItem{id: id, kind: conversationItemThinking, role: RoleThinking, revision: m.nextRevision(), text: strings.TrimRight(text, "\n"), collapsed: true})
	idx := len(m.committed) - 1
	m.notify()
	return idx
}

func (m *ConversationModel) AddImage(role Role, image ImageComponent) int {
	id := image.ContentBlockID
	if id == "" {
		id = m.nextID("image")
	}
	m.committed = append(m.committed, conversationItem{id: id, kind: conversationItemImage, role: role, revision: m.nextRevision(), image: image})
	idx := len(m.committed) - 1
	m.notify()
	return idx
}

func (m *ConversationModel) AddThinking(frames ...string) {
	label := formatThinkingLabel("")
	if len(frames) > 0 {
		label = formatThinkingLabel(frames[0])
	}
	for i := range m.transient {
		if m.transient[i].role == RoleThinking {
			if m.transient[i].text != label {
				m.transient[i].text = label
				m.transient[i].revision = m.nextRevision()
				m.notify()
			}
			return
		}
	}
	m.transient = append(m.transient, transientItem{id: m.nextID("thinking"), role: RoleThinking, revision: m.nextRevision(), text: label})
	m.notify()
}

func (m *ConversationModel) SetThinkingFrame(frame string) {
	label := formatThinkingLabel(frame)
	changed := false
	for i := range m.transient {
		if m.transient[i].role == RoleThinking && m.transient[i].text != label {
			m.transient[i].text = label
			m.transient[i].revision = m.nextRevision()
			changed = true
		}
	}
	if changed {
		m.notify()
	}
}

func (m *ConversationModel) ClearThinking() {
	kept := m.transient[:0]
	changed := false
	for _, item := range m.transient {
		if item.role == RoleThinking {
			changed = true
			continue
		}
		kept = append(kept, item)
	}
	if !changed {
		return
	}
	m.transient = kept
	m.nextRevision()
	m.notify()
}

func formatThinkingLabel(frame string) string {
	frame = strings.TrimSpace(frame)
	if frame == "" {
		return "Thinking..."
	}
	return frame + " Thinking..."
}

func (m *ConversationModel) hasTransientThinking() bool {
	for _, item := range m.transient {
		if item.role == RoleThinking {
			return true
		}
	}
	return false
}

func (m *ConversationModel) AddToolCall(name, args string) { m.AddToolCallWithID("", name, args) }

func (m *ConversationModel) AddToolCallWithID(toolCallID, name, args string) {
	if toolCallID != "" {
		if idx := m.findTool(toolCallID); idx >= 0 {
			m.committed[idx].toolName = name
			m.committed[idx].toolArgs = args
			m.committed[idx].revision = m.nextRevision()
			m.notify()
			return
		}
	}
	m.committed = append(m.committed, conversationItem{id: m.nextID("tool"), kind: conversationItemToolExecution, role: RoleToolCall, revision: m.nextRevision(), toolCallID: toolCallID, toolName: name, toolArgs: args, toolState: toolExecutionExecuting})
	if m.draft != nil && m.draft.text == "" {
		m.draftAfter = len(m.committed)
	}
	m.notify()
}

func (m *ConversationModel) AddToolProgress(toolCallID, chunk string) {
	if chunk == "" {
		return
	}
	var idx int
	if toolCallID != "" {
		idx = m.findTool(toolCallID)
	} else {
		idx = m.findLastTool()
	}
	if idx < 0 {
		return
	}
	m.committed[idx].toolOutput += chunk
	m.committed[idx].revision = m.nextRevision()
	m.notify()
}

func (m *ConversationModel) AddToolResult(output string, isError bool) {
	toolName := ""
	for i := len(m.committed) - 1; i >= 0; i-- {
		if m.committed[i].kind == conversationItemToolExecution {
			toolName = m.committed[i].toolName
			break
		}
	}
	m.AddToolResultWithName("", toolName, output, isError)
}

func (m *ConversationModel) AddToolResultWithID(toolCallID, output string, isError bool) {
	m.AddToolResultWithName(toolCallID, "", output, isError)
}

func (m *ConversationModel) AddToolResultWithName(toolCallID, toolName, output string, isError bool) {
	m.AddToolResultWithDetails(toolCallID, toolName, output, isError, nil)
}

func (m *ConversationModel) AddToolResultWithDetails(toolCallID, toolName, output string, isError bool, details any) {
	output = strings.TrimRight(output, "\n")
	var idx int
	if toolCallID != "" {
		idx = m.findTool(toolCallID)
	} else if toolName != "" {
		idx = m.findLastToolByName(toolName)
	} else {
		idx = m.findLastTool()
	}
	if idx >= 0 {
		m.committed[idx].toolOutput = output
		m.committed[idx].toolDetails = details
		m.committed[idx].toolState = toolExecutionComplete
		m.committed[idx].isError = isError
		if toolName != "" {
			m.committed[idx].toolName = toolName
		}
		if isError {
			m.committed[idx].toolState = toolExecutionError
		}
		m.committed[idx].revision = m.nextRevision()
		m.notify()
		return
	}
	state := toolExecutionComplete
	if isError {
		state = toolExecutionError
	}
	m.committed = append(m.committed, conversationItem{id: m.nextID("tool"), kind: conversationItemToolExecution, role: RoleToolResult, revision: m.nextRevision(), toolCallID: toolCallID, toolName: toolName, toolOutput: output, toolDetails: details, toolState: state, isError: isError})
	if m.draft != nil && m.draft.text == "" {
		m.draftAfter = len(m.committed)
	}
	m.notify()
}

func (m *ConversationModel) CancelTool(toolCallID, reason string) {
	reason = strings.TrimRight(reason, "\n")
	var idx int
	if toolCallID != "" {
		idx = m.findTool(toolCallID)
	} else {
		idx = m.findLastTool()
	}
	if idx < 0 {
		return
	}
	m.committed[idx].toolState = toolExecutionCancelled
	m.committed[idx].isError = true
	if reason != "" {
		m.committed[idx].toolOutput = reason
	} else if m.committed[idx].toolOutput == "" {
		m.committed[idx].toolOutput = "cancelled"
	}
	m.committed[idx].revision = m.nextRevision()
	m.notify()
}

func (m *ConversationModel) findTool(toolCallID string) int {
	for i := range m.committed {
		if m.committed[i].kind == conversationItemToolExecution && m.committed[i].toolCallID == toolCallID {
			return i
		}
	}
	return -1
}

func (m *ConversationModel) findLastTool() int {
	for i := len(m.committed) - 1; i >= 0; i-- {
		if m.committed[i].kind == conversationItemToolExecution {
			return i
		}
	}
	return -1
}

func (m *ConversationModel) findLastToolByName(name string) int {
	for i := len(m.committed) - 1; i >= 0; i-- {
		if m.committed[i].kind == conversationItemToolExecution && m.committed[i].toolName == name {
			return i
		}
	}
	return -1
}

func toolProjectionText(role Role, name, args, output string) string {
	if role == RoleToolResult || role == RoleError && name == "" && args == "" {
		return strings.TrimRight(output, "\n")
	}
	return toolDisplayText(name, args, output)
}

func toolDisplayText(name, args, output string) string {
	text := name
	if args != "" {
		text += " " + strings.TrimRight(args, "\n")
	}
	if output != "" {
		if text != "" {
			text += "\n"
		}
		text += strings.TrimRight(output, "\n")
	}
	return text
}

func (m *ConversationModel) AddError(text string) {
	m.committed = append(m.committed, conversationItem{id: m.nextID("error"), kind: conversationItemError, revision: m.nextRevision(), text: strings.TrimRight(text, "\n"), isError: true})
	m.notify()
}

func (m *ConversationModel) AddSystem(text string) {
	m.committed = append(m.committed, conversationItem{id: m.nextID("system"), kind: conversationItemSystem, revision: m.nextRevision(), text: strings.TrimRight(text, "\n")})
	m.notify()
}

// compactionCustomType tags a compaction summary for extension renderers, which
// key off CustomComponent.Type.
const compactionCustomType = "compaction"

// compactionTitle is the header row above a compaction summary. The token count
// is what makes the row worth reading — it says how much context came back.
func compactionTitle(tokensBefore int) string {
	return compactionTitleText("Context compacted", tokensBefore)
}

// fallbackCompactionTitle is the header for a summary the model did not write:
// the compaction degraded to a local summary after the model call failed.
func fallbackCompactionTitle(tokensBefore int) string {
	return compactionTitleText("Local fallback summary", tokensBefore)
}

func compactionTitleText(prefix string, tokensBefore int) string {
	if tokensBefore <= 0 {
		return prefix
	}
	return prefix + " · " + formatTokenCount(tokensBefore) + " tokens summarized"
}

// AddCompactionSummary commits the summary a compaction produced (app.md §9:
// compaction_end "commits a summary message"). Empty summaries are dropped —
// an empty box tells the user nothing and still costs transcript rows.
func (m *ConversationModel) AddCompactionSummary(summary string, tokensBefore int) {
	m.commitCompactionSummary(summary, tokensBefore, false)
}

// AddFallbackCompactionSummary commits the local summary substituted when the
// model call failed. The error leads the body so the degraded provenance is
// the first thing read, and the item is flagged so the chat log renders it on
// the fallback tint instead of the regular compaction box.
func (m *ConversationModel) AddFallbackCompactionSummary(summary, fallbackErr string, tokensBefore int) {
	m.commitCompactionSummary("model call failed: "+fallbackErr+"\n\n"+summary, tokensBefore, true)
}

func (m *ConversationModel) commitCompactionSummary(text string, tokensBefore int, fallback bool) {
	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" {
		return
	}
	m.committed = append(m.committed, conversationItem{
		id:           m.nextID("compaction"),
		kind:         conversationItemCompactionSummary,
		role:         RoleSystem,
		revision:     m.nextRevision(),
		text:         text,
		tokensBefore: tokensBefore,
		fallback:     fallback,
	})
	m.notify()
}

func (m *ConversationModel) IsStreaming() bool { return m.draft != nil }

func (m *ConversationModel) Clear() {
	if len(m.committed) == 0 && m.draft == nil && len(m.transient) == 0 {
		return
	}
	m.committed = nil
	m.draft = nil
	m.transient = nil
	m.nextRevision()
	m.notify()
}
