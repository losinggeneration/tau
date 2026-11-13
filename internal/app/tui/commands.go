package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/ai/store"
	"github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/autocomplete"
)

type CredentialStore = store.CredentialStore

// Command is a registered slash command the user can invoke from the editor
// by typing `/<name>` (and optionally arguments). Handlers run on the
// toolkit loop via Root.HandleAction, so they must not block.
type Command struct {
	// Name is the bare command identifier without the leading slash
	// (e.g. "help", "model"). Used as the completion key.
	Name string

	// Description is a one-line summary shown in the completion popup and
	// used by /help. Should fit comfortably on a single terminal row at
	// 80 columns.
	Description string

	// Handler executes the command when the user submits a line starting
	// with `/<name>`. The remaining text after the command name (with
	// leading whitespace trimmed) is passed as the argument string.
	// Nil means the command is shown for completion but is dispatched
	// elsewhere (typically the session's prompt pipeline, e.g. for
	// extension/skill/plugin commands). The TUI never calls a nil
	// handler; instead the line is forwarded to the session as a normal
	// prompt and the session's command registry handles it.
	Handler func(app *App, args string)

	// CompleteArgs, when set, suggests the text after `/<name> ` as it is
	// typed. prefix is everything typed after the space.
	CompleteArgs func(app *App, prefix string) []autocomplete.Item
}

// Commands is an ordered registry of slash commands. The zero value is
// ready to use via Register; NewCommands returns one pre-populated with
// the built-in interactive commands.
//
// The registry is safe for concurrent registration and lookup. The
// expected use is: the TUI seeds the registry at construction; plugins
// (and any other runtime source) may call Register/Unregister from any
// goroutine; the popup filter and submit dispatch read from it on the
// app loop without taking long-held locks.
type Commands struct {
	mu     sync.RWMutex
	byName map[string]Command
	order  []string
	// conflicts tracks the names that have been registered by two
	// different sources (e.g. a built-in and a plugin, or two
	// plugins). The map value is the list of sources that
	// registered this name, in chronological order. The first
	// source is the canonical owner; subsequent registrations are
	// noted as conflicts but the first one wins so user-facing
	// dispatch continues to behave consistently. Commands
	// surfaced by Conflicts() can be displayed in /help or
	// settings so the user can see which commands shadow each
	// other.
	conflicts map[string][]string
	// canonicalSource records the source that first registered
	// each command name. Used to skip no-op re-registrations by
	// the same source and to anchor the conflict list. Keys that
	// have no recorded source are silently treated as
	// "anonymous"; their conflicts are still tracked but no
	// canonical owner is named.
	canonicalSource map[string]string
}

