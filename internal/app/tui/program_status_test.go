package tui

import (
	"testing"

	"github.com/losinggeneration/rovel/backend/headless"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
)

func TestProgramStatusFollowsRunSettlementAndRetries(t *testing.T) {
	var reporter programStatusReporter
	assert.Equal(t, "idle", reporter.current().State)
	for _, step := range []struct {
		event   sdk.Event
		want    string
		message string
	}{
		{sdk.Event{Type: "agent_start"}, "working", ""},
		{sdk.Event{Type: "message_end", Payload: map[string]any{"role": "assistant", "finish_reason": "error", "error_message": "Denied\nprivate detail"}}, "working", ""},
		{sdk.Event{Type: "error", Payload: map[string]any{"message": "Denied\nprivate detail"}}, "working", ""},
		{sdk.Event{Type: "session_end"}, "error", "Denied"},
		{sdk.Event{Type: "agent_start"}, "working", ""},
		{sdk.Event{Type: "message_end", Payload: map[string]any{"role": "assistant", "finish_reason": "error", "error_message": "retry me"}}, "working", ""},
		{sdk.Event{Type: "agent_end"}, "working", ""},
		{sdk.Event{Type: "agent_start"}, "working", ""},
		{sdk.Event{Type: "message_end", Payload: map[string]any{"role": "assistant", "finish_reason": "stop", "content": "private answer"}}, "working", ""},
		{sdk.Event{Type: "session_end"}, "done", ""},
		{sdk.Event{Type: "agent_start"}, "working", ""},
		{sdk.Event{Type: "message_end", Payload: map[string]any{"role": "assistant", "finish_reason": "aborted"}}, "working", ""},
		{sdk.Event{Type: "error", Payload: map[string]any{"message": "provider response aborted"}}, "working", ""},
		{sdk.Event{Type: "session_end"}, "idle", ""},
	} {
		reporter.handleEvent(step.event)
		got := reporter.current()
		assert.Equal(t, step.want, got.State, step.event.Type)
		assert.Equal(t, step.message, got.Message, step.event.Type)
	}
}

func TestProgramStatusCompactionAndDialogsOverrideRunWithoutLosingOutcome(t *testing.T) {
	var reporter programStatusReporter
	reporter.handleEvent(sdk.Event{Type: "compaction_start"})
	assert.Equal(t, tauui.ProgramStatus{State: "working", Message: "Compacting context"}, reporter.current())
	reporter.blocked = tauui.ProgramStatus{State: "blocked", Kind: "question", Message: "Choose"}
	reporter.handleEvent(sdk.Event{Type: "compaction_end", Payload: map[string]any{"reason": "manual", "error_message": "Failed\nsecret"}})
	assert.Equal(t, "blocked", reporter.current().State)
	reporter.blocked = tauui.ProgramStatus{}
	assert.Equal(t, tauui.ProgramStatus{State: "error", Message: "Failed"}, reporter.current())
	reporter.handleEvent(sdk.Event{Type: "compaction_start"})
	reporter.handleEvent(sdk.Event{Type: "compaction_end", Payload: map[string]any{"aborted": true}})
	assert.Equal(t, "idle", reporter.current().State)
}

func TestProgramStatusAppReportsFinalErrorInsteadOfOverwritingWithDone(t *testing.T) {
	t.Setenv("TAU_PROGRAM_STATUS", "")
	t.Setenv("PI_PROGRAM_STATUS", "1")
	a := newTestApp(t)
	be := headless.New(tauui.Size{W: 20, H: 4})
	toolkit, err := tauui.New(tauui.Opts{Root: tauui.NewText("status"), Backend: be})
	require.NoError(t, err)
	a.app = toolkit
	require.NoError(t, toolkit.Start())
	defer func() { require.NoError(t, toolkit.Stop()) }()
	a.handleEvent(sdk.Event{Type: "agent_start"})
	a.handleEvent(sdk.Event{Type: "error", Payload: map[string]any{"message": "Denied"}})
	a.handleEvent(sdk.Event{Type: "session_end"})
	output := string(be.Output())
	assert.Contains(t, output, "state=working:app=tau")
	assert.Contains(t, output, "state=error:app=tau:msg=RGVuaWVk")
	assert.NotContains(t, output, "state=done")
	a.resetProgramStatus()
	assert.Contains(t, string(be.Output()), "state=idle:app=tau")
}

func TestProgramStatusSettlementAbortOverridesMissingOrSuccessfulAssistant(t *testing.T) {
	for _, outcome := range []string{"missing", "stop", "error"} {
		t.Run(outcome, func(t *testing.T) {
			var reporter programStatusReporter
			reporter.handleEvent(sdk.Event{Type: "agent_start"})
			if outcome != "missing" {
				reporter.handleEvent(sdk.Event{Type: "message_end", Payload: map[string]any{"role": "assistant", "finish_reason": outcome, "error_message": "retry me"}})
			}
			reporter.handleEvent(sdk.Event{Type: "error", Payload: map[string]any{"message": "context canceled"}})
			reporter.handleEvent(sdk.Event{Type: "session_end", Payload: map[string]any{"aborted": true}})
			assert.Equal(t, tauui.ProgramStatus{State: "idle"}, reporter.current())
			reporter.handleEvent(sdk.Event{Type: "agent_start"})
			reporter.handleEvent(sdk.Event{Type: "session_end", Payload: map[string]any{"aborted": false}})
			assert.Equal(t, "done", reporter.current().State)
		})
	}
}

func TestProgramStatusAppReportsIdleOnSettlementAbort(t *testing.T) {
	t.Setenv("TAU_PROGRAM_STATUS", "")
	t.Setenv("PI_PROGRAM_STATUS", "1")
	a := newTestApp(t)
	be := headless.New(tauui.Size{W: 20, H: 4})
	toolkit, err := tauui.New(tauui.Opts{Root: tauui.NewText("status"), Backend: be})
	require.NoError(t, err)
	a.app = toolkit
	require.NoError(t, toolkit.Start())
	defer func() { require.NoError(t, toolkit.Stop()) }()
	a.handleEvent(sdk.Event{Type: "agent_start"})
	a.handleEvent(sdk.Event{Type: "error", Payload: map[string]any{"message": "context canceled"}})
	a.handleEvent(sdk.Event{Type: "session_end", Payload: map[string]any{"aborted": true}})
	output := string(be.Output())
	assert.Contains(t, output, "state=working:app=tau")
	assert.Contains(t, output, "state=idle:app=tau")
	assert.NotContains(t, output, "state=done")
	assert.NotContains(t, output, "state=error")
}
