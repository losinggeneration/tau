package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pngData  = "iVBORw0KGgo="
	jpegData = "/9j/4A=="
	gifData  = "R0lGODlh"
	webpData = "UklGRgAAAABXRUJQ"
)

var echo = Tool{Name: "echo", Execute: func(_ context.Context, args json.RawMessage) (any, error) {
	if args == nil {
		return nil, nil
	}
	return args, nil
}}

func run(t *testing.T, code string, opts Options) Result {
	t.Helper()
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Second
	}
	result, err := Execute(t.Context(), code, opts)
	require.NoError(t, err)
	return result
}

func value(t *testing.T, r Result) any {
	t.Helper()
	require.Nil(t, r.Error, "script failed: %+v", r.Error)
	var v any
	if r.Value != nil {
		require.NoError(t, json.Unmarshal(r.Value, &v))
	}
	return v
}

func TestExecuteReturnsValuesAndOutput(t *testing.T) {
	r := run(t, "const x = await Promise.resolve(2);\nreturn { a: x * 3, b: [1, 'two'] }", Options{})
	assert.JSONEq(t, `{"a":6,"b":[1,"two"]}`, string(r.Value))

	r = run(t, `
		console.log("hello", 1, { a: 1 });
		text({ json: true });
		text(undefined);
		text(7);
		image("data:image/png;base64,`+pngData+`");
		image({ image_url: "data:image/jpeg;base64,`+jpegData+`" });
		image({ type: "image", data: "`+gifData+`", mimeType: "image/gif" });
		image("data:image/png;base64,`+webpData+`");
		image("data:image/png;base64,iVBORw0K\r\nGgo=\n");
		console.error(new Error("bad"));
		return null;`, Options{})
	require.Nil(t, r.Error)
	require.Len(t, r.Output, 10)
	assert.Equal(t, []OutputItem{
		{Type: "text", Text: `hello 1 {"a":1}`, Console: true},
		{Type: "text", Text: `{"json":true}`},
		{Type: "text", Text: "undefined"},
		{Type: "text", Text: "7"},
		{Type: "image", Data: pngData, MIMEType: "image/png"},
		{Type: "image", Data: jpegData, MIMEType: "image/jpeg"},
		{Type: "image", Data: gifData, MIMEType: "image/gif"},
		{Type: "image", Data: webpData, MIMEType: "image/webp"},
		{Type: "image", Data: pngData, MIMEType: "image/png"},
	}, r.Output[:9])
	assert.True(t, r.Output[9].Console)
	assert.True(t, strings.HasPrefix(r.Output[9].Text, "Error: bad"), r.Output[9].Text)
}

func TestExecuteRejectsInvalidImages(t *testing.T) {
	r := run(t, `
		const errors = [];
		for (const run of [
			() => image(""),
			() => image("https://example.com/a.png"),
			() => image("data:image/png,raw"),
			() => image({ type: "text", text: "x" }),
			() => image({ type: "image", data: "" }),
			() => image("data:image/png;base64,AAAA!"),
			() => image("data:image/png;base64,QUJD"),
		]) {
			try { run(); errors.push("no error"); } catch (error) { errors.push(error.name + ": " + error.message); }
		}
		return errors;`, Options{})
	assert.Equal(t, []any{
		`TypeError: image expects a non-empty image URL string, an object with image_url, or an image block { type: "image", data, mimeType }`,
		"TypeError: remote image URLs are not supported in tool outputs. Pass a base64 data URI instead",
		"TypeError: invalid image output. Pass a base64 data URI instead",
		`TypeError: image only accepts image blocks, got "text"`,
		"TypeError: image expected image block data",
		"TypeError: invalid image output. The image data is not valid base64 (truncated or corrupted?)",
		"TypeError: invalid image output. The image data is not a PNG, JPEG, GIF, or WebP image",
	}, value(t, r))
	assert.Empty(t, r.Output)
}

