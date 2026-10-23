package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/losinggeneration/tau/tui/internal/testbackend"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTerminalColorReplies(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  RGBColor
	}{
		{"#ff0080", RGBColor{255, 0, 128}},
		{"#ffff00008080", RGBColor{255, 0, 128}},
		{"rgb:f/0/8", RGBColor{255, 0, 136}},
		{"RGBA:ffff/0000/8080/ffff", RGBColor{255, 0, 128}},
		{"  rgb:ff/00/80  ", RGBColor{255, 0, 128}},
	} {
		for _, terminator := range []string{"\a", "\x1b\\"} {
			target, color, ok := parseTerminalColorReply("\x1b]4;13;" + tc.value + terminator)
			require.True(t, ok, tc.value)
			assert.Equal(t, 13, target)
			require.NotNil(t, color)
			assert.Equal(t, tc.want, *color)
		}
	}
	for _, value := range []string{"#fff", "#gg0080", "rgb:ff/00", "rgb:ff/xx/80", "rgb:/00/80", "#ff0080\aextra", "rgb:fffffffffffffffff/0/0"} {
		_, color, _ := parseTerminalColorReply("\x1b]10;" + value + "\a")
		assert.Nil(t, color, value)
	}
	_, color, ok := parseTerminalColorReply("\x1b]12;#ff0080\a")
	assert.False(t, ok)
	assert.Nil(t, color)
}

func TestTerminalColorQueriesRespectStatusSentinelAndCompletePalette(t *testing.T) {
	t.Setenv("TAU_PROGRAM_STATUS", "")
	t.Setenv("PI_PROGRAM_STATUS", "")
	be := testbackend.New(Size{W: 20, H: 4}, nil)
	app, err := New(Opts{Root: NewText("colors"), Backend: be})
	require.NoError(t, err)
	require.NoError(t, app.Start())
	defer func() { require.NoError(t, app.Stop()) }()
	var results []TerminalColors
	require.NoError(t, app.QueryTerminalColors(time.Hour, func(colors TerminalColors) { results = append(results, colors) }))
	output := string(be.Output())
	assert.Contains(t, output, "\x1b]10;?\a\x1b]11;?\a\x1b]4;0;?\a")
	assert.True(t, strings.HasSuffix(output, "\x1b[c"))
	app.handleTerminalReply(TerminalReplyEvent{Sequence: "\x1b]7501;?\x1b\\"})
	app.handleTerminalReply(TerminalReplyEvent{Sequence: "\x1b[?1;2c"})
	require.Empty(t, results, "the status sentinel must not settle the color query")
	app.handleTerminalReply(TerminalReplyEvent{Sequence: "\x1b]10;#ffffff\a"})
	app.handleTerminalReply(TerminalReplyEvent{Sequence: "\x1b]11;rgb:0000/0000/0000\x1b\\"})
	for i := range 16 {
		app.handleTerminalReply(TerminalReplyEvent{Sequence: fmt.Sprintf("\x1b]4;%d;#ff0080\a", i)})
	}
	require.Len(t, results, 1)
	assert.Equal(t, RGBColor{255, 255, 255}, *results[0].Foreground)
	assert.Equal(t, RGBColor{}, *results[0].Background)
	require.NotNil(t, results[0].Palette)
	assert.Equal(t, RGBColor{255, 0, 128}, results[0].Palette[15])
	app.handleTerminalReply(TerminalReplyEvent{Sequence: "\x1b[?62;22c"})
	require.Len(t, results, 1)
}

func TestTerminalColorQueriesRetainLateRepliesAndQueryOrder(t *testing.T) {
	t.Setenv("PI_PROGRAM_STATUS", "0")
	t.Setenv("TAU_PROGRAM_STATUS", "")
	be := testbackend.New(Size{W: 20, H: 4}, nil)
	app, err := New(Opts{Root: NewText("colors"), Backend: be})
	require.NoError(t, err)
	require.NoError(t, app.Start())
	defer func() { require.NoError(t, app.Stop()) }()
	var first, second []TerminalColors
	require.NoError(t, app.QueryTerminalColors(time.Hour, func(c TerminalColors) { first = append(first, c) }))
	require.NoError(t, app.QueryTerminalColors(time.Hour, func(c TerminalColors) { second = append(second, c) }))
	q := app.terminalColors.queries[0]
	app.expireTerminalColorQuery(q)
	require.Len(t, first, 1)
	assert.Empty(t, first[0])
	app.handleTerminalReply(TerminalReplyEvent{Sequence: "\x1b]11;#ffffff\a"})
	for i := range 8 {
		app.handleTerminalReply(TerminalReplyEvent{Sequence: fmt.Sprintf("\x1b]4;%d;#000000\a", i)})
	}
	app.handleTerminalReply(TerminalReplyEvent{Sequence: "\x1b[?62;22c"})
	require.Len(t, first, 2)
	assert.Equal(t, RGBColor{255, 255, 255}, *first[1].Background)
	assert.Nil(t, first[1].Palette)
	assert.Nil(t, first[0].Background, "delivered snapshots must not change")
	app.handleTerminalReply(TerminalReplyEvent{Sequence: "\x1b[?62;22c"})
	require.Len(t, second, 1)
	assert.Empty(t, second[0])
	assert.Empty(t, app.terminalColors.queries)
}

