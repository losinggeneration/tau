package tui

import (
	"testing"

	"github.com/losinggeneration/rovel/backend/ansi"
	"github.com/losinggeneration/tau/rpc/coord"
	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/keymap"
	"github.com/stretchr/testify/require"
)

func TestInstancePickerShowsAndFiltersFirstPrompt(t *testing.T) {
	const cwd = "/project"
	instances := []coord.Instance{
		{ID: "worker-a", CWD: cwd, SessionID: "session-a", FirstPrompt: "Fix\n  the picker", State: coord.StateWorking},
		{ID: "worker-b", CWD: cwd, SessionID: "session-b", FirstPrompt: "Review the tests", State: coord.StateAwaitingUser},
		{ID: "worker-c", CWD: cwd, SessionID: "session-c", State: coord.StateAwaitingUser},
	}
	items := instancePickerItems(instances, cwd, "worker-a", "")
	require.Len(t, items, 4)
	require.Equal(t, "Fix the picker", items[0].Name)
	require.Equal(t, "working · current · worker-a · /project", items[0].Description)
	require.Equal(t, "Review the tests", items[1].Name)
	require.Equal(t, cwd, items[2].Name)
	require.Equal(t, NewInstanceChoice, items[3].Value)

	filtered := instancePickerItems(instances, cwd, "worker-a", "PICKER")
	require.Len(t, filtered, 1)
	require.Equal(t, "worker-a", filtered[0].Value)
}

func TestInstancePickerMarksOutdatedStatus(t *testing.T) {
	instances := []coord.Instance{
		{ID: "worker-a", CWD: "/a", State: coord.StateAwaitingUser, Outdated: true},
		{ID: "worker-b", CWD: "/b", State: coord.StateWorking, PendingInputs: 2, Outdated: true},
		{ID: "worker-c", CWD: "/c", State: coord.StateWorking},
	}
	items := instancePickerItems(instances, "/a", "worker-a", "")
	require.Equal(t, "awaiting user ⟳ · current · worker-a", items[0].Description)
	require.Equal(t, "needs input (2) ⟳ · worker-b", items[1].Description)
	require.Equal(t, "working · worker-c", items[2].Description)
}

func TestInstancePickerKeymap(t *testing.T) {
	keymap := sessionSelectorKeymap{keys: instancePickerKey}
	for _, tc := range []struct {
		ks   tauui.Keystroke
		want tauui.Action
	}{
		{tauui.Keystroke{Key: tauui.KeyF1}, instancePickerRestart},
		{tauui.Keystroke{Key: tauui.KeyF1, Mod: tauui.ModShift}, instancePickerRestartOutdated},
		{tauui.Keystroke{Key: tauui.KeyF2}, instancePickerStop},
		{tauui.Keystroke{Key: tauui.KeyEsc}, tauui.ActionQuit},
	} {
		act, ok := keymap.Resolve(tauui.KeyCtxGlobal, tc.ks)
		require.True(t, ok, "%+v", tc.ks)
		require.Equal(t, tc.want, act, "%+v", tc.ks)
	}
	_, ok := instancePickerKey(tauui.Keystroke{Key: tauui.KeyF2, Mod: tauui.ModShift})
	require.False(t, ok)
}

func TestInstancePickerTerminalShortcuts(t *testing.T) {
	pickerKeys := sessionSelectorKeymap{keys: instancePickerKey}
	for _, tc := range []struct {
		name, sequence string
		want           tauui.Action
	}{
		{"F1", "\x1bOP", instancePickerRestart},
		{"Shift+F1", "\x1b[1;2P", instancePickerRestartOutdated},
		{"Shift+F1 (legacy)", "\x1b[25~", instancePickerRestartOutdated},
		{"F2", "\x1bOQ", instancePickerStop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var decoder ansi.DecodeInput
			events := decoder.Push(nil, []byte(tc.sequence))
			require.Len(t, events, 1, "terminal sequence %q should produce one key", tc.sequence)
			ke, ok := events[0].(tauui.KeyEvent)
			require.True(t, ok, "%T", events[0])
			action, found := pickerKeys.Resolve(tauui.KeyCtxGlobal, keymap.KeystrokeOf(ke))
			require.True(t, found, "%+v", ke)
			require.Equal(t, tc.want, action)
		})
	}
}

