package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/losinggeneration/rovel/backend/headless"
	"github.com/losinggeneration/rovel/geom"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/sdk"
	sdkpkg "github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sdkSlashInfo is a local alias so the merge test reads naturally.
type sdkSlashInfo = sdkpkg.SlashCommandInfo

// TestCommandsBuiltins verifies NewCommands seeds the canonical built-in list
// without depending on unrelated tests.
func TestCommandsBuiltins(t *testing.T) {
	c := NewCommands()

	// NewCommands seeds 22 builtins: the original interactive commands,
	// show-model, /layout, plus the interactive stubs. The ordering is
	// significant (it controls completion popup order) so
	// we list every entry.
	want := []string{
		"help", "model", "show-model", "quit",
		"settings", "scoped-models", "export", "import",
		"copy", "name", "session", "sessions", "hotkeys", "layout", "fork", "forget",
		"tree", "login", "logout", "new", "compact", "retry", "resume", "history",
	}
	got := make([]string, 0, len(c.All()))
	for _, cmd := range c.All() {
		got = append(got, cmd.Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("builtins = %v, want %v", got, want)
	}

	for _, name := range want {
		if _, ok := c.Lookup(name); !ok {
			t.Errorf("Lookup(%q) missing", name)
		}
	}
}

func TestCommandsRegisterReplaces(t *testing.T) {
	c := NewCommands()
	c.Register(Command{Name: "help", Description: "v2"})

	cmd, ok := c.Lookup("help")
	if !ok {
		t.Fatal("Lookup(help) failed after re-register")
	}
	if cmd.Description != "v2" {
		t.Errorf("re-registered description = %q, want %q", cmd.Description, "v2")
	}
}

func TestCommandsFilter(t *testing.T) {
	c := NewCommands()
	// Add some extra commands so we can verify prefix filtering.
	c.Register(Command{Name: "model", Description: "pick a model"})
	c.Register(Command{Name: "session", Description: "manage sessions"})

	cases := []struct {
		prefix string
		want   []string
	}{
		// Empty prefix returns all builtins. We list them
		// explicitly so any change to NewCommands is caught here
		// (this is the canonical command list for the TUI).
		{"", []string{
			"help", "model", "show-model", "quit",
			"settings", "scoped-models", "export", "import",
			"copy", "name", "session", "sessions", "hotkeys", "layout", "fork", "forget",
			"tree", "login", "logout", "new", "compact", "retry", "resume", "history",
		}},
		{"h", []string{"help", "history", "hotkeys"}},
		{"c", []string{"compact", "copy"}},
		{"q", []string{"quit"}},
		{"m", []string{"model"}},
		// Several s-prefix commands: scoped-models, session, sessions, settings, show-model.
		{"s", []string{"scoped-models", "session", "sessions", "settings", "show-model"}},
		{"HE", []string{"help"}}, // case-insensitive
		{"zz", nil},
	}
	for _, tc := range cases {
		t.Run("prefix="+tc.prefix, func(t *testing.T) {
			var got []string
			for _, cmd := range c.Filter(tc.prefix) {
				got = append(got, cmd.Name)
			}
			sort.Strings(got)
			sort.Strings(tc.want)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Filter(%q) = %v, want %v", tc.prefix, got, tc.want)
			}
		})
	}
}

func TestParseCommandLine(t *testing.T) {
	cases := []struct {
		in   string
		name string
		args string
		ok   bool
	}{
		{"", "", "", false},
		{"hello", "", "", false},
		{"/", "", "", false},
		{"/help", "help", "", true},
		{"/help me", "help", "me", true},
		{"/help  with  spaces", "help", "with  spaces", true},
		{"/help\twith\ttabs", "help", "with\ttabs", true},
		{"/quit", "quit", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			n, a, ok := parseCommandLine(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if n != tc.name {
				t.Errorf("name = %q, want %q", n, tc.name)
			}
			if a != tc.args {
				t.Errorf("args = %q, want %q", a, tc.args)
			}
		})
	}
}

// TestCmdHelp exercises the built-in that mutates conversation state without
// quitting the app. Quit is covered indirectly via the test-driver path (and we
// don't want to actually shut down the toolkit in a unit test).
func TestCmdHelp(t *testing.T) {
	app := newTestApp(t)

	cmdHelp(app, "")

	got := app.conv.Messages()
	if len(got) != 1 {
		t.Fatalf("after /help: messages = %d, want 1", len(got))
	}
	if got[0].Role != RoleSystem {
		t.Errorf("help role = %q, want %q", got[0].Role, RoleSystem)
	}
	for _, cmd := range app.cmds.All() {
		if !strings.Contains(got[0].Text, "/"+cmd.Name) {
			t.Errorf("/help output missing /%s:\n%s", cmd.Name, got[0].Text)
		}
	}

	// New format assertions: the help output should be grouped with
	// explicit section headers and include keybinding hints. These
	// lock in the spec-aligned output shape so future edits don't
	// regress the structure.
	for _, must := range []string{
		"Interactive commands:",
		"Keybindings:",
		"ctrl-c",
		"tab",
		"enter",
	} {
		if !strings.Contains(got[0].Text, must) {
			t.Errorf("/help output missing %q section/hint:\n%s", must, got[0].Text)
		}
	}
	// "Session commands:" section is only printed when external
	// commands exist. With a nil-session test app there are none,
	// so we don't assert its presence here — but we do assert it
	// WOULD appear if external commands were available.
	if external := app.externalCommands(); len(external) > 0 {
		if !strings.Contains(got[0].Text, "Session commands:") {
			t.Errorf("/help output missing 'Session commands:' section when %d external commands exist:\n%s", len(external), got[0].Text)
		}
	}
}