// NewCommands returns a Commands registry seeded with the built-in
// interactive commands:
//
//	/settings, /model, /scoped-models, /export, /import,
//	/copy, /name, /session, /hotkeys, /fork, /tree,
//
// plus the convenience /help (standard across interactive CLIs).
//
// /reload is owned by the session's command pipeline (it re-discovers
// skills/prompts/themes and reloads plugins); the TUI forwards the line
// when the session advertises it. /export, /copy, /fork, /tree, /login,
// /logout, /hotkeys, /compact, /resume, /settings, /scoped-models,
// /layout, /new, /model, /name, /session, /show-model, and /quit have
// live handlers.
func NewCommands() *Commands {
	c := &Commands{byName: map[string]Command{}}
	c.Register(Command{
		Name:        "help",
		Description: "Show available commands",
		Handler:     cmdHelp,
	})
	c.Register(Command{
		Name:        "model",
		Description: "Switch the active model",
		Handler:     cmdModel,
	})
	c.Register(Command{
		Name:        "show-model",
		Description: "Show the active model",
		Handler:     cmdShowModel,
	})
	c.Register(Command{
		Name:        "quit",
		Description: "Exit interactive mode",
		Handler:     cmdQuit,
	})
	// Interactive builtins (in display order). Most are stubbed
	// today and will be replaced as their implementation lands.
	// /compact is wired to sdk.Session.Compact because that API is
	// already available on the session interface; the rest follow
	// when
	// their respective backends (OAuth, session store, etc.) are
	// hooked up.
	c.Register(Command{Name: "settings", Description: "Open settings", Handler: cmdSettingsOverlay})
	c.Register(Command{Name: "scoped-models", Description: "Scope available models", Handler: cmdScopedModelsOverlay})
	c.Register(Command{Name: "export", Description: "Export the current session", Handler: cmdExport})
	c.Register(Command{Name: "import", Description: "Import a session", Handler: cmdImport})
	c.Register(Command{Name: "copy", Description: "Copy the last reply", Handler: cmdCopy})
	c.Register(Command{Name: "name", Description: "Name this session", Handler: cmdName})
	c.Register(Command{Name: "session", Description: "Open the session workspace", Handler: cmdSessionWorkspace})
	// /sessions is the plural alias users type coming from `tau sessions`
	// on the shell. Same workspace; the shell builtin's "run from the
	// shell" reply is useless inside the TUI.
	c.Register(Command{Name: "sessions", Description: "Browse saved sessions (resume, fork, rename, pin)", Handler: cmdSessionWorkspace})
	c.Register(Command{Name: "mcp", CompleteArgs: completeMCPArgs, Description: "Manage MCP servers: sign in, reconnect, enable or disable, and change exposure", Handler: cmdMCP})
	c.Register(Command{Name: "plugins", CompleteArgs: completePluginsArgs, Description: "List plugins and tools, or enable and disable them per session, project or globally", Handler: cmdPlugins})
	c.Register(Command{Name: "hotkeys", Description: "Show keybinding hints", Handler: cmdHotkeys})
	c.Register(Command{Name: "layout", Description: "Switch layout: fixed or document", Handler: cmdLayout})
	c.Register(Command{Name: "fork", Description: "Fork session (space: pick point)", Handler: cmdForkOrStub})
	c.Register(Command{Name: "forget", Description: "Drop messages from a picked point onward", Handler: cmdForget})
	c.Register(Command{Name: "tree", Description: "Show the session tree", Handler: cmdTreeOrStub})
	c.Register(Command{Name: "login", Description: "Sign in", Handler: cmdLogin})
	c.Register(Command{Name: "logout", Description: "Sign out", Handler: cmdLogout})
	c.Register(Command{Name: "new", Description: "Start a new session", Handler: cmdNew})
	c.Register(Command{Name: "compact", Description: "Compact session history", Handler: cmdCompact})
	// nil handler: the session owns /retry, the entry is for completion.
	c.Register(Command{Name: "retry", Description: "Resume an interrupted or failed run"})
	c.Register(Command{Name: "resume", Description: "Resume a saved session", Handler: cmdResume})
	c.Register(Command{Name: "history", Description: "Show the full session history (uncompacted)", Handler: cmdHistory})
	// /reload is deliberately not registered here: the session's command
	// pipeline owns it (artifact + plugin reload), and a local entry — even a
	// stub — would shadow that handler.
	return c
}

// Register adds (or replaces) a command. Later registrations with the
// same name overwrite earlier ones; the position in the display order
// is the most recent registration's position. Safe to call from any
// goroutine — the typical use is a plugin calling Register after the
// TUI is up.
//
// Source is an optional label (e.g. "builtin", "plugin",
// "session:auth") that identifies who registered the command. When
// two registrations disagree on source for the same name, the
// conflict is recorded (see Conflicts) so the help and settings UIs
// can surface the overlap. The first registration wins for
// dispatch.
func (c *Commands) Register(cmd Command) {
	c.registerWithSource(cmd, "")
}

func (c *Commands) registerWithSource(cmd Command, source string) {
	c.mu.Lock()
	if c.byName == nil {
		c.byName = map[string]Command{}
	}
	existing, exists := c.byName[cmd.Name]
	if !exists {
		// First registration for this name — wins dispatch
		// and is recorded with the source it arrived from.
		c.order = append(c.order, cmd.Name)
		c.byName[cmd.Name] = cmd
		if source != "" {
			// Remember the canonical source so a later
			// conflicting registration can be reported
			// against it. Use a parallel map keyed by
			// name to avoid leaking the source into the
			// dispatch entry.
			if c.canonicalSource == nil {
				c.canonicalSource = map[string]string{}
			}
			c.canonicalSource[cmd.Name] = source
		}
	} else if source != "" {
		// Subsequent registration with a source label. Record
		// the conflict against the new source. The first
		// registration stays as the canonical owner of
		// dispatch so user-facing behavior is consistent.
		if c.conflicts == nil {
			c.conflicts = map[string][]string{}
		}
		// Drop our own copy of the entry so the canonical
		// owner is the one in byName.
		_ = existing
		conflictList := c.conflicts[cmd.Name]
		// Skip if the canonical source is the same — that
		// means the plugin is re-registering its own command
		// (a no-op) and shouldn't be reported as a conflict.
		if c.canonicalSource[cmd.Name] == source {
			c.mu.Unlock()
			return
		}
		alreadySeen := false
		for _, s := range conflictList {
			if s == source {
				alreadySeen = true
				break
			}
		}
		if !alreadySeen {
			c.conflicts[cmd.Name] = append(conflictList, source)
		}
		// Re-assign to preserve the canonical entry. (This is
		// a no-op if existing was already in byName.)
		c.byName[cmd.Name] = existing
	} else {
		// Anonymous re-registration (no source label). Per
		// the Register contract, this replaces the existing
		// entry while keeping its position in the display
		// order. This is the path used by tests and by code
		// that wants to override a built-in at runtime.
		c.byName[cmd.Name] = cmd
	}
	c.mu.Unlock()
}