func TestInstancePickerActionTargets(t *testing.T) {
	instances := []coord.Instance{
		{ID: "worker-a", State: coord.StateAwaitingUser, Outdated: true},
		{ID: "worker-b", State: coord.StateAwaitingUser},
		{ID: "worker-c", State: coord.StateWorking, Outdated: true, LinkState: coord.LinkConnected},
		{ID: "worker-d", State: coord.StateAwaitingUser, Outdated: true, LinkState: "disconnected"},
	}

	got, handled := instancePickerAction(instancePickerRestart, instances, "worker-b")
	require.True(t, handled)
	require.Equal(t, InstanceSelection{Action: InstanceActionRestart, Targets: instances[1:2], Value: "worker-b"}, got)

	got, handled = instancePickerAction(instancePickerStop, instances, "worker-a")
	require.True(t, handled)
	require.Equal(t, InstanceSelection{Action: InstanceActionStop, Targets: instances[:1], Value: "worker-a"}, got)

	got, handled = instancePickerAction(instancePickerRestartOutdated, instances, NewInstanceChoice)
	require.True(t, handled)
	require.Equal(t, InstanceSelection{Action: InstanceActionRestartOutdated, Targets: []coord.Instance{instances[0], instances[2]}}, got)

	for _, focused := range []string{NewInstanceChoice, "", "worker-d"} {
		for _, act := range []tauui.Action{instancePickerRestart, instancePickerStop} {
			got, handled = instancePickerAction(act, instances, focused)
			require.True(t, handled)
			require.Zero(t, got)
		}
	}

	got, handled = instancePickerAction(instancePickerRestartOutdated, instances[1:2], "worker-b")
	require.True(t, handled)
	require.Zero(t, got)

	_, handled = instancePickerAction(tauui.ActionQuit, instances, "worker-a")
	require.False(t, handled)
}

func TestInstancePickerFocusRestoresState(t *testing.T) {
	items := []tauui.DialogPickerItem{{Value: "worker-a"}, {Value: "worker-b"}, {Value: NewInstanceChoice}}
	require.Equal(t, 1, instancePickerFocus(items, InstancePickerState{Focused: 0, FocusID: "worker-b"}))
	require.Equal(t, 1, instancePickerFocus(items, InstancePickerState{Focused: 1, FocusID: "stopped"}))
	require.Equal(t, 2, instancePickerFocus(items, InstancePickerState{Focused: 9}))
	require.Equal(t, 0, instancePickerFocus(nil, InstancePickerState{Focused: 3}))
}

func TestInstanceManagementPrompt(t *testing.T) {
	idle := coord.Instance{ID: "a", CWD: "/idle", State: coord.StateAwaitingUser}
	busy := coord.Instance{ID: "b", CWD: "/busy", State: coord.StateWorking}
	waiting := coord.Instance{ID: "c", CWD: "/waiting", State: coord.StateAwaitingUser, PendingInputs: 1}

	_, ok := instanceManagementPrompt(InstanceActionRestartOutdated, []coord.Instance{idle, idle})
	require.False(t, ok)
	confirmed, err := ConfirmInstanceManagement(t.Context(), InstanceActionStop, []coord.Instance{idle})
	require.NoError(t, err)
	require.True(t, confirmed)

	for _, state := range []string{coord.StateUsageLimited, coord.StateRetrying, coord.StatePaused} {
		_, ok = instanceManagementPrompt(InstanceActionStop, []coord.Instance{{ID: "d", CWD: "/stuck", State: state}})
		require.True(t, ok, state)
	}
	_, ok = instanceManagementPrompt(InstanceActionStop, []coord.Instance{{ID: "e", CWD: "/failed", State: coord.StateError}})
	require.False(t, ok)

	prompt, ok := instanceManagementPrompt(InstanceActionStop, []coord.Instance{busy})
	require.True(t, ok)
	require.Equal(t, "Stop this worker for all clients? Working or awaiting input:\n/busy", prompt)

	prompt, ok = instanceManagementPrompt(InstanceActionRestartOutdated, []coord.Instance{idle, busy, waiting})
	require.True(t, ok)
	require.Equal(t, "Restart 3 workers for all clients? Working or awaiting input:\n/busy\n/waiting", prompt)
}