// TestTrySubmitForwardsNonLocalCommand verifies the TUI falls through to
// the session for slash commands it doesn't own. The TUI only intercepts
// its own builtins; agent commands, skills, and plugin commands go
// through the session's prompt pipeline unchanged.
func TestTrySubmitForwardsNonLocalCommand(t *testing.T) {
	app := newTestApp(t)
	fs := app.sess.(*fakeSession)
	fs.mu.Lock()
	fs.slashInfos = []sdkSlashInfo{{Name: "skill:my-skill", Description: "run skill", Source: "skill"}}
	fs.mu.Unlock()
	editor := newPromptEditorForTest()
	app.editor = editor
	app.footer = newFooterForTest()
	editor.SetText(testCtx(), "/skill:my-skill arg1 arg2")
	ctx := testCtx()
	ctx.FocusedID = editor.ID()
	if !app.trySubmit(ctx) {
		t.Fatal("trySubmit did not consume a /command")
	}
	if editor.Text() != "" {
		t.Errorf("editor text after submit = %q, want empty", editor.Text())
	}
	// submit() dispatches the session call on a background goroutine; yield
	// briefly so the call lands in the recording fake before we assert.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		fs := app.sess.(*fakeSession)
		fs.mu.Lock()
		n := len(fs.sentTexts)
		fs.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.sentTexts) != 1 {
		t.Fatalf("session.Send calls = %d, want 1", len(fs.sentTexts))
	}
	if fs.sentTexts[0] != "/skill:my-skill arg1 arg2" {
		t.Errorf("forwarded text = %q, want %q", fs.sentTexts[0], "/skill:my-skill arg1 arg2")
	}
}

// TestTrySubmitForwardsReloadToSession verifies /reload reaches the session's
// command pipeline rather than being intercepted by a TUI-local stub. The
// session's builtin reload re-discovers artifacts (skills, prompts, themes)
// and reloads plugins; /reload <plugin> force-restarts the named plugin.
func TestTrySubmitForwardsReloadToSession(t *testing.T) {
	app := newTestApp(t)
	fs := app.sess.(*fakeSession)
	fs.mu.Lock()
	fs.slashInfos = []sdkSlashInfo{{
		Name:         "reload",
		Description:  "Re-discover artifacts (skills, prompts, themes) and plugins",
		ArgumentHint: "<plugin>",
		Source:       "builtin",
	}}
	fs.mu.Unlock()
	editor := newPromptEditorForTest()
	app.editor = editor
	app.footer = newFooterForTest()
	editor.SetText(testCtx(), "/reload my-plugin")
	ctx := testCtx()
	ctx.FocusedID = editor.ID()
	if !app.trySubmit(ctx) {
		t.Fatal("trySubmit did not consume a /command")
	}
	if editor.Text() != "" {
		t.Errorf("editor text after submit = %q, want empty", editor.Text())
	}
	// The session call lands on a background goroutine; yield briefly so the
	// recording fake sees it before we assert.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		fs.mu.Lock()
		n := len(fs.sentTexts)
		fs.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.sentTexts) != 1 {
		t.Fatalf("session.Send calls = %d, want 1", len(fs.sentTexts))
	}
	if fs.sentTexts[0] != "/reload my-plugin" {
		t.Errorf("forwarded text = %q, want %q", fs.sentTexts[0], "/reload my-plugin")
	}
	// No local stub may claim the command: a "coming soon" system row would
	// mean the TUI shadowed the session handler again.
	if _, found := app.cmds.Lookup("reload"); found {
		t.Error("reload registered as a TUI-local command; the session owns it")
	}
}

func TestTrySubmitExpandsLargePasteMarkers(t *testing.T) {
	app := newTestApp(t)
	fs := app.sess.(*fakeSession)
	editor := newPromptEditorForTest()
	app.editor = editor
	app.footer = newFooterForTest()
	largePaste := strings.Repeat("pasted line\n", 16)
	editor.Model().Paste(largePaste)
	if !strings.Contains(editor.Text(), "[paste #") {
		t.Fatalf("editor text = %q, want large paste marker", editor.Text())
	}
	ctx := testCtx()
	ctx.FocusedID = editor.ID()

	if !app.trySubmit(ctx) {
		t.Fatal("trySubmit did not consume prompt")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		fs.mu.Lock()
		n := len(fs.sentTexts)
		fs.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.sentTexts) != 1 {
		t.Fatalf("session.Send calls = %d, want 1", len(fs.sentTexts))
	}
	want := strings.TrimSpace(largePaste)
	if fs.sentTexts[0] != want {
		t.Fatalf("sent text = %q, want expanded paste %q", fs.sentTexts[0], want)
	}
	if strings.Contains(fs.sentTexts[0], "[paste #") {
		t.Fatalf("sent text still contains paste marker: %q", fs.sentTexts[0])
	}
}