// Unregister removes a command. Safe to call from any goroutine. Returns
// true if the command was registered and is now gone.
func (c *Commands) Unregister(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.byName[name]; !ok {
		return false
	}
	delete(c.byName, name)
	for i, n := range c.order {
		if n == name {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	return true
}

// Lookup returns the command registered under name, if any.
func (c *Commands) Lookup(name string) (Command, bool) {
	c.mu.RLock()
	cmd, ok := c.byName[name]
	c.mu.RUnlock()
	return cmd, ok
}

// Conflicts returns a snapshot of command names that have been
// registered by more than one source. The map value is the list of
// sources that registered the name (excluding the canonical owner
// that registered first). The returned map is a fresh copy; callers
// may mutate it without affecting the registry.
func (c *Commands) Conflicts() map[string][]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.conflicts) == 0 {
		return nil
	}
	out := make(map[string][]string, len(c.conflicts))
	for name, sources := range c.conflicts {
		cp := append([]string(nil), sources...)
		out[name] = cp
	}
	return out
}

// All returns every registered command in display order. The returned
// slice is freshly allocated; mutating it does not affect the registry.
func (c *Commands) All() []Command {
	c.mu.RLock()
	out := make([]Command, 0, len(c.order))
	for _, n := range c.order {
		out = append(out, c.byName[n])
	}
	c.mu.RUnlock()
	return out
}

// Filter returns commands whose names start with prefix (case-insensitive).
// An empty prefix returns everything in registration order.
func (c *Commands) Filter(prefix string) []Command {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if prefix == "" {
		out := make([]Command, 0, len(c.order))
		for _, n := range c.order {
			out = append(out, c.byName[n])
		}
		return out
	}
	lower := strings.ToLower(prefix)
	var out []Command
	for _, n := range c.order {
		if strings.HasPrefix(strings.ToLower(n), lower) {
			out = append(out, c.byName[n])
		}
	}
	return out
}

// ExternalCommandsProvider is implemented by any session or backend
// that can surface additional slash commands (built-in agent commands,
// prompt templates, skills, extension/plugin commands) for completion.
// The TUI uses this to merge non-local commands into its popup without
// having to know how to dispatch them — anything not found in the
// local registry is forwarded to the session at submit time, where
// the session's own command pipeline handles execution.
//
// A nil return is fine; the TUI falls back to its local registry only.
type ExternalCommandsProvider interface {
	SlashCommands() []ExternalCommand
}

// ExternalCommand is the subset of a session-provided slash command that
// the TUI needs for completion. It is independent of the TUI's Command
// type so the session can stay decoupled from TUI-specific concerns
// like the App receiver on the handler signature.
type ExternalCommand struct {
	Name         string
	Description  string
	ArgumentHint string
	Source       string // "builtin", "skill", "plugin", "prompt", etc.
}

// parseCommandLine splits a line into (command name, arguments). It expects
// the line to begin with `/`. Returns ok=false if the line does not look
// like a slash command (empty, no slash, or whitespace before slash).
func parseCommandLine(line string) (name string, args string, ok bool) {
	if !strings.HasPrefix(line, "/") {
		return "", "", false
	}
	rest := line[1:]
	if rest == "" {
		return "", "", false
	}
	// Split on first whitespace run.
	for i, r := range rest {
		if r == ' ' || r == '\t' {
			return rest[:i], strings.TrimSpace(rest[i+1:]), true
		}
	}
	return rest, "", true
}

// Built-in command handlers. Each is invoked with the app pointer so it can
// mutate conversation state, footer state, or request shutdown. All run on
// the toolkit loop.

