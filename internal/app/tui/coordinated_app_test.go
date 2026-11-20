package tui

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/losinggeneration/tau/ai"
	"github.com/stretchr/testify/require"

	"github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
)

func TestCoordinatedBashCompletionPreservesOutput(t *testing.T) {
	a := newTestApp(t)
	a.applyCoordinatedEvent(sdk.Event{Type: "bash_execution_update", Payload: map[string]any{"id": "shell", "delta": "worker output\n"}})
	a.applyCoordinatedEvent(sdk.Event{Type: "bash_execution_end", Payload: map[string]any{"id": "shell"}})
	idx := a.conv.findTool("shell")
	if idx < 0 {
		t.Fatal("missing shell row")
	}
	row := a.conv.committed[idx]
	if row.toolState != toolExecutionComplete || row.toolOutput != "worker output" || a.coordinatedBash != "" {
		t.Fatalf("completed shell: state=%v output=%q active=%q", row.toolState, row.toolOutput, a.coordinatedBash)
	}
}

func TestPendingCoordinatedSendKeepsDraftAndPreventsLeaving(t *testing.T) {
	a := newTestApp(t)
	a.coordinatedPendingSend = true
	a.editor = newPromptEditorForTest()
	ctx := testCtx()
	ctx.FocusedID = a.editor.ID()
	a.editor.SetText(ctx, "pending draft")
	a.instanceAction = func(string, string, string) { t.Error("switched while send was pending") }
	a.cancelRun = func() { t.Error("quit while send was pending") }
	a.confirmOrQuit(ctx)
	if a.editor.Text() != "pending draft" {
		t.Fatalf("draft = %q", a.editor.Text())
	}
	a.leaveInstance("instances", "")
	a.quit()
}

func TestUnattachedComposerRequestsWorkerOnlyOnSubmit(t *testing.T) {
	for _, tc := range []struct{ text, action, arg, draft string }{
		{"", "", "", ""},
		{"first message", "prompt", "first message", "first message"},
		{"/new", "new", "", ""},
		{"!pwd", "", "", ""},
		{"/resume saved", "resume", "saved", ""},
		{"/instances", "instances", "", ""},
	} {
		t.Run(tc.action, func(t *testing.T) {
			var action, arg, draft string
			a := newTestApp(t)
			a.sess = nil
			a.app = &tauui.App{}
			a.editor = newPromptEditorForTest()
			a.instanceAction = func(ac, ar, d string) { action, arg, draft = ac, ar, d }
			a.cmds.Register(Command{Name: "resume", Handler: cmdResume})
			a.cmds.Register(Command{Name: "new", Handler: cmdNew})
			a.registerInstanceCommands()
			ctx := testCtx()
			ctx.FocusedID = a.editor.ID()
			a.editor.SetText(ctx, tc.text)
			if !a.trySubmit(ctx) {
				t.Fatal("submit not consumed")
			}
			if action != tc.action || arg != tc.arg || draft != tc.draft {
				t.Fatalf("action=%q arg=%q draft=%q", action, arg, draft)
			}
		})
	}
}

func TestUnattachedComposerKeepsSessionCommands(t *testing.T) {
	a := newTestApp(t)
	a.sess = nil
	a.editor = newPromptEditorForTest()
	a.instanceAction = func(string, string, string) { t.Error("session command launched a worker") }
	a.cmds.Register(Command{Name: "retry"})
	ctx := testCtx()
	ctx.FocusedID = a.editor.ID()
	a.editor.SetText(ctx, "/retry")
	if !a.trySubmit(ctx) || a.editor.Text() != "/retry" {
		t.Fatalf("session command was discarded: %q", a.editor.Text())
	}
}

func TestUnattachedModelSelectionSetsStartupDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	credentials := appCredentialStore(t)
	require.NoError(t, credentials.Set(t.Context(), ai.ProviderOpenAICodex, ai.NewOAuthCredential("fixture-access", "fixture-refresh", time.Now().Add(time.Hour), "")))
	a, err := New(Options{SettingsPath: path, CredentialStore: credentials, InstanceAction: func(string, string, string) { t.Fatal("model selection launched a worker") }})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
	const model = "openai-codex/gpt-5.6-luna"
	a.applyModelFromKey(model)
	saved, err := ai.LoadSettings(path)
	require.NoError(t, err)
	require.Equal(t, model, ai.NewModelResolver(saved).ResolveDefaultModel().String())
	got, _ := a.footer.render(160)
	require.Contains(t, got, model+" • medium")
}

func TestUnattachedFooterShowsConfiguredModel(t *testing.T) {
	a, err := New(Options{DisplaySettings: &ai.Settings{DefaultProvider: ai.ProviderOpenAICodex, DefaultModel: "gpt-5.6-luna", DefaultThinkingLevel: "high"}, InstanceAction: func(string, string, string) { t.Fatal("rendering launched a worker") }})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
	got, _ := a.footer.render(160)
	require.Contains(t, got, "openai-codex/gpt-5.6-luna • high")
}