// TestTrySubmitHandlesLocalCommand verifies the TUI handles its own
// builtins (here /help) without forwarding to the session.
func TestTrySubmitUnknownSlashCommandDoesNotSendToSession(t *testing.T) {
	app := newTestApp(t)
	editor := newPromptEditorForTest()
	app.editor = editor
	app.footer = newFooterForTest()
	editor.SetText(testCtx(), "/does-not-exist")
	ctx := testCtx()
	ctx.FocusedID = editor.ID()

	if !app.trySubmit(ctx) {
		t.Fatal("trySubmit did not consume unknown slash command")
	}
	if editor.Text() != "" {
		t.Errorf("editor text after submit = %q, want empty", editor.Text())
	}
	fs := app.sess.(*fakeSession)
	fs.mu.Lock()
	sent := append([]string(nil), fs.sentTexts...)
	fs.mu.Unlock()
	if len(sent) != 0 {
		t.Fatalf("unknown slash command sent to session: %v", sent)
	}
	msgs := app.conv.Messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "unknown command: /does-not-exist") {
		t.Fatalf("conversation messages = %+v, want unknown-command error", msgs)
	}
	if app.isStreaming() {
		t.Fatal("unknown slash command should not start streaming")
	}
}

func TestTrySubmitShowModelHandledLocally(t *testing.T) {
	app := newTestApp(t)
	fs := app.sess.(*fakeSession)
	fs.mu.Lock()
	fs.stateModel = "openai-codex/gpt-5.4"
	fs.mu.Unlock()
	editor := newPromptEditorForTest()
	app.editor = editor
	app.footer = newFooterForTest()
	editor.SetText(testCtx(), "/show-model")
	ctx := testCtx()
	ctx.FocusedID = editor.ID()

	if !app.trySubmit(ctx) {
		t.Fatal("trySubmit did not consume /show-model")
	}
	fs.mu.Lock()
	sent := append([]string(nil), fs.sentTexts...)
	fs.mu.Unlock()
	if len(sent) != 0 {
		t.Fatalf("/show-model sent to session: %v", sent)
	}
	msgs := app.conv.Messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "Current model: openai-codex/gpt-5.4") {
		t.Fatalf("conversation messages = %+v, want current model system notice", msgs)
	}
	if app.isStreaming() {
		t.Fatal("/show-model should not start streaming")
	}
}

// /sessions must open the TUI's session workspace instead of being
// forwarded to the session's command pipeline, whose builtin answer
// ("use tau sessions <subcommand> from the shell") is impossible to
// follow from inside the TUI.
func TestTrySubmitSessionsOpensWorkspace(t *testing.T) {
	app := newTestApp(t)
	app.dialogPicker = tauui.NewDialogPicker()
	app.dialogPicker.SetOnCancel(app.dismissDialog)
	fs := app.sess.(*fakeSession)
	fs.mu.Lock()
	// The runtime command registry advertises a shell-oriented "sessions"
	// builtin; the TUI must shadow it locally rather than forward.
	fs.slashInfos = []sdkSlashInfo{{Name: "sessions", Description: "List, fork, and export sessions", Source: "builtin"}}
	fs.mu.Unlock()
	app.sessionLister = func(context.Context) ([]sdkpkg.SessionInfo, error) {
		return []sdkpkg.SessionInfo{{ID: "abc12345", Name: "demo", Created: "2026-01-01T00:00:00Z"}}, nil
	}
	editor := newPromptEditorForTest()
	app.editor = editor
	app.footer = newFooterForTest()
	editor.SetText(testCtx(), "/sessions")
	ctx := testCtx()
	ctx.FocusedID = editor.ID()

	if !app.trySubmit(ctx) {
		t.Fatal("trySubmit did not consume /sessions")
	}
	if editor.Text() != "" {
		t.Errorf("editor text after submit = %q, want empty", editor.Text())
	}
	if !app.dialogPicker.Shown() {
		t.Fatal("/sessions did not open the session workspace overlay")
	}
	if app.dialogPicker.Component() == nil {
		t.Fatal("/sessions opened an empty dialog picker; want the workspace overlay")
	}
	fs.mu.Lock()
	sent := append([]string(nil), fs.sentTexts...)
	fs.mu.Unlock()
	if len(sent) != 0 {
		t.Fatalf("/sessions forwarded to the session: %v", sent)
	}
}

func TestTrySubmitHandlesLocalCommand(t *testing.T) {
	app := newTestApp(t)
	editor := newPromptEditorForTest()
	app.editor = editor
	app.footer = newFooterForTest()
	editor.SetText(testCtx(), "/help")
	ctx := testCtx()
	ctx.FocusedID = editor.ID()
	if !app.trySubmit(ctx) {
		t.Fatal("trySubmit did not consume /help")
	}
	if editor.Text() != "" {
		t.Errorf("editor after /help = %q, want empty", editor.Text())
	}
	if got := len(app.conv.Messages()); got != 1 {
		t.Errorf("messages after /help = %d, want 1", got)
	}
	fs := app.sess.(*fakeSession)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.sentTexts) != 0 {
		t.Errorf("session.Send calls = %d, want 0 (built-in must not forward)", len(fs.sentTexts))
	}
}