func cmdHelp(app *App, _ string) {
	var b strings.Builder

	// Interactive builtins — commands registered locally in the TUI.
	// These are intercepted before the prompt pipeline; commands the
	// TUI doesn't own are forwarded to the session.
	b.WriteString("Interactive commands:\n")
	for _, cmd := range app.cmds.All() {
		fmt.Fprintf(&b, "  /%-8s  %s\n", cmd.Name, cmd.Description)
	}

	// Session/extension commands — surfaced by the agent session
	// (extension commands, prompt templates, skills). These fall
	// through to the prompt pipeline rather than being handled
	// locally.
	if external := app.externalCommands(); len(external) > 0 {
		b.WriteString("\nSession commands:\n")
		for _, cmd := range external {
			hint := ""
			if cmd.ArgumentHint != "" {
				hint = " " + cmd.ArgumentHint
			}
			fmt.Fprintf(&b, "  /%-8s  %s%s\n", cmd.Name, cmd.Description, hint)
		}
	}

	// Keybinding hints. Current bindings are:
	//   Ctrl+C       exit (confirm-to-quit)
	//   Tab          editor completion
	//   Enter        submit, or steer while working
	//   Shift+Enter  newline (Alt+Enter may be equivalent in some terminals)
	//   Alt+Backspace pop the first queued follow-up
	//   /            slash-command mode
	b.WriteString("\nKeybindings:\n")
	b.WriteString("  ctrl-c   exit (press twice to confirm)\n")
	b.WriteString("  tab      complete command or model\n")
	b.WriteString("  enter      submit, or steer while working\n")
	b.WriteString("  shift-enter newline (alt-enter may be equivalent in some terminals)\n")
	b.WriteString("  alt+bs     pop the first queued follow-up\n")
	b.WriteString("  /          slash commands\n")

	b.WriteString("\nPress Tab in the editor to complete a command name.")

	// Surface any command-name conflicts so the user can see why
	// a custom command might shadow a built-in. Pi's /help
	// similarly lists disputed commands at the bottom.
	if conflicts := app.cmds.Conflicts(); len(conflicts) > 0 {
		b.WriteString("\n\nCommand conflicts:")
		// Stable order so the section is reproducible across runs.
		names := make([]string, 0, len(conflicts))
		for name := range conflicts {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(&b, "\n  /%-8s  shadowed by: %s", name, strings.Join(conflicts[name], ", "))
		}
	}

	app.conv.AddSystem(strings.TrimRight(b.String(), "\n"))
	app.refreshChat()
}

func cmdNew(app *App, _ string) {
	if app == nil || app.sess == nil {
		return
	}
	state := app.sess.State()
	cwd := state.CWD()
	if cwd == "" {
		app.conv.AddError("new: session working directory is unavailable")
		app.refreshChat()
		return
	}
	if app.sessionFactory == nil {
		app.conv.AddError("new: session switching is unavailable")
		app.refreshChat()
		return
	}
	// Preserve the currently selected model when creating the new session.
	currentModel := app.currentModel
	newSess, err := app.sessionFactory(app.ctxOrBackground(), sdk.SessionOptions{CWD: cwd, Model: currentModel})
	if err != nil {
		app.conv.AddError("new: " + err.Error())
		app.refreshChat()
		return
	}
	if app.extensionDialog != nil || app.loginDialog != nil {
		app.dismissDialog()
	}
	if err := app.sess.Close(app.ctxOrBackground()); err != nil {
		app.conv.AddError("new: close current session: " + err.Error())
		app.refreshChat()
		if closeErr := newSess.Close(app.ctxOrBackground()); closeErr != nil {
			app.conv.AddError("new: cleanup replacement session: " + closeErr.Error())
			app.refreshChat()
		}
		return
	}
	app.sess = newSess
	// The observer is per-session state; the replacement has none until it is
	// told, and without it compaction goes back to being invisible.
	app.wireSessionEvents()
	app.currentModel = newSess.State().Model()
	app.replaceConversation(NewConversationModel())
	app.pendingModelKey = ""
	app.pendingQueue = nil
	if app.pending != nil {
		app.refreshPending(app.loopContext())
	}
	if app.footer != nil && app.footer.data != nil {
		app.footer.data.model = app.currentModel
		app.footer.data.cwd = cwd
	}
	app.refreshFooterStats(app.loopContext())
	app.conv.AddSystem("Started a new session")
	app.refreshChat()
}

func (a *App) replaceConversation(conv *ConversationModel) {
	a.replaceConversationWithMode(conv, true)
}

