package tui

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/losinggeneration/tau/tui/internal/testbackend"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProgramStatusSanitizesAndBoundsMessage(t *testing.T) {
	sequence, err := FormatProgramStatus(ProgramStatus{State: "blocked", App: "tau", Kind: "auth", Message: "\x1b" + strings.Repeat("界", 1000)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sequence, "\x1b]7501;state=blocked:app=tau:kind=auth:msg=") || len(sequence) > 2800 {
		t.Fatalf("invalid status %q", sequence)
	}
	var output bytes.Buffer
	if err := WriteProgramStatus(&output, false, ProgramStatus{State: "working"}); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatal("unsupported terminal received status")
	}
	if _, err := FormatProgramStatus(ProgramStatus{State: "injected\x1b"}); err == nil {
		t.Fatal("invalid state accepted")
	}
}

func TestProgramStatusNegotiatesAndReplaysLatestAtBackend(t *testing.T) {
	t.Setenv("TAU_PROGRAM_STATUS", "")
	t.Setenv("PI_PROGRAM_STATUS", "")
	be := testbackend.New(Size{W: 20, H: 4}, nil)
	app, err := New(Opts{Root: NewText("hello"), Backend: be})
	require.NoError(t, err)
	require.NoError(t, app.SetProgramStatus(ProgramStatus{State: "working", App: "tau"}))
	require.Empty(t, be.Output())
	require.NoError(t, app.Start())
	t.Cleanup(func() { require.NoError(t, app.Stop()) })
	output := string(be.Output())
	require.Contains(t, output, "\x1b]7501;?\x1b\\\x1b[c")
	require.NotContains(t, output, "state=working")
	app.handleTerminalReply(TerminalReplyEvent{Sequence: "\x1b]7501;?;version=1\x1b\\"})
	require.Contains(t, string(be.Output()), "state=working:app=tau")
	require.NoError(t, app.SetProgramStatus(ProgramStatus{State: "blocked", Kind: "question"}))
	require.Contains(t, string(be.Output()), "state=blocked:kind=question")
	require.NoError(t, app.Stop())
	require.Contains(t, string(be.Output()), "state=clear")
}

func TestProgramStatusOverridesAndSentinel(t *testing.T) {
	for _, override := range []string{"0", "1", ""} {
		t.Run("override="+override, func(t *testing.T) {
			t.Setenv("TAU_PROGRAM_STATUS", "")
			t.Setenv("PI_PROGRAM_STATUS", override)
			be := testbackend.New(Size{W: 20, H: 4}, nil)
			app, err := New(Opts{Root: NewText("hello"), Backend: be})
			require.NoError(t, err)
			require.NoError(t, app.SetProgramStatus(ProgramStatus{State: "idle"}))
			require.NoError(t, app.Start())
			defer func() { require.NoError(t, app.Stop()) }()
			before := string(be.Output())
			assert.Equal(t, override == "", strings.Contains(before, "7501;?"))
			assert.Equal(t, override == "1", strings.Contains(before, "state=idle"))
			app.handleTerminalReply(TerminalReplyEvent{Sequence: "\x1b[?1;2c"})
			app.handleTerminalReply(TerminalReplyEvent{Sequence: "\x1b]7501;?\x1b\\"})
			assert.Equal(t, before, string(be.Output()), "unsolicited or late support cannot enable reporting")
		})
	}
}

func TestTerminalRepliesReachAppWithoutBecomingEditorInput(t *testing.T) {
	t.Setenv("PI_PROGRAM_STATUS", "0")
	t.Setenv("TAU_PROGRAM_STATUS", "")
	var replies []string
	state := &counterState{}
	app, err := New(Opts{
		Root:            newCounterView(state),
		Stdin:           strings.NewReader("\x1b]10;rgb:ff/ee/dd\x07\x1b[?997;2n+"),
		OnTerminalReply: func(reply TerminalReplyEvent) { replies = append(replies, reply.Sequence) },
	})
	require.NoError(t, err)
	app.AddInputListener(func(e Event) (Event, bool) {
		_, reply := e.(TerminalReplyEvent)
		return nil, reply
	})
	require.NoError(t, app.Run())
	assert.Equal(t, []string{"\x1b]10;rgb:ff/ee/dd\x1b\\", "\x1b[?997;2n"}, replies)
	assert.Equal(t, 1, state.Value())
}

func TestProgramStatusTransportFailuresDoNotSkipRestore(t *testing.T) {
	t.Setenv("TAU_PROGRAM_STATUS", "")
	t.Setenv("PI_PROGRAM_STATUS", "1")
	want := errors.New("status transport failed")
	be := &statusFailureBackend{Backend: testbackend.New(Size{W: 20, H: 4}, nil)}
	app, err := New(Opts{Root: NewText("hello"), Backend: be})
	require.NoError(t, err)
	require.NoError(t, app.Start())
	require.NoError(t, app.SetProgramStatus(ProgramStatus{State: "working"}))
	be.err = want
	require.ErrorIs(t, app.SetProgramStatus(ProgramStatus{State: "done"}), want)
	require.ErrorIs(t, app.Stop(), want)
	assert.True(t, be.Restored())
}

type statusFailureBackend struct {
	*testbackend.Backend
	err error
}

func (b *statusFailureBackend) Write(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	return b.Backend.Write(p)
}