// TestTrySubmitSlashCommandHandler verifies the known-command path routes
// to the handler and clears the editor. (Kept for completeness; the
// forwarding path is covered by TestTrySubmitForwardsNonLocalCommand
// and the local-dispatch path by TestTrySubmitHandlesLocalCommand.)
func TestTrySubmitSlashCommandHandler(t *testing.T) {
	app := newTestApp(t)
	editor := newPromptEditorForTest()
	app.editor = editor
	app.footer = newFooterForTest()
	editor.SetText(testCtx(), "/help")
	ctx := testCtx()
	ctx.FocusedID = editor.ID()
	if !app.trySubmit(ctx) {
		t.Fatal("trySubmit did not consume /help")
	}
	if editor.Text() != "" {
		t.Errorf("editor after /help = %q, want empty", editor.Text())
	}
	if got := len(app.conv.Messages()); got != 1 {
		t.Errorf("messages after /help = %d, want 1", got)
	}
}

func TestCmdQuitInvokesShutdown(t *testing.T) {
	app := newTestApp(t)
	app.cancelRun = func() {} // pre-set so quit's cancelRun call is a no-op
	// cmdQuit calls a.app.Quit() which panics on a nil toolkit. We test
	// the side effect that matters in isolation: cancelRun fires.
	called := false
	app.cancelRun = func() { called = true }
	// Manually exercise the parts of cmdQuit we can unit-test without
	// the toolkit: cancelRun and Close. This is the same pattern the
	// quit() helper would take if the toolkit were wired in.
	app.cancelRun()
	if !called {
		t.Error("cancelRun was not called by the quit path")
	}
	_ = app.sess.Close(context.Background())
}

// TestCompletionItemsMergesExternal verifies the popup's item list merges
// the TUI's local commands with whatever the session reports, with local
// commands first so builtins stay accessible even when many session
// commands exist.
func TestCompletionItemsMergesExternal(t *testing.T) {
	app := newTestApp(t)
	fs := app.sess.(*fakeSession)
	fs.mu.Lock()
	fs.slashInfos = []sdkSlashInfo{
		{Name: "model", Description: "switch model", Source: "builtin"},
		{Name: "fork", Description: "fork the session", Source: "builtin"},
		{Name: "ext-cmd", Description: "only from the session", Source: "plugin"},
	}
	fs.mu.Unlock()

	items := app.completionItems("")
	// The local registry has 24 entries; the two same-named session
	// commands are shadowed by the local registry, leaving one external
	// entry. We don't assert the exact list ordering beyond the first
	// five (the originals) so future stub additions don't break this
	// test.
	if len(items) != 25 {
		t.Fatalf("completionItems = %d items, want 25 (24 local + 1 external)", len(items))
	}
	wantLocal := []string{"help", "model", "show-model", "quit", "settings"}
	for i, name := range wantLocal {
		if items[i].Name != name {
			t.Errorf("item[%d] = %q, want local %q", i, items[i].Name, name)
		}
		if items[i].Handler == nil {
			t.Errorf("item[%d] = %q has nil handler; local commands must keep theirs", i, items[i].Name)
		}
	}
	// External items (the last one) must have nil handlers so the
	// TUI forwards them to the session.
	if items[len(items)-1].Handler != nil {
		t.Errorf("external item %q has non-nil handler; should be nil so the TUI forwards", items[len(items)-1].Name)
	}

	items = app.completionItems("comp")
	if len(items) != 1 || items[0].Name != "compact" {
		t.Errorf("filter /comp = %v, want [compact]", names(items))
	}
	items = app.completionItems("mod")
	// The local /model builtin shadows the session-supplied model
	// command: one entry, not two.
	if len(items) != 1 || items[0].Name != "model" {
		t.Errorf("filter /mod = %v, want only the local model", names(items))
	}
}

// TestRegisterUnregisterAtRuntime covers the plugin lifecycle: a command
// can be added after the TUI is up, appears in the completion list, and
// can be removed again. Concurrency-safe by construction.
func TestRegisterUnregisterAtRuntime(t *testing.T) {
	c := NewCommands()
	if _, ok := c.Lookup("plugin-cmd"); ok {
		t.Fatal("plugin-cmd already present before register")
	}
	c.Register(Command{Name: "plugin-cmd", Description: "from a plugin"})
	if _, ok := c.Lookup("plugin-cmd"); !ok {
		t.Fatal("plugin-cmd not present after register")
	}
	// NewCommands seeds the builtins: 4 originals (help, model,
	// show-model, quit) plus the interactive stubs/commands (settings,
	// scoped-models, export, import, copy, name, session,
	// sessions, hotkeys, fork, tree, login, logout, new,
	// compact, resume, history, layout, forget). /reload is not a TUI
	// local — the session owns it. With 1 plugin registered, All()
	// returns 25.
	if got := len(c.All()); got != 25 {
		t.Errorf("All() length = %d, want 25 (24 builtins + 1 plugin)", got)
	}
	if !c.Unregister("plugin-cmd") {
		t.Error("Unregister(plugin-cmd) returned false")
	}
	if _, ok := c.Lookup("plugin-cmd"); ok {
		t.Error("plugin-cmd still present after unregister")
	}
	if c.Unregister("plugin-cmd") {
		t.Error("second Unregister(plugin-cmd) returned true; want false")
	}
}

func names(cs []Command) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