func (a *App) replaceConversationWithMode(conv *ConversationModel, clearChat bool) {
	if a == nil || conv == nil {
		return
	}
	if a.extensionDialog != nil || a.loginDialog != nil {
		a.dismissDialog()
	}
	if a.app != nil {
		a.app.ResetMouseState()
	}
	a.conv = conv
	a.resetHistoryPaging()
	if a.chat != nil {
		if clearChat {
			a.chat.SetConversation(conv)
		} else {
			a.chat.SetConversationPreserveRender(conv)
		}
	}
	if a.conversationEvents != nil {
		a.conversationEvents = newConversationEventAdapter(a)
	}
	if clearChat && a.scroll != nil {
		// The transcript is being replaced wholesale; whatever the user was
		// scrolled to in the old one is gone, so follow-the-tail resumes for
		// the new transcript. setAutoScrollOff also drops the
		// pendingNewMessages counter — there are no longer any "new messages
		// since scrolling" to advertise against a transcript that no longer
		// exists.
		a.setAutoScrollOff(false)
		if ctx := a.loopContext(); ctx != nil {
			a.scroll.ScrollTop(ctx)
		}
	}
}

// cmdModel opens the model picker via the unified inline selector. With
// the unified selector design, the model picker is just another mode
// of the same widget the completion popup uses; switching modes is as
// simple as rewriting the editor text to `/model ` (which the App's
// trySubmit already does). This handler is still wired because plugin
// code or test fixtures may invoke it directly.
func cmdModel(app *App, _ string) {
	if app == nil || app.editor == nil {
		return
	}
	ctx := app.loopContext()
	if ctx == nil {
		return
	}
	app.editor.SetText(ctx, "/model ")
	app.refreshSelector("/model ", ctx)
}

func cmdShowModel(app *App, _ string) {
	if app == nil {
		return
	}
	if app.sess == nil {
		app.conv.AddError("show-model: no active session")
		app.refreshChat()
		return
	}
	model := app.sess.State().Model()
	if model == "" {
		model = "(none)"
	}
	app.conv.AddSystem("Current model: " + model)
	app.refreshChat()
}

func cmdQuit(app *App, _ string) {
	app.quit()
}

// cmdHistory re-seeds the chat log with the entire persisted transcript,
// ignoring compaction truncation: the default resume view starts at the most
// recent compaction, so pre-compaction messages are otherwise unreachable from
// the TUI. This is display-only; the agent's context stays compacted.
func cmdHistory(app *App, _ string) {
	if app == nil || app.sess == nil {
		return
	}
	full, ok := app.sess.(sdk.FullMessageLister)
	if !ok {
		app.conv.AddError("history: full history is unavailable for this session")
		app.refreshChat()
		return
	}
	messages := full.GetAllMessages()
	app.replaceConversation(NewConversationModel())
	app.populateChatFromMessages(messages, "full session history (context stays compacted)")
	app.refreshChat()
}

func cmdResume(app *App, args string) {
	if app == nil {
		return
	}
	id := strings.TrimSpace(args)
	if id == "" {
		app.conv.AddError("resume: usage: /resume <session-id>")
		app.refreshChat()
		return
	}
	if app.sessionFactory == nil {
		app.conv.AddError("resume: session switching is unavailable")
		app.refreshChat()
		return
	}
	current := app.sess
	state := app.sessionState()
	newSess, err := app.sessionFactory(app.ctxOrBackground(), sdk.SessionOptions{
		SessionID: id,
		CWD:       state.CWD(),
		UI:        nil,
	})
	if err != nil {
		app.conv.AddError("resume: " + err.Error())
		app.refreshChat()
		return
	}
	if current != nil {
		if app.extensionDialog != nil || app.loginDialog != nil {
			app.dismissDialog()
		}
		if err := current.Close(app.ctxOrBackground()); err != nil {
			if closeErr := newSess.Close(app.ctxOrBackground()); closeErr != nil {
				app.conv.AddError("resume: cleanup replacement session: " + closeErr.Error())
			}
			app.conv.AddError("resume: close current session: " + err.Error())
			app.refreshChat()
			return
		}
	}
	app.sess = newSess
	// The observer is per-session state; the replacement has none until it is
	// told, and without it compaction goes back to being invisible.
	app.wireSessionEvents()
	app.currentModel = newSess.State().Model()
	app.pendingModelKey = ""
	app.pendingQueue = nil
	if app.pending != nil {
		app.refreshPending(app.loopContext())
	}
	if app.footer != nil && app.footer.data != nil {
		newState := newSess.State()
		app.footer.data.model = app.currentModel
		if cwd := newState.CWD(); cwd != "" {
			app.footer.data.cwd = cwd
		}
	}
	app.refreshFooterStats(app.loopContext())
	app.replaceConversation(NewConversationModel())
	app.seedChatFromSession(newSess, "Resumed session "+newSess.ID())
	app.refreshChat()
}