func TestExecuteReportsScriptErrors(t *testing.T) {
	r := run(t, "text('partial');\nthrow new TypeError('boom ' + 1)", Options{})
	require.NotNil(t, r.Error)
	assert.Equal(t, ErrorScript, r.Error.Kind)
	assert.Equal(t, "TypeError", r.Error.Name)
	assert.Equal(t, "boom 1", r.Error.Message)
	assert.Contains(t, r.Error.Stack, "codemode.js:2")
	assert.NotContains(t, r.Error.Stack, "codemode-prelude.js")
	assert.Equal(t, []OutputItem{{Type: "text", Text: "partial"}}, r.Output)
	assert.Nil(t, r.Value)

	r = run(t, "const a = 1;\nconst b = ;\nreturn a", Options{})
	require.NotNil(t, r.Error)
	assert.Equal(t, "SyntaxError", r.Error.Name)
	assert.Contains(t, r.Error.Stack, "codemode.js: Line 2")

	r = run(t, "throw { code: 7 }", Options{})
	assert.Equal(t, &Error{Kind: ErrorScript, Message: `{"code":7}`}, r.Error)

	r = run(t, "const error = new Error('x'); error.message = 42; throw error;", Options{})
	assert.Equal(t, "42", r.Error.Message)
}

func TestExecuteExitKeepsOutputAndWrites(t *testing.T) {
	r := run(t, `
		text("before");
		store("k", 1);
		await tools.echo(1);
		try { exit(); } catch {}
		text("after");
		return "unreachable";`, Options{Tools: []Tool{echo}})
	require.Nil(t, r.Error)
	assert.Nil(t, r.Value)
	assert.Equal(t, []OutputItem{{Type: "text", Text: "before"}}, r.Output)
	assert.Equal(t, map[string]json.RawMessage{"k": json.RawMessage("1")}, r.StoreWrites.Set)
}

func TestExecuteCallsToolsAndGlobals(t *testing.T) {
	failing := Tool{Name: "web-search", Execute: func(context.Context, json.RawMessage) (any, error) {
		return nil, errors.New("search is down")
	}}
	var globalArgs atomic.Value
	globals := []Tool{
		{Name: "hello", Execute: func(context.Context, json.RawMessage) (any, error) { return "hi", nil }},
		{Name: "ns.sum", Spread: true, Execute: func(_ context.Context, args json.RawMessage) (any, error) {
			globalArgs.Store(string(args))
			var nums []float64
			if err := json.Unmarshal(args, &nums); err != nil {
				return nil, err
			}
			return nums[0] + nums[1], nil
		}},
	}
	r := run(t, `
		const [a, b] = await Promise.all([tools.echo({ x: 1 }), tools["web-search"]({}).catch((e) => e.message)]);
		const none = await tools.echo();
		return [a, b, none, ALL_TOOLS.map((t) => t.name), await hello(), await ns.sum(2, 3), tools.web_search === tools["web-search"]];`,
		Options{Tools: []Tool{echo, failing}, Globals: globals})
	assert.Equal(t, []any{map[string]any{"x": float64(1)}, "search is down", nil, []any{"echo", "web_search"}, "hi", float64(5), true}, value(t, r))
	assert.Equal(t, "[2,3]", globalArgs.Load())
	require.Len(t, r.Calls, 3)
	assert.Equal(t, []string{"echo", "web-search", "echo"}, []string{r.Calls[0].Name, r.Calls[1].Name, r.Calls[2].Name})
	assert.Equal(t, CallOK, r.Calls[0].Status)
	assert.Equal(t, CallError, r.Calls[1].Status)

	attempt := func(expression string) string {
		r := run(t, "return "+expression, Options{Tools: []Tool{echo, failing}, Globals: globals})
		if r.Error != nil {
			return r.Error.Message
		}
		return string(r.Value)
	}
	assert.Equal(t, `tools.Echo does not exist. Did you mean tools.echo? ALL_TOOLS lists every tool; searchTools(query) finds tools by topic. Check for a member with "Echo" in tools.`, attempt("tools.Echo"))
	assert.Contains(t, attempt("tools.websearch"), "Did you mean tools.web_search?")
	assert.Contains(t, attempt("tools.nothing"), "Available: echo, web_search.")
	assert.Contains(t, attempt("ns.product"), "ns.product does not exist. Available: sum.")
	assert.JSONEq(t, `[true,false,"undefined","{}"]`, attempt("['echo' in tools, 'nothing' in tools, String(tools.toString), JSON.stringify(tools)]"))
}

