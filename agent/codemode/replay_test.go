package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type replayLog struct {
	fingerprint string
	calls       []HostCall
	idles       [][2]int64
	errs        []error
	result      Result
}

// replay drives a Start run: at every idle it answers the calls still waiting,
// numbering them in call order but delivering them in reverse.
func replay(t *testing.T, ctx context.Context, code string, seed int64, answer func(HostCall) Outcome) replayLog {
	t.Helper()
	var log replayLog
	var waiting []HostCall
	// Callbacks can run before Start returns, so the run arrives on a channel.
	ready := make(chan *Run, 1)
	seq := int64(0)
	run, err := Start(ctx, code, RunOptions{
		StartedAt: 1_000_000, Seed: seed, Tools: []Tool{{Name: "echo"}, {Name: "fail"}},
		Globals:   []Tool{{Name: "clock"}},
		Timeout:   5 * time.Second,
		OnStarted: func(fingerprint string) { log.fingerprint = fingerprint },
		OnCall: func(call HostCall) {
			log.calls = append(log.calls, call)
			waiting = append(waiting, call)
		},
		OnIdle: func(processed, issued int64) {
			log.idles = append(log.idles, [2]int64{processed, issued})
			type numbered struct {
				seq  int64
				call HostCall
			}
			var batch []numbered
			for _, call := range waiting {
				seq++
				batch = append(batch, numbered{seq, call})
			}
			waiting = nil
			run := <-ready
			ready <- run
			for _, n := range slices.Backward(batch) {
				log.errs = append(log.errs, run.Deliver(n.seq, 1_000_000+float64(n.seq)*1000, n.call.ID, answer(n.call)))
			}
		},
	})
	require.NoError(t, err)
	ready <- run
	log.result = run.Result()
	assert.True(t, run.Finished())
	for i := range log.result.Calls {
		log.result.Calls[i].Duration = 0 // wall time, outside the replay contract
	}
	return log
}

func echoOrFail(call HostCall) Outcome {
	if call.Name == "fail" {
		return Outcome{Message: "exit 2", Properties: map[string]any{"code": 2, "message": "ignored"}}
	}
	if call.Name == "clock" {
		return Outcome{OK: true}
	}
	return Outcome{OK: true, Value: call.Args}
}

const replayScript = `
const started = Date.now();
const r = Math.random();
const [a, b] = await Promise.allSettled([tools.echo({ x: 1 }), tools.fail({})]);
const mid = Date.now();
await clock();
text(JSON.stringify({ started, r, a: a.value, b: b.reason.message, code: b.reason.code, mid, end: Date.now() }));
return await tools.echo({ n: Math.floor(Math.random() * 1e6) });`

func TestStartReplaysIdenticallyWhateverTheArrivalOrder(t *testing.T) {
	first := replay(t, t.Context(), replayScript, 42, echoOrFail)
	second := replay(t, t.Context(), replayScript, 42, echoOrFail)
	require.Nil(t, first.result.Error, first.result.Error)
	for _, err := range first.errs {
		require.NoError(t, err)
	}
	assert.Equal(t, first, second)

	assert.Regexp(t, `^goja .+, prelude [0-9a-f]{8}, deterministic 1, stack 10000, zone .+$`, first.fingerprint)
	assert.Equal(t, []string{"echo", "fail", "clock", "echo"}, []string{first.calls[0].Name, first.calls[1].Name, first.calls[2].Name, first.calls[3].Name})
	assert.Equal(t, []string{"tool", "tool", "global", "tool"}, []string{first.calls[0].Target, first.calls[1].Target, first.calls[2].Target, first.calls[3].Target})
	assert.Equal(t, [][2]int64{{0, 2}, {1, 2}, {2, 3}, {3, 4}}, first.idles)
	var out struct {
		Started, Mid, End float64
		R                 float64
		A                 map[string]any
		B                 string
		Code              float64
	}
	require.Len(t, first.result.Output, 1)
	require.NoError(t, json.Unmarshal([]byte(first.result.Output[0].Text), &out))
	assert.Equal(t, 1_000_000.0, out.Started, "the clock reads StartedAt before any result")
	assert.Equal(t, 1_002_000.0, out.Mid, "the clock reads the now of the result being processed")
	assert.Equal(t, 1_003_000.0, out.End)
	assert.Equal(t, map[string]any{"x": 1.0}, out.A)
	assert.Equal(t, "exit 2", out.B)
	assert.Equal(t, 2.0, out.Code)
	assert.Equal(t, &During{Seq: 4, Issued: 4}, first.result.During)
	assert.Equal(t, []CallStatus{CallOK, CallError, CallOK}, []CallStatus{first.result.Calls[0].Status, first.result.Calls[1].Status, first.result.Calls[2].Status})

	other := replay(t, t.Context(), replayScript, 7, echoOrFail)
	assert.NotEqual(t, first.result.Output, other.result.Output, "another seed draws other numbers")
}