func appendSDKMessageToConversation(conv *ConversationModel, msg sdk.Message) {
	if conv == nil {
		return
	}
	role := strings.TrimSpace(msg.Role)
	if role == "" {
		return
	}

	if role == "custom" && msg.CustomType == sdk.BashExecutionCustomType {
		if raw, err := json.Marshal(msg.Details); err == nil {
			var execution sdk.BashExecution
			if json.Unmarshal(raw, &execution) == nil && execution.Command != "" {
				args, err := json.Marshal(map[string]any{"command": execution.Command, "exclude_from_context": execution.ExcludeFromContext})
				if err == nil {
					conv.AddToolCallWithID("", "bash", string(args))
					addUserBashResult(conv, "", execution)
					return
				}
			}
		}
	}

	if role == "tool" || role == "toolResult" {
		legacy := false
		for _, block := range msg.Content {
			if block.Type == ai.ContentBlockToolResult {
				legacy = true
				break
			}
		}
		if !legacy {
			conv.addToolResult(msg.ToolCallID, msg.ToolName, msg.Text, msg.IsError, msg.Details, msg.DurationMs, msg.ExcludeFromContext, ToolExecutionComponent{Content: msg.Content, StructuredContent: msg.StructuredContent, NestedCalls: msg.NestedCalls, ToolUsage: msg.ToolUsage})
			return
		}
	}

	if len(msg.Content) > 0 {
		appendContentBlocksToConversation(conv, role, msg.Content, msg)
		return
	}

	text := strings.TrimRight(msg.Text, "\n")
	if text == "" && role != "tool" && role != "toolResult" {
		return
	}
	switch role {
	case "user":
		conv.AddUserMessage(text)
	case "assistant":
		conv.AddAssistantMessage(text)
	case "system":
		conv.AddSystem(text)
	case "tool", "toolResult":
		conv.addToolResult(msg.ToolCallID, msg.ToolName, text, msg.IsError, msg.Details, msg.DurationMs, msg.ExcludeFromContext)
	default:
		conv.AddMessage(Role(role), text)
	}
}

func appendContentBlocksToConversation(conv *ConversationModel, role string, blocks []ai.ContentBlock, msg sdk.Message) {
	var text strings.Builder
	var hasToolResult bool
	for _, block := range blocks {
		switch block.Type {
		case ai.ContentBlockText:
			text.WriteString(block.Text)
		case ai.ContentBlockThinking:
			if strings.TrimRight(text.String(), "\n") != "" {
				conv.AddAssistantMessage(text.String())
				text.Reset()
			}
			thinking := block.Thinking
			if thinking == "" {
				thinking = block.Text
			}
			conv.AddHistoricalThinking(block.ID, thinking)
		case ai.ContentBlockToolCall:
			if strings.TrimRight(text.String(), "\n") != "" {
				conv.AddAssistantMessage(text.String())
				text.Reset()
			}
			conv.AddToolCallWithID(block.ID, block.Name, string(block.Arguments))
		case ai.ContentBlockToolResult:
			hasToolResult = true
			if strings.TrimRight(text.String(), "\n") != "" {
				conv.AddAssistantMessage(text.String())
				text.Reset()
			}
			conv.addToolResult(block.ToolCallID, block.ToolName, toolResultBlockText(block), msg.IsError, msg.Details, msg.DurationMs, msg.ExcludeFromContext)
		case ai.ContentBlockImage:
			if strings.TrimRight(text.String(), "\n") != "" {
				addRoleText(conv, role, text.String(), msg.IsError)
				text.Reset()
			}
			conv.AddImage(Role(role), imageComponentFromBlock(block))
		}
	}

	remaining := strings.TrimRight(text.String(), "\n")
	if remaining == "" && (hasToolResult || role != "tool" && role != "toolResult") {
		return
	}
	switch role {
	case "user":
		conv.AddUserMessage(remaining)
	case "assistant":
		conv.AddAssistantMessage(remaining)
	case "system":
		conv.AddSystem(remaining)
	case "tool", "toolResult":
		conv.addToolResult(msg.ToolCallID, msg.ToolName, remaining, msg.IsError, msg.Details, msg.DurationMs, msg.ExcludeFromContext)
	default:
		conv.AddMessage(Role(role), remaining)
	}
}

func toolResultBlockText(block ai.ContentBlock) string {
	if block.Text != "" {
		return block.Text
	}
	if block.Data != "" {
		return block.Data
	}
	if len(block.Arguments) > 0 {
		return string(block.Arguments)
	}
	return ""
}