func TestTerminalColorsNotificationsCleanupAndWriteFailure(t *testing.T) {
	t.Setenv("PI_PROGRAM_STATUS", "0")
	t.Setenv("TAU_PROGRAM_STATUS", "")
	be := &statusFailureBackend{Backend: testbackend.New(Size{W: 20, H: 4}, nil)}
	var schemes []string
	app, err := New(Opts{Root: NewText("colors"), Backend: be, OnTerminalColorScheme: func(s string) { schemes = append(schemes, s) }})
	require.NoError(t, err)
	require.NoError(t, app.SetTerminalColorSchemeNotifications(true))
	assert.Empty(t, be.Output())
	require.NoError(t, app.Start())
	assert.Contains(t, string(be.Output()), "\x1b[?2031h")
	app.handleTerminalReply(TerminalReplyEvent{Sequence: "\x1b[?997;2n"})
	app.handleTerminalReply(TerminalReplyEvent{Sequence: "\x1b[?997;3n"})
	assert.Equal(t, []string{"light"}, schemes)
	var delivered int
	require.NoError(t, app.QueryTerminalColors(time.Hour, func(TerminalColors) { delivered++ }))
	q := app.terminalColors.queries[0]
	require.NoError(t, app.Stop())
	assert.Contains(t, string(be.Output()), "\x1b[?2031l")
	app.expireTerminalColorQuery(q)
	assert.Zero(t, delivered)
	assert.Empty(t, app.terminalColors.queries)
	require.NoError(t, app.Start())
	failure := errors.New("color query failed")
	be.err = failure
	require.ErrorIs(t, app.QueryTerminalColors(time.Hour, func(TerminalColors) { delivered++ }), failure)
	assert.Empty(t, app.terminalColors.queries)
	require.ErrorIs(t, app.Stop(), failure)
	assert.True(t, be.Restored())
}

func TestTerminalColorTimeoutRunsOnAppLoop(t *testing.T) {
	t.Setenv("PI_PROGRAM_STATUS", "0")
	t.Setenv("TAU_PROGRAM_STATUS", "")
	be := testbackend.New(Size{W: 20, H: 4}, nil)
	app, err := New(Opts{Root: NewText("colors"), Backend: be})
	require.NoError(t, err)
	var results []TerminalColors
	require.NoError(t, app.Post(func(*UpdateCtx) {
		require.NoError(t, app.QueryTerminalColors(time.Millisecond, func(c TerminalColors) {
			results = append(results, c)
			app.Quit()
		}))
	}))
	watchdog := time.AfterFunc(time.Second, app.Quit)
	defer watchdog.Stop()
	require.NoError(t, app.Run())
	require.Len(t, results, 1)
	assert.Empty(t, results[0])
	assert.Empty(t, app.terminalColors.queries)
}

func TestTerminalColorMalformedPaletteEntryCannotBecomeBlack(t *testing.T) {
	t.Setenv("PI_PROGRAM_STATUS", "0")
	t.Setenv("TAU_PROGRAM_STATUS", "")
	app, err := New(Opts{Root: NewText("colors"), Backend: testbackend.New(Size{W: 20, H: 4}, nil)})
	require.NoError(t, err)
	require.NoError(t, app.Start())
	defer func() { require.NoError(t, app.Stop()) }()
	var results []TerminalColors
	require.NoError(t, app.QueryTerminalColors(time.Hour, func(c TerminalColors) { results = append(results, c) }))
	app.handleTerminalReply(TerminalReplyEvent{Sequence: "\x1b]4;16;#ffffff\a"})
	app.handleTerminalReply(TerminalReplyEvent{Sequence: "\x1b]4;17;#ffffff\a"})
	app.handleTerminalReply(TerminalReplyEvent{Sequence: "\x1b]4;0;bogus\a"})
	for i := range 16 {
		app.handleTerminalReply(TerminalReplyEvent{Sequence: fmt.Sprintf("\x1b]4;%d;#000000\a", i)})
	}
	require.Empty(t, results)
	app.handleTerminalReply(TerminalReplyEvent{Sequence: "\x1b]10;#ffffff\a"})
	app.handleTerminalReply(TerminalReplyEvent{Sequence: "\x1b]11;#000000\a"})
	require.Len(t, results, 1)
	assert.Nil(t, results[0].Palette)
	assert.Equal(t, RGBColor{}, *results[0].Background)
}