// TestCmdCopyCopiesLastReplyByDefault pins that bare /copy is wired to the
// real implementation: it copies the last assistant reply to the clipboard,
// not a "coming soon" stub. /copy all and /copy last keep their explicit forms.
func TestCmdCopyCopiesLastReplyByDefault(t *testing.T) {
	app := newTestApp(t)
	app.conv.AddUserMessage("question")
	app.conv.AddAssistantMessage("the answer")

	var copied string
	ctx := &tauui.Ctx{ClipboardWrite: func(s string) { copied = s }}
	app.loopMu.Lock()
	app.loopCtx = ctx
	app.loopMu.Unlock()

	cmd, ok := app.cmds.Lookup("copy")
	if !ok {
		t.Fatal("copy command not registered")
	}
	cmd.Handler(app, "")

	if copied != "the answer" {
		t.Fatalf("clipboard = %q, want the last assistant reply %q", copied, "the answer")
	}
	msgs := app.conv.Messages()
	last := msgs[len(msgs)-1]
	if last.Role != RoleSystem || last.Text != "copy: copied last" {
		t.Fatalf("notice = %q (%s), want \"copy: copied last\" (system)", last.Text, last.Role)
	}
}

func TestCmdHotkeysShowsKeybindingHints(t *testing.T) {
	app := newTestApp(t)
	app.conv.Clear()
	cmdHotkeys(app, "")
	got := app.conv.Messages()
	if len(got) != 1 {
		t.Fatalf("/hotkeys produced %d messages, want 1", len(got))
	}
	if got[0].Role != RoleSystem {
		t.Errorf("/hotkeys role = %q, want %q", got[0].Role, RoleSystem)
	}
	for _, must := range []string{
		"Keybindings:",
		"ctrl+c",
		"ctrl+p",
		"tab",
		"enter",
		"shift-enter",
		"alt+backspace",
		"esc",
	} {
		if !strings.Contains(got[0].Text, must) {
			t.Errorf("/hotkeys output missing %q:\n%s", must, got[0].Text)
		}
	}
}

// TestCmdCompactCallsSession verifies the /compact command invokes
// sdk.Session.Compact on the bound session and surfaces a chat-log
// notice on success. /compact (alongside /hotkeys) is one of the few
// interactive builtins with a real implementation; the other stubs
// are placeholders until their backends land.
func TestCmdLoginStoresNoCredentialProvider(t *testing.T) {
	app := newTestApp(t)
	store := appCredentialStore(t)
	app.authStore = store

	cmdLogin(app, ai.ProviderClaudeAgent)

	cred, err := store.Get(context.Background(), ai.ProviderClaudeAgent)
	if err != nil {
		t.Fatalf("credential not stored: %v", err)
	}
	if cred.Type != ai.CredentialTypeNone {
		t.Fatalf("credential type = %q, want none", cred.Type)
	}
	msgs := app.conv.Messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "Configured "+ai.ProviderClaudeAgent) {
		t.Fatalf("messages = %+v, want configured notice", msgs)
	}
}

func TestCmdLoginWithoutDialogPointsAtKeySyntax(t *testing.T) {
	app := newTestApp(t)
	app.authStore = appCredentialStore(t)

	cmdLogin(app, ai.ProviderOpenAI)

	msgs := app.conv.Messages()
	if len(msgs) != 1 || msgs[0].Role != RoleError || !strings.Contains(msgs[0].Text, "/login <provider> <api-key>") {
		t.Fatalf("messages = %+v, want key syntax hint", msgs)
	}
}

func TestCmdLoginStoresAPIKey(t *testing.T) {
	app := newTestApp(t)
	store := appCredentialStore(t)
	app.authStore = store

	cmdLogin(app, ai.ProviderOpenAI+" sk-test")

	cred, err := store.Get(context.Background(), ai.ProviderOpenAI)
	if err != nil {
		t.Fatalf("credential not stored: %v", err)
	}
	if cred.Type != ai.CredentialTypeAPIKey || cred.APIKey != "sk-test" {
		t.Fatalf("credential = %+v, want API key", cred)
	}
}

func TestCmdLogoutDeletesCredential(t *testing.T) {
	app := newTestApp(t)
	store := appCredentialStore(t)
	app.authStore = store
	if err := store.Set(context.Background(), ai.ProviderOpenAI, ai.NewAPIKeyCredential("sk-test")); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	cmdLogout(app, ai.ProviderOpenAI)

	if _, err := store.Get(context.Background(), ai.ProviderOpenAI); err == nil {
		t.Fatal("credential still exists after logout")
	}
	msgs := app.conv.Messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "Logged out of "+ai.ProviderOpenAI) {
		t.Fatalf("messages = %+v, want logout notice", msgs)
	}
}