func addRoleText(conv *ConversationModel, role, text string, isError bool) {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return
	}
	switch role {
	case "user":
		conv.AddUserMessage(text)
	case "assistant":
		conv.AddAssistantMessage(text)
	case "system":
		conv.AddSystem(text)
	case "tool", "toolResult":
		conv.AddToolResult(text, isError)
	default:
		conv.AddMessage(Role(role), text)
	}
}

func imageComponentFromBlock(block ai.ContentBlock) ImageComponent {
	alt := strings.TrimSpace(block.Name)
	if alt == "" {
		alt = strings.TrimSpace(block.Text)
	}
	source := strings.TrimSpace(block.ID)
	if source == "" && block.Data != "" {
		source = fmt.Sprintf("%d bytes", len(block.Data))
	}
	return ImageComponent{Source: source, MIMEType: strings.TrimSpace(block.MimeType), AltText: alt, ContentBlockID: strings.TrimSpace(block.ID), Data: strings.TrimSpace(block.Data)}
}

func (a *App) populateChatFromSession(sess sdk.Session) {
	if a == nil || sess == nil || a.conv == nil {
		return
	}
	lister, ok := sess.(sdk.MessageLister)
	if !ok {
		return
	}
	a.populateChatFromMessages(lister.GetMessages(), "")
}

// seedChatFromSession installs a resumed transcript into the chat log. The
// chat log wraps only a tail window of it and grows the window from real
// renders as the user scrolls up; no height is estimated.
func (a *App) seedChatFromSession(sess sdk.Session, notice string) {
	if a == nil || sess == nil || a.conv == nil {
		return
	}
	lister, ok := sess.(sdk.MessageLister)
	if !ok {
		if notice != "" {
			a.conv.AddSystem(notice)
		}
		return
	}
	messages := lister.GetMessages()
	if a.seedChatFromHistory(sess) {
		if notice != "" {
			a.conv.AddSystem(notice)
		}
	} else {
		a.populateChatFromMessages(messages, notice)
	}
	a.populateEditorHistoryFromMessages(messages)
}

func (a *App) populateChatFromMessages(messages []sdk.Message, notice string) {
	if a == nil || a.conv == nil {
		return
	}
	a.conv.Batch(func() {
		for _, msg := range messages {
			appendSDKMessageToConversation(a.conv, msg)
		}
		if notice != "" {
			a.conv.AddSystem(notice)
		}
	})
}

func (a *App) populateEditorHistoryFromMessages(messages []sdk.Message) {
	if a == nil || a.editor == nil || a.editor.Model() == nil {
		return
	}
	for _, msg := range messages {
		if strings.TrimSpace(msg.Role) != "user" {
			continue
		}
		text := editorHistoryText(msg)
		if text == "" {
			continue
		}
		a.editor.Model().AddHistory(text)
	}
}