func TestExecuteRejectsInvalidGlobals(t *testing.T) {
	for _, globals := range [][]Tool{
		{{Name: "text"}}, {{Name: "a.b.c"}}, {{Name: "bad-name"}}, {{Name: "x"}, {Name: "x"}}, {{Name: "ns"}, {Name: "ns.a"}},
	} {
		_, err := Execute(t.Context(), "", Options{Globals: globals})
		assert.Error(t, err, "%v", globals)
	}
}

func TestExecuteCancelsUnawaitedCalls(t *testing.T) {
	cancelled := make(chan struct{})
	slow := Tool{Name: "slow", Execute: func(ctx context.Context, _ json.RawMessage) (any, error) {
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	}}
	r := run(t, "tools.slow(); return 'early'", Options{Tools: []Tool{slow}})
	assert.Equal(t, "early", value(t, r))
	assert.Equal(t, CallCancelled, r.Calls[0].Status)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("unawaited call was not cancelled")
	}
}

func TestExecuteFailsPromisesNothingCanSettle(t *testing.T) {
	r := run(t, "await tools.echo(1); await new Promise(() => {}); return 'never'", Options{Tools: []Tool{echo}, Timeout: -1})
	require.NotNil(t, r.Error)
	assert.Equal(t, ErrorScript, r.Error.Kind)
	assert.Contains(t, r.Error.Message, "can never settle")
}