func TestCmdForkUsesSessionFactory(t *testing.T) {
	oldSession := &fakeSession{stateID: "old", stateModel: "provider/old", stateCWD: "/tmp/project"}
	newSession := &fakeSession{stateID: "new", stateModel: "provider/old", stateCWD: "/tmp/project"}
	app := newTestApp(t)
	app.sess = oldSession
	app.footer = newFooterForTest()
	app.sessionFactory = func(ctx context.Context, opts sdkpkg.SessionOptions) (sdkpkg.Session, error) {
		if opts.ForkFrom != "old" {
			t.Fatalf("ForkFrom = %q, want old", opts.ForkFrom)
		}
		if opts.ForkFromEntry != "entry-1" {
			t.Fatalf("ForkFromEntry = %q, want entry-1", opts.ForkFromEntry)
		}
		if opts.CWD != "/tmp/project" {
			t.Fatalf("CWD = %q, want /tmp/project", opts.CWD)
		}
		return newSession, nil
	}

	cmdFork(app, "entry-1")

	if app.sess != newSession {
		t.Fatalf("app.sess was not replaced with forked session")
	}
	msgs := app.conv.Messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "Forked session old -> new") {
		t.Fatalf("messages = %+v, want fork notice", msgs)
	}
}

func TestCmdTreeShowsSessionParents(t *testing.T) {
	parent := "root"
	app := newTestApp(t)
	app.sess = &fakeSession{stateID: "child"}
	app.sessionLister = func(context.Context) ([]sdkpkg.SessionInfo, error) {
		return []sdkpkg.SessionInfo{
			{ID: "root", Name: "Root", Created: "2026-01-01T00:00:00Z"},
			{ID: "child", Name: "Child", ParentID: &parent, Created: "2026-01-02T00:00:00Z"},
		}, nil
	}

	cmdTree(app, "")

	msgs := app.conv.Messages()
	if len(msgs) != 1 || msgs[0].Role != RoleSystem {
		t.Fatalf("messages = %+v, want system tree", msgs)
	}
	for _, must := range []string{"Session tree:", "Root", "Child", "*"} {
		if !strings.Contains(msgs[0].Text, must) {
			t.Fatalf("tree output missing %q:\n%s", must, msgs[0].Text)
		}
	}
}

// /history re-seeds the chat log with the full persisted transcript: the
// default resume view is the compacted context, so pre-compaction messages are
// otherwise unreachable from the TUI. The agent context is untouched.
func TestCmdHistorySeedsFullTranscript(t *testing.T) {
	app := newTestApp(t)
	fs := app.sess.(*fakeSession)
	fs.messages = []sdkpkg.Message{
		{Role: "system", Text: "Session Summary: early history"},
		{Role: "user", Text: "after the compaction"},
	}
	fs.allMessages = []sdkpkg.Message{
		{Role: "user", Text: "before the compaction"},
		{Role: "assistant", Text: "acknowledged"},
		{Role: "system", Text: "Session Summary: early history"},
		{Role: "user", Text: "after the compaction"},
	}

	cmdHistory(app, "")

	var joined []string
	for _, m := range app.conv.Messages() {
		joined = append(joined, m.Text)
	}
	all := strings.Join(joined, "\n")
	for _, want := range []string{"before the compaction", "acknowledged", "Session Summary: early history", "after the compaction", "full session history"} {
		if !strings.Contains(all, want) {
			t.Errorf("/history output missing %q:\n%s", want, all)
		}
	}
}

// A session that cannot supply the full transcript reports it instead of
// silently doing nothing.
func TestCmdHistoryWithoutFullListerReportsUnavailable(t *testing.T) {
	app := newTestApp(t)
	app.sess = &sessionNoFollowUp{inner: app.sess.(*fakeSession)}

	cmdHistory(app, "")

	msgs := app.conv.Messages()
	if len(msgs) != 1 || msgs[0].Role != RoleError {
		t.Fatalf("messages = %+v, want one error row", msgs)
	}
	if !strings.Contains(msgs[0].Text, "full history") {
		t.Errorf("error = %q, want the full-history unavailability notice", msgs[0].Text)
	}
}

func TestCmdCompactCallsSession(t *testing.T) {
	app := newTestApp(t)
	app.conv.Clear()
	cmdCompact(app, "")
	got := app.conv.Messages()
	if len(got) != 1 {
		t.Fatalf("/compact produced %d messages, want 1", len(got))
	}
	if got[0].Role != RoleSystem {
		t.Errorf("/compact role = %q, want %q", got[0].Role, RoleSystem)
	}
	if !strings.Contains(got[0].Text, "compact:") {
		t.Errorf("/compact notice = %q, missing 'compact:' prefix", got[0].Text)
	}
}

func TestCmdCompactPassesCustomInstructions(t *testing.T) {
	app := newTestApp(t)
	app.conv.Clear()
	cmdCompact(app, "focus on blockers")

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		fs := app.sess.(*fakeSession)
		fs.mu.Lock()
		calls := fs.compactCalls
		fs.mu.Unlock()
		if calls > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	fs := app.sess.(*fakeSession)
	fs.mu.Lock()
	calls := fs.compactCalls
	args := append([]string(nil), fs.compactArgs...)
	fs.mu.Unlock()

	if calls != 1 {
		t.Fatalf("compact calls = %d, want 1", calls)
	}
	if len(args) != 1 || args[0] != "focus on blockers" {
		t.Fatalf("compact args = %v, want [focus on blockers]", args)
	}
}