func editorHistoryText(msg sdk.Message) string {
	if strings.TrimSpace(msg.Text) != "" {
		return msg.Text
	}
	if len(msg.Content) == 0 {
		return ""
	}
	var b strings.Builder
	for _, block := range msg.Content {
		if block.Type != ai.ContentBlockText || block.Text == "" {
			continue
		}
		b.WriteString(block.Text)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (a *App) ctxOrBackground() context.Context {
	if a != nil && a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

func (a *App) sessionState() sdk.SessionState {
	if a == nil || a.sess == nil {
		return sdk.SessionState{}
	}
	return a.sess.State()
}

func cmdLogout(app *App, args string) {
	if app == nil {
		return
	}
	provider := strings.TrimSpace(args)
	if provider == "" {
		app.conv.AddError("usage: /logout <provider>")
		app.refreshChat()
		return
	}
	if app.authStore == nil {
		app.conv.AddError("logout: credential store unavailable")
		app.refreshChat()
		return
	}
	if app.loginDialog != nil {
		app.dismissDialog()
	}
	if err := app.authStore.Delete(app.ctxOrBackground(), provider); err != nil {
		app.conv.AddError("logout: " + err.Error())
		app.refreshChat()
		return
	}
	app.conv.AddSystem("Logged out of " + provider)
	app.refreshChat()
}

func cmdLogin(app *App, args string) {
	if app == nil {
		return
	}
	fields := strings.Fields(args)
	if len(fields) == 0 {
		app.startLogin("")
		return
	}
	provider := fields[0]
	apiKey := ""
	if len(fields) > 1 {
		apiKey = strings.Join(fields[1:], " ")
	}
	if app.authStore == nil {
		app.conv.AddError("login: credential store unavailable")
		app.refreshChat()
		return
	}
	if apiKey == "" && !ai.ProviderAllowsNoCredential(provider) {
		app.startLogin(provider)
		return
	}
	if app.loginDialog != nil {
		app.dismissDialog()
	}
	var cred ai.Credential
	if apiKey != "" {
		cred = ai.NewAPIKeyCredential(apiKey)
	} else if ai.ProviderAllowsNoCredential(provider) {
		cred = ai.NewNoneCredential()
	} else {
		app.conv.AddError("usage: /login " + provider + " <api-key>")
		app.refreshChat()
		return
	}
	if err := app.authStore.Set(app.ctxOrBackground(), provider, cred); err != nil {
		app.conv.AddError("login: " + err.Error())
		app.refreshChat()
		return
	}
	if apiKey != "" {
		app.completeProviderLogin(provider, "Logged in to "+provider+" with API key")
	} else {
		app.conv.AddSystem("Configured " + provider + " (no credential required)")
	}
	app.refreshChat()
}

// cmdHotkeys shows the same keybinding hints as /help's "Keybindings"
// section. The standalone /hotkeys command is a shortcut users can
// type when they want the keybinding summary without the rest of the
// help text.
func cmdHotkeys(app *App, _ string) {
	var b strings.Builder
	b.WriteString("Keybindings:\n")
	// Configurable actions are described from the live registry, so a user who
	// rebound something sees their key here rather than the default we shipped.
	for _, hint := range app.keybindingHints() {
		b.WriteString("  " + hint + "\n")
	}
	// Fixed hints. These keys are resolved contextually rather than through the
	// registry (what Enter means depends on whether a picker is open), so they
	// are literal by design — §10.3 allows raw-key hints.
	b.WriteString("  tab          complete command or model\n")
	b.WriteString("  enter        submit (or select+submit when picker is open)\n")
	b.WriteString("  shift-enter  insert newline (alt-enter may be equivalent in some terminals)\n")
	b.WriteString("  esc          dismiss picker or interrupt run\n")
	b.WriteString("  /            slash commands\n")
	app.conv.AddSystem(strings.TrimRight(b.String(), "\n"))
	app.refreshChat()
}

// hotkeyHintOrder fixes the order the configurable actions are listed in.
// Ranging the registry map directly would reshuffle /hotkeys on every call.
//
// It must name every globally-bindable action: an action missing from this list
// is invisible to /hotkeys, which is the only place a user can discover the
// keys. The /scoped-models ids are the deliberate exception — they only apply
// inside that dialog, which advertises its own keys. TestHotkeysListEveryGlobal
// Action guards the invariant.
var hotkeyHintOrder = []string{
	actionIDExit,
	actionIDSuspend,
	actionIDModelSelect,
	actionIDModelCycleForward,
	actionIDModelCycleBack,
	actionIDThinkingCycle,
	actionIDThinkingToggle,
	actionIDPaste,
	actionIDMessageCopy,
	actionIDMessageDequeue,
	actionIDMessageFollowUp,
	actionIDHistorySearchBackward,
	actionIDHistorySearchForward,
	actionIDHeaderExpand,
	actionIDChatScrollBottom,
}

// keybindingHints renders "<keys>  <description>" for each configurable action,
// joining multiple keys with "/" per §10.3. Actions the user has disabled by
// binding them to nothing are skipped: listing a hotkey with no key is worse
// than omitting it.
func (a *App) keybindingHints() []string {
	manager := a.keybindings
	if manager == nil {
		manager = defaultAppKeybindings()
	}
	defs := appKeybindingDefinitions()

	hints := make([]string, 0, len(hotkeyHintOrder))
	for _, id := range hotkeyHintOrder {
		keys := manager.Keys(id)
		if len(keys) == 0 {
			continue
		}
		desc := ""
		if d, ok := defs[id]; ok {
			desc = d.Description
		}
		hints = append(hints, fmt.Sprintf("%-12s %s", strings.Join(keys, "/"), desc))
	}
	return hints
}

// cmdCompact triggers session compaction. The optional argument string is
// passed through as custom summary instructions, matching Pi's
// `/compact <instructions>` behavior.
func cmdCompact(app *App, args string) {
	if app == nil || app.sess == nil {
		return
	}
	instructions := strings.TrimSpace(args)
	app.conv.AddSystem("compact: history compaction requested")
	app.applySessionEvent(sdk.Event{Type: "compaction_start"})
	app.refreshChat()

	ctx := app.ctxOrBackground()
	go func() {
		if err := app.sess.Compact(ctx, instructions); err != nil {
			app.post(func(_ *tauui.UpdateCtx) {
				app.clearStatus(compactionStatusKey)
				app.conv.AddError("compact: " + err.Error())
				app.refreshChat()
			})
		}
	}()
}
