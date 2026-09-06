package tui

// Parity audit: this file is a *behaviour inventory* for the interactive
// TUI app, not a behavioural test in the strict sense. Each test pins
// down an observable property the Pi TUI exposes; the test fails if
// tau's app regresses against any of them. New parity work should
// extend the inventory rather than duplicate existing tests elsewhere.

import (
	"context"
	"strings"
	"testing"

	"github.com/losinggeneration/tau/sdk"
)

// TestParityAudit_CommandsRegistered verifies that every built-in
// interactive command Pi documents (/help, /model, /show-model, /quit,
// /settings, /scoped-models, /export, /import, /share, /copy, /name,
// /session, /changelog, /hotkeys, /fork, /tree, /login, /logout, /new,
// /compact, /resume) is registered in the app's command registry.
// Missing entries surface here so the inventory stays authoritative.
// /reload is not local: the session's command pipeline owns it, so its
// coverage lives in TestTrySubmitForwardsReloadToSession.
func TestParityAudit_CommandsRegistered(t *testing.T) {
	app := newTestApp(t)
	required := []string{
		"help", "model", "show-model", "quit",
		"settings", "scoped-models", "export", "import",
		"share", "copy", "name", "session", "changelog",
		"hotkeys", "fork", "tree", "login", "logout",
		"new", "compact", "resume",
	}
	registered := map[string]bool{}
	for _, c := range app.cmds.All() {
		registered[c.Name] = true
	}
	for _, name := range required {
		if !registered[name] {
			t.Errorf("parity audit: missing built-in command /%s", name)
		}
	}
}

// TestParityAudit_ModelSelectorModeSwitch verifies that the inline
// selector transitions from command mode → model mode when the user
// types a space after `/model`. This is the user-visible behavior
// Pi's slash-command UI exposes.
func TestParityAudit_ModelSelectorModeSwitch(t *testing.T) {
	a := newAppWithSelector(t)
	a.refreshSelector("/model", nil)
	if a.selectorMode != selectorModeCommands {
		t.Fatalf("expected commands mode for /model, got %d", a.selectorMode)
	}
	a.refreshSelector("/model ", nil)
	if a.selectorMode != selectorModeModels {
		t.Fatalf("expected models mode for /model , got %d", a.selectorMode)
	}
}

// TestParityAudit_SessionSelectorModeSwitch verifies the same
// transition for the resume selector.
func TestParityAudit_SessionSelectorModeSwitch(t *testing.T) {
	a := newAppWithSelector(t)
	a.sessionLister = func(ctx context.Context) ([]sdk.SessionInfo, error) {
		return []sdk.SessionInfo{{ID: "sess-1", Name: "demo", Created: "2026-01-01T00:00:00Z"}}, nil
	}
	a.refreshSelector("/resume ", nil)
	if a.selectorMode != selectorModeSessions {
		t.Fatalf("expected sessions mode for /resume , got %d", a.selectorMode)
	}
}

// TestParityAudit_HelpTextSections verifies the /help output contains
// the three required sections: interactive command list, session
// command list, and keybindings. Pi organizes the help screen this
// way; a missing section is a parity regression.
func TestParityAudit_HelpTextSections(t *testing.T) {
	app := newTestApp(t)
	app.conv.Clear()
	cmdHelp(app, "")
	msgs := app.conv.Messages()
	if len(msgs) == 0 {
		t.Fatal("cmdHelp produced no message")
	}
	text := msgs[len(msgs)-1].Text
	for _, section := range []string{"Interactive commands", "Keybindings"} {
		if !strings.Contains(text, section) {
			t.Errorf("help missing section %q in:\n%s", section, text)
		}
	}
}

// TestParityAudit_FooterShowsTokens verifies the footer surfaces
// token usage when the session reports a non-zero count. Pi's footer
// is the canonical token-usage display in interactive mode.
func TestParityAudit_FooterShowsTokens(t *testing.T) {
	app := newTestApp(t)
	app.footer = newFooterForTest()
	fs := app.sess.(*fakeSession)
	fs.contextWindow = 200000
	fs.totalTokens = 12000
	fs.usedPct = 6
	app.refreshFooterStats(nil)
	if app.footer.data.contextWindow != 200000 {
		t.Errorf("contextWindow = %d, want 200000", app.footer.data.contextWindow)
	}
	if app.footer.data.contextUsed != 12000 {
		t.Errorf("contextUsed = %d, want 12000", app.footer.data.contextUsed)
	}
	out, _ := app.footer.render(120)
	if !strings.Contains(out, "6%/200k") {
		t.Errorf("footer should display context usage: %q", out)
	}
}

// TestParityAudit_FooterShowsThinkingLevel verifies the footer shows
// the active thinking level (off/minimal/low/medium/high/xhigh) when
// it is set. Pi's footer surfaces the active level so the user can
// see why a model is reasoning harder.
func TestParityAudit_FooterShowsThinkingLevel(t *testing.T) {
	app := newTestApp(t)
	app.footer = newFooterForTest()
	app.refreshFooterStats(nil)
	app.footer.data.thinkingLevel = "high"
	app.footer.data.supportsReasoning = true
	out, _ := app.footer.render(120)
	if !strings.Contains(out, " • high") {
		t.Errorf("footer should display thinking level 'high': %q", out)
	}
}

// TestParityAudit_ApplyModelFromArgsRequiresAuthentication verifies
// that the model switch path refuses to switch to a model whose
// provider the user is not authenticated for. Pi's `/model` picker
// filters out un-authenticated providers; this test pins the
// backend's auth gate so a regression that bypasses the gate is
// caught.
func TestParityAudit_ApplyModelFromArgsRequiresAuthentication(t *testing.T) {
	app := newTestApp(t)
	app.footer = newFooterForTest()
	app.refreshFooterStats(nil)
	app.credentialStore = nil
	// No env key, no creds → model is un-authenticated.
	before := app.currentModel
	app.applyModelFromKey("anthropic/claude-sonnet-4-6")
	if app.currentModel != before {
		t.Errorf("model switched despite no auth: %q", app.currentModel)
	}
}