func TestStartValidatesOptionsAndDeliveries(t *testing.T) {
	onCall := func(HostCall) {}
	for _, opts := range []RunOptions{
		{StartedAt: -1, OnCall: onCall},
		{StartedAt: math.Inf(1), OnCall: onCall},
		{Seed: math.MaxUint32 + 1, OnCall: onCall},
		{Seed: -1, OnCall: onCall},
		{},
	} {
		_, err := Start(t.Context(), "return 1", opts)
		assert.Error(t, err)
	}

	calls := make(chan HostCall, 1)
	run, err := Start(t.Context(), "return await tools.echo({})", RunOptions{StartedAt: 5, Tools: []Tool{{Name: "echo"}}, OnCall: func(c HostCall) { calls <- c }})
	require.NoError(t, err)
	call := <-calls
	assert.EqualError(t, run.Deliver(0, 1, call.ID, Outcome{OK: true}), "result number 0 is not a positive integer used once")
	assert.EqualError(t, run.Deliver(1, math.NaN(), call.ID, Outcome{OK: true}), "the clock value of result 1 is not a finite number")
	assert.EqualError(t, run.Deliver(1, 1, 99, Outcome{OK: true}), "call 99 is not waiting for a result")
	assert.EqualError(t, run.Deliver(1, 1, call.ID, Outcome{Message: "x", Properties: []int{1}}), "the properties of result 1 are not an object")
	assert.Error(t, run.Deliver(1, 1, call.ID, Outcome{OK: true, Value: math.NaN()}), "a value JSON can't hold leaves the call waiting")
	require.NoError(t, run.Deliver(1, 1, call.ID, Outcome{OK: true, Value: "ok"}))
	assert.EqualError(t, run.Deliver(1, 1, call.ID, Outcome{OK: true}), "result number 1 is not a positive integer used once")
	result := run.Result()
	require.Nil(t, result.Error)
	assert.JSONEq(t, `"ok"`, string(result.Value))
	assert.True(t, run.Finished())
	assert.NoError(t, run.Deliver(2, 1, call.ID, Outcome{OK: true}), "deliveries after the run finished are ignored")
}

func TestStartCancellationEndsAWaitingRun(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	run, err := Start(ctx, "await tools.echo({})", RunOptions{Tools: []Tool{{Name: "echo"}}, OnCall: func(HostCall) { cancel() }})
	require.NoError(t, err)
	result := run.Result()
	require.NotNil(t, result.Error)
	assert.Equal(t, ErrorAborted, result.Error.Kind)
	assert.Equal(t, &During{Seq: 0, Issued: 1}, result.During)
	assert.Equal(t, CallCancelled, result.Calls[0].Status)

	run, err = Start(t.Context(), "await new Promise(() => {})", RunOptions{OnCall: func(HostCall) {}})
	require.NoError(t, err)
	result = run.Result()
	require.NotNil(t, result.Error)
	assert.Contains(t, result.Error.Message, "can never settle")
}

func TestZoneNameFollowsGoZoneSelection(t *testing.T) {
	noEnv := func(string) (string, bool) { return "", false }
	env := func(value string) func(string) (string, bool) {
		return func(string) (string, bool) { return value, true }
	}
	link := func(target string) func(string) (string, error) {
		return func(string) (string, error) { return target, nil }
	}
	noLink := func(string) (string, error) { return "", errors.New("not a link") }

	assert.Equal(t, "America/Los_Angeles", zoneName(env(":America/Los_Angeles"), noLink))
	assert.Equal(t, "UTC", zoneName(env(""), link("/usr/share/zoneinfo/Europe/Berlin")), "an empty TZ means UTC")
	assert.Equal(t, "Europe/Berlin", zoneName(noEnv, link("../usr/share/zoneinfo/Europe/Berlin")))
	assert.Empty(t, zoneName(noEnv, noLink))
	assert.Empty(t, zoneName(noEnv, link("/etc/custom-zone")))
}