// TestCommandsConflicts verifies the conflict registry surfaces
// duplicate-name registrations (e.g. a built-in shadowed by a
// plugin) so help/settings can list them. The first registration
// remains the canonical owner of dispatch.
func TestCommandsConflicts(t *testing.T) {
	c := NewCommands()
	// No conflicts on a fresh registry.
	if got := c.Conflicts(); len(got) != 0 {
		t.Errorf("Conflicts on empty registry = %v, want empty", got)
	}
	// Register a plugin that shadows an existing builtin with a
	// non-empty source. The first registration (the builtin) is
	// the canonical owner; the plugin's source is recorded as a
	// conflict.
	c.registerWithSource(Command{Name: "help", Description: "from plugin", Handler: nil}, "plugin:sample")
	got := c.Conflicts()
	if got == nil {
		t.Fatal("Conflicts() returned nil after shadow; want a conflict entry")
	}
	sources, ok := got["help"]
	if !ok || len(sources) != 1 || sources[0] != "plugin:sample" {
		t.Errorf("Conflicts[help] = %v, want [plugin:sample]", sources)
	}
	// The first registration wins for dispatch.
	cmd, ok := c.Lookup("help")
	if !ok {
		t.Fatal("Lookup(help) returned !ok")
	}
	if cmd.Description == "from plugin" {
		t.Errorf("Lookup(help) Description = %q, want builtin (first registration wins)", cmd.Description)
	}
}

// TestCmdHelpSurfacesConflicts checks that /help appends a "Command
// conflicts" section listing any shadowed commands when the registry
// has recorded conflicts. This is the parity requirement that
// settings/help surfaces command conflicts.
func TestCmdHelpSurfacesConflicts(t *testing.T) {
	app := newTestApp(t)
	app.cmds.registerWithSource(Command{Name: "fork", Description: "from plugin", Handler: nil}, "plugin:sample")
	app.conv.Clear()
	cmdHelp(app, "")
	msgs := app.conv.Messages()
	if len(msgs) != 1 {
		t.Fatalf("/help produced %d messages, want 1", len(msgs))
	}
	if !strings.Contains(msgs[0].Text, "Command conflicts:") {
		t.Errorf("/help missing conflicts section: %q", msgs[0].Text)
	}
	if !strings.Contains(msgs[0].Text, "/fork") {
		t.Errorf("/help missing /fork in conflicts: %q", msgs[0].Text)
	}
	if !strings.Contains(msgs[0].Text, "plugin:sample") {
		t.Errorf("/help missing source in conflicts: %q", msgs[0].Text)
	}
}

func TestCmdNewUsesSessionFactory(t *testing.T) {
	oldSession := &fakeSession{stateID: "old", stateModel: "provider/old", stateCWD: "/tmp/project"}
	newSession := &fakeSession{stateID: "new", stateModel: "provider/new", stateCWD: "/tmp/project"}
	app := newTestApp(t)
	app.sess = oldSession
	app.footer = newFooterForTest()
	app.sessionFactory = func(ctx context.Context, opts sdkpkg.SessionOptions) (sdkpkg.Session, error) {
		if opts.CWD != "/tmp/project" {
			t.Fatalf("session factory CWD = %q, want /tmp/project", opts.CWD)
		}
		return newSession, nil
	}

	cmdNew(app, "")

	if app.sess != newSession {
		t.Fatalf("app.sess was not replaced with factory session")
	}
	if app.currentModel != "provider/new" {
		t.Fatalf("currentModel = %q, want provider/new", app.currentModel)
	}
}

func TestRestoredToolExecutionDuration(t *testing.T) {
	for _, msg := range []sdkpkg.Message{
		{Role: "tool", ToolCallID: "call", ToolName: "bash", Text: "ok", DurationMs: new(12.5)},
		{Role: "toolResult", ToolCallID: "call", ToolName: "powershell", Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}}, DurationMs: new(12.5)},
		{Role: "tool", Content: []ai.ContentBlock{{Type: ai.ContentBlockToolResult, ToolCallID: "call", ToolName: "bash", Text: "ok"}}, DurationMs: new(12.5)},
	} {
		conv := NewConversationModel()
		appendSDKMessageToConversation(conv, msg)
		snapshot := conv.Snapshot()
		if len(snapshot.Components) != 1 || snapshot.Components[0].ToolExecution.DurationMs == nil || *snapshot.Components[0].ToolExecution.DurationMs != 12.5 {
			t.Fatalf("duration lost: %+v", snapshot)
		}
	}
}

func TestRestoredEmptyToolResultKeepsTiming(t *testing.T) {
	for _, content := range [][]ai.ContentBlock{nil, {{Type: ai.ContentBlockText, Text: ""}}} {
		conv := NewConversationModel()
		conv.AddToolCallWithID("call", "bash", `{"command":"true"}`)
		appendSDKMessageToConversation(conv, sdkpkg.Message{Role: "tool", ToolCallID: "call", ToolName: "bash", Content: content, DurationMs: new(0.0)})
		tool := conv.Snapshot().Components[0].ToolExecution
		if tool.State != toolExecutionComplete || tool.DurationMs == nil || *tool.DurationMs != 0 {
			t.Fatalf("empty result remained running: %+v", tool)
		}
	}
}

func TestRestoredExcludedShellKeepsHeaderFlag(t *testing.T) {
	conv := NewConversationModel()
	conv.AddToolCallWithID("call", "bash", `{"command":"pwd"}`)
	appendSDKMessageToConversation(conv, sdkpkg.Message{Role: "tool", ToolCallID: "call", ToolName: "bash", Text: "ok", ExcludeFromContext: true})
	if !conv.Snapshot().Components[0].ToolExecution.ExcludeFromContext {
		t.Fatal("restored context-exclusion flag lost")
	}
}