func TestExecuteInterruptsLoopsOnDeadlineAndCancel(t *testing.T) {
	for _, code := range []string{"while (true) {}", "while (true) await null", "async function f() { await null; return f() } await f()"} {
		start := time.Now()
		r := run(t, code, Options{Timeout: 50 * time.Millisecond})
		require.NotNil(t, r.Error, code)
		assert.Equal(t, &Error{Kind: ErrorTimeout, Message: "Execution timed out after 50 ms"}, r.Error, code)
		assert.Less(t, time.Since(start), time.Second)
	}

	called := make(chan struct{})
	var toolCtx context.Context
	hang := Tool{Name: "hang", Execute: func(ctx context.Context, _ json.RawMessage) (any, error) {
		toolCtx = ctx
		close(called)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	ctx, cancel := context.WithCancelCause(t.Context())
	go func() {
		<-called
		cancel(errors.New("user cancelled"))
	}()
	r, err := Execute(ctx, "await tools.hang(); return 'never'", Options{Tools: []Tool{hang}})
	require.NoError(t, err)
	assert.Equal(t, &Error{Kind: ErrorAborted, Message: "user cancelled"}, r.Error)
	assert.Equal(t, CallCancelled, r.Calls[0].Status)
	assert.Error(t, toolCtx.Err())

	ctx, plainCancel := context.WithCancel(t.Context())
	plainCancel()
	r, err = Execute(ctx, "while (true) {}", Options{})
	require.NoError(t, err)
	assert.Equal(t, &Error{Kind: ErrorAborted, Message: "Execution aborted"}, r.Error)
}

func TestExecuteOutputLimitsFailEvenWhenCaught(t *testing.T) {
	r := run(t, `const s = "x".repeat(1 << 20); for (;;) { try { text(s); } catch {} }`, Options{})
	require.NotNil(t, r.Error)
	assert.Equal(t, "RangeError", r.Error.Name)
	assert.Contains(t, r.Error.Message, "script output exceeded")
	chars := 0
	for _, item := range r.Output {
		chars += len(item.Text)
	}
	assert.LessOrEqual(t, chars, 16777216)

	r = run(t, `for (;;) text("");`, Options{})
	require.NotNil(t, r.Error)
	assert.Len(t, r.Output, 100000)
}

func TestExecuteStore(t *testing.T) {
	store := map[string]json.RawMessage{"keep": json.RawMessage(`{"n":1}`), "drop": json.RawMessage(`2`)}
	r := run(t, `
		const kept = load("keep");
		kept.n = 99;
		store("drop", undefined);
		store("new", [1, 2]);
		const errors = [];
		for (const run of [() => store(1, 1), () => store("f", () => 1), () => store("big", "x".repeat(262144))]) {
			try { run(); } catch (error) { errors.push(error.name); }
		}
		return [load("keep"), load("drop"), load("new"), errors];`, Options{Store: store})
	assert.Equal(t, []any{map[string]any{"n": float64(1)}, nil, []any{float64(1), float64(2)}, []any{"TypeError", "TypeError", "RangeError"}}, value(t, r))
	assert.Equal(t, StoreWrites{Set: map[string]json.RawMessage{"new": json.RawMessage("[1,2]")}, Delete: []string{"drop"}}, r.StoreWrites)

	r = run(t, `store("k", 1); throw new Error("no")`, Options{})
	assert.Empty(t, r.StoreWrites.Set)
}

func TestExecuteHasNoHostCapabilitiesAndFrozenBuiltins(t *testing.T) {
	r := run(t, `return [typeof process, typeof require, typeof module, typeof setTimeout, typeof fetch, typeof WebAssembly,
		eval("typeof require"), new Function("return typeof setTimeout")()]`, Options{})
	assert.Equal(t, []any{"undefined", "undefined", "undefined", "undefined", "undefined", "undefined", "undefined", "undefined"}, value(t, r))

	r = run(t, `
		Array.prototype.toJSON = () => null;
		Promise.prototype.then = () => {};
		globalThis.JSON = { stringify: () => "x", parse: () => "x" };
		store("k", [1]);
		const object = {};
		object.toString = () => "custom";
		class MyError extends Error { constructor(m) { super(m); this.name = "MyError"; } }
		let patched = "silent";
		try { Error.prototype.name = "Patched"; } catch (error) { patched = error.constructor.name; }
		try { tools.echo = () => "nope"; } catch {}
		return [await tools.echo([2]), JSON.stringify({ a: 1 }), String(object), new MyError("x").name, patched,
			Object.isFrozen(Object.getPrototypeOf(async function () {}))];`, Options{Tools: []Tool{echo}})
	assert.Equal(t, []any{[]any{float64(2)}, `{"a":1}`, "custom", "MyError", "TypeError", true}, value(t, r))
	assert.JSONEq(t, "[1]", string(r.StoreWrites.Set["k"]))
}

// goja's stack overflow is uncatchable, unlike QuickJS's RangeError, so the run fails.
func TestExecuteDeepRecursionFailsWithRangeError(t *testing.T) {
	r := run(t, `
		function dive() { dive(); }
		try { dive(); } catch (error) { return "caught"; }`, Options{})
	require.NotNil(t, r.Error)
	assert.Equal(t, ErrorScript, r.Error.Kind)
	assert.Equal(t, "RangeError", r.Error.Name)
	assert.LessOrEqual(t, strings.Count(r.Error.Stack, "\n"), 10)
}

func TestExecuteUsesInjectedClockAndRandom(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	r := run(t, "return [Date.now(), Math.random()]", Options{Now: func() time.Time { return at }, Rand: func() float64 { return 0.25 }})
	assert.Equal(t, []any{float64(at.UnixMilli()), 0.25}, value(t, r))
}