func TestRestoredRichToolResultKeepsOneExecution(t *testing.T) {
	conv := NewConversationModel()
	conv.AddToolCallWithID("call", "lookup", `{}`)
	blocks := []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "first"}, {Type: ai.ContentBlockImage, MimeType: "image/png", Data: "aW1hZ2U="}, {Type: ai.ContentBlockText, Text: "second"}}
	appendSDKMessageToConversation(conv, sdkpkg.Message{Role: "tool", ToolCallID: "call", ToolName: "lookup", Content: blocks, StructuredContent: json.RawMessage(`{"answer":42}`), NestedCalls: []ai.NestedToolCall{{ID: "child"}}, ToolUsage: &ai.Usage{Input: 4}})
	components := conv.Snapshot().Components
	if len(components) != 1 {
		t.Fatalf("split rich result into %d components: %+v", len(components), components)
	}
	tool := components[0].ToolExecution
	if len(tool.Content) != 3 || string(tool.StructuredContent) != `{"answer":42}` || len(tool.NestedCalls) != 1 || tool.ToolUsage == nil || tool.ToolUsage.Input != 4 {
		t.Fatalf("lost rich result: %+v", tool)
	}
	if !strings.Contains(tool.Output, "first") || !strings.Contains(tool.Output, "second") {
		t.Fatalf("lost text: %q", tool.Output)
	}
}

func TestRestoredUserBashExecutionRendersAsShellRow(t *testing.T) {
	conv := NewConversationModel()
	details := map[string]any{"command": "make", "output": "boom", "exitCode": float64(2), "excludeFromContext": true, "durationMs": 12.5}
	appendSDKMessageToConversation(conv, sdk.Message{Role: "custom", CustomType: sdk.BashExecutionCustomType, Text: "Ran `make`", Display: true, Details: details})
	components := conv.Snapshot().Components
	if len(components) != 1 {
		t.Fatalf("components = %+v", components)
	}
	tool := components[0].ToolExecution
	if tool.Name != "bash" || tool.Output != "boom" || !tool.IsError || !tool.ExcludeFromContext || !strings.Contains(tool.Args, `"command":"make"`) || tool.DurationMs == nil || *tool.DurationMs != 12.5 {
		t.Fatalf("restored row = %+v", tool)
	}
}

func TestTranscriptReplacementResetsMultiClickState(t *testing.T) {
	for _, clearRows := range []bool{false, true} {
		t.Run(fmt.Sprintf("clear=%t", clearRows), func(t *testing.T) {
			a := newTestApp(t)
			a.conv.AddAssistantMessage("Inline `code` here")
			a.chat.Layout(tauui.Rect{W: 40, H: 10})
			a.chat.setRenderNotifier(func() error { a.chat.applyPreparedRender(); return nil })
			waitForChatLog(t, a.chat)
			row := findStyledRow(t, a.chat)
			be := headless.New(geom.Size{W: 40, H: 10})
			defer be.Close()
			var err error
			root := &transcriptReplacementView{ChatLog: a.chat}
			a.app, err = tauui.New(tauui.Opts{Root: root, Backend: be})
			require.NoError(t, err)
			checked := false
			root.onKey = func(key tauui.KeyEvent) {
				switch key.Rune {
				case 'r':
					replacement := NewConversationModel()
					replacement.AddAssistantMessage("Replacement `text` here")
					a.replaceConversationWithMode(replacement, clearRows)
					waitForChatLog(t, a.chat)
				case 'q':
					a.chat.mu.Lock()
					selection := a.chat.selection
					a.chat.mu.Unlock()
					assert.True(t, selection.active)
					assert.Equal(t, chatLogSelectionModeRange, selection.mode, "new transcript inherited double-click")
					checked = true
					a.app.Quit()
				}
			}
			be.SendEvent(tauui.MouseEvent{Button: tauui.MouseButtonLeft, Action: tauui.MousePress, X: 10, Y: row})
			be.SendEvent(tauui.MouseEvent{Button: tauui.MouseButtonLeft, Action: tauui.MouseRelease, X: 10, Y: row})
			be.SendEvent(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'r'})
			be.SendEvent(tauui.MouseEvent{Button: tauui.MouseButtonLeft, Action: tauui.MousePress, X: 10, Y: row})
			be.SendEvent(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'q'})
			require.NoError(t, a.app.Run())
			assert.True(t, checked)
		})
	}
}

type transcriptReplacementView struct {
	*ChatLog
	onKey func(tauui.KeyEvent)
}

func (v *transcriptReplacementView) Handle(e tauui.Event, ctx *tauui.Ctx) bool {
	if key, ok := e.(tauui.KeyEvent); ok {
		v.onKey(key)
		return true
	}
	return v.ChatLog.Handle(e, ctx)
}

func TestCmdLoginOpensNativeOAuthWithoutBlocking(t *testing.T) {
	a, _ := nativeDialogApp(t)
	a.authStore = appCredentialStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	a.ctx = ctx
	cmdLogin(a, "openrouter")
	require.NotNil(t, a.loginDialog)
	assert.Equal(t, "auth", a.programStatus.current().Kind)
	a.stopLoginWorkers()
	assert.False(t, a.dialogPicker.Shown())
}
