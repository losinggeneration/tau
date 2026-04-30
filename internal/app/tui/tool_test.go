package tui

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	tauui "github.com/losinggeneration/tau/tui"
)

func renderedToolLines(t *testing.T, theme string, build func(*ConversationModel)) []string {
	t.Helper()
	conv := NewConversationModel()
	build(conv)
	log := newTestChatLog(t, conv)
	if theme != "" {
		log.SetLookName(theme)
	}
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 60, H: 200})
	waitForChatLog(t, log)
	log.mu.Lock()
	defer log.mu.Unlock()
	return renderLineTexts(log.lines)
}

func countLinesContaining(lines []string, substr string) int {
	n := 0
	for _, line := range lines {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// Bounded preview belongs to the specific built-in renderers. A tool the app
// knows nothing about must render its whole result: truncating it would hide
// output with no way to get it back.
func TestGenericToolFallbackRendersFullOutput(t *testing.T) {
	const total = 60
	var output []string
	for i := range total {
		output = append(output, "generic-line-"+strconv.Itoa(i))
	}
	lines := renderedToolLines(t, "", func(conv *ConversationModel) {
		conv.AddToolCallWithID("tool-1", "some_unknown_tool", `{}`)
		conv.AddToolResultWithID("tool-1", strings.Join(output, "\n"), false)
	})

	for _, want := range []string{"generic-line-0", "generic-line-30", "generic-line-59"} {
		if countLinesContaining(lines, want) == 0 {
			t.Fatalf("generic tool output missing %q; got %d rows", want, len(lines))
		}
	}
	if countLinesContaining(lines, "truncated") != 0 {
		t.Fatalf("generic tool output was truncated: %#v", lines)
	}
}

func TestBashToolPreviewIsBounded(t *testing.T) {
	var output []string
	for i := range bashToolPreviewLines + 15 {
		output = append(output, fmt.Sprintf("bash-line-%02d", i))
	}
	for _, theme := range []string{"", "tau"} {
		lines := renderedToolLines(t, theme, func(conv *ConversationModel) {
			conv.AddToolCallWithID("tool-1", "bash", `{"command":"seq 35"}`)
			conv.AddToolResultWithID("tool-1", strings.Join(output, "\n"), false)
		})
		if countLinesContaining(lines, "bash output truncated") != 1 {
			t.Fatalf("theme %q: want exactly one bash truncation marker, got %#v", theme, lines)
		}
		last := fmt.Sprintf("bash-line-%02d", bashToolPreviewLines-1)
		if countLinesContaining(lines, last) == 0 {
			t.Fatalf("theme %q: preview should reach %q", theme, last)
		}
		beyond := fmt.Sprintf("bash-line-%02d", bashToolPreviewLines)
		if countLinesContaining(lines, beyond) != 0 {
			t.Fatalf("theme %q: preview leaked %q past the %d-line bound", theme, beyond, bashToolPreviewLines)
		}
	}
}

// The tau look draws bash inside a dynamic border whose title carries the
// execution state.
func TestBashToolDynamicBorderCarriesState(t *testing.T) {
	cases := []struct {
		name  string
		build func(*ConversationModel)
		want  string
	}{
		{
			name: "complete",
			build: func(conv *ConversationModel) {
				conv.AddToolCallWithID("tool-1", "bash", `{"command":"ls"}`)
				conv.AddToolResultWithID("tool-1", "a\nb", false)
			},
			want: "bash · complete",
		},
		{
			name: "error",
			build: func(conv *ConversationModel) {
				conv.AddToolCallWithID("tool-1", "bash", `{"command":"false"}`)
				conv.AddToolResultWithID("tool-1", "exit status 1", true)
			},
			want: "bash · error",
		},
		{
			name: "running",
			build: func(conv *ConversationModel) {
				conv.AddToolCallWithID("tool-1", "bash", `{"command":"sleep 10"}`)
			},
			want: "bash · running",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := renderedToolLines(t, "tau", tc.build)
			if countLinesContaining(lines, tc.want) == 0 {
				t.Fatalf("missing border title %q in %#v", tc.want, lines)
			}
			if countLinesContaining(lines, "╰") == 0 {
				t.Fatalf("missing dynamic border bottom in %#v", lines)
			}
		})
	}
}

// A successful read is described by its call line; echoing the file back would
// bury the transcript in content already on disk.
func TestReadToolHidesOutputOnSuccess(t *testing.T) {
	lines := renderedToolLines(t, "", func(conv *ConversationModel) {
		conv.AddToolCallWithID("tool-1", "read", `{"path":"main.go"}`)
		conv.AddToolResultWithID("tool-1", "package main\nfunc main() {}", false)
	})
	if countLinesContaining(lines, "read main.go") == 0 {
		t.Fatalf("missing read call line: %#v", lines)
	}
	if countLinesContaining(lines, "package main") != 0 {
		t.Fatalf("successful read echoed file contents: %#v", lines)
	}
}

// A failure is not described by its call line — the reason lives in the output.
func TestReadToolShowsBoundedOutputOnError(t *testing.T) {
	var output []string
	for i := range readToolPreviewLines + 5 {
		output = append(output, fmt.Sprintf("err-line-%02d", i))
	}
	lines := renderedToolLines(t, "", func(conv *ConversationModel) {
		conv.AddToolCallWithID("tool-1", "read", `{"path":"missing.go"}`)
		conv.AddToolResultWithID("tool-1", strings.Join(output, "\n"), true)
	})
	if countLinesContaining(lines, "err-line-00") == 0 {
		t.Fatalf("failed read hid its error output: %#v", lines)
	}
	if countLinesContaining(lines, "read output truncated") != 1 {
		t.Fatalf("failed read preview is unbounded: %#v", lines)
	}
	beyond := fmt.Sprintf("err-line-%02d", readToolPreviewLines)
	if countLinesContaining(lines, beyond) != 0 {
		t.Fatalf("failed read preview leaked %q past the bound: %#v", beyond, lines)
	}
}

func TestToolStyleUsesPerStateTokens(t *testing.T) {
	log := newTestChatLog(t, NewConversationModel())
	p := log.pal()

	cases := []struct {
		name string
		tool ToolExecutionComponent
		want tauui.Style
	}{
		{"complete", ToolExecutionComponent{State: toolExecutionComplete}, p.toolResult},
		{"executing", ToolExecutionComponent{State: toolExecutionExecuting}, p.toolCall},
		{"error", ToolExecutionComponent{State: toolExecutionError}, p.toolError},
		{"cancelled", ToolExecutionComponent{State: toolExecutionCancelled}, p.toolError},
		{"flagged error", ToolExecutionComponent{State: toolExecutionComplete, IsError: true}, p.toolError},
	}
	for _, tc := range cases {
		if got := log.toolStyle(tc.tool); got != tc.want {
			t.Fatalf("toolStyle(%s) = %#v, want %#v", tc.name, got, tc.want)
		}
	}
}

func TestPreviewLinesBounds(t *testing.T) {
	cases := []struct {
		in        string
		limit     int
		wantLen   int
		truncated bool
	}{
		{"", 5, 0, false},
		{"a\nb\n", 5, 2, false},
		{"a\nb\nc", 2, 2, true},
		{"a\nb\nc", 0, 3, false},
		{"a\nb\nc", 3, 3, false},
	}
	for _, tc := range cases {
		lines, truncated := previewLines(tc.in, tc.limit)
		if len(lines) != tc.wantLen || truncated != tc.truncated {
			t.Fatalf("previewLines(%q, %d) = %d lines/truncated=%v, want %d/%v",
				tc.in, tc.limit, len(lines), truncated, tc.wantLen, tc.truncated)
		}
	}
}

// hookRenderer implements whichever subset of the tool render hooks a test
// needs; nil funcs mean "this renderer does not implement that hook".
type hookRenderer struct {
	name   string
	shell  func(ToolExecutionComponent, int) ([]string, bool)
	call   func(ToolExecutionComponent, int) ([]string, bool)
	result func(ToolExecutionComponent, int) ([]string, bool)
}

func (h *hookRenderer) ToolName() string { return h.name }

func (h *hookRenderer) RenderShell(exec ToolExecutionComponent, width int) ([]string, bool) {
	if h.shell == nil {
		return nil, false
	}
	return h.shell(exec, width)
}

// callHookRenderer adds only the call hook, so the type assertion for
// ToolResultRenderer fails and the result stays built-in.
type callHookRenderer struct{ *hookRenderer }

func (h callHookRenderer) RenderCall(exec ToolExecutionComponent, width int) ([]string, bool) {
	return h.call(exec, width)
}

type resultHookRenderer struct{ *hookRenderer }

func (h resultHookRenderer) RenderResult(exec ToolExecutionComponent, width int) ([]string, bool) {
	return h.result(exec, width)
}

type bothHooksRenderer struct{ *hookRenderer }

func (h bothHooksRenderer) RenderCall(exec ToolExecutionComponent, width int) ([]string, bool) {
	return h.call(exec, width)
}

func (h bothHooksRenderer) RenderResult(exec ToolExecutionComponent, width int) ([]string, bool) {
	return h.result(exec, width)
}

func rowsFn(rows ...string) func(ToolExecutionComponent, int) ([]string, bool) {
	return func(ToolExecutionComponent, int) ([]string, bool) { return rows, true }
}

// A renderer that implements no optional hook and declines the shell leaves the
// tool entirely built-in.
func TestToolOverridesEmptyWhenRendererDeclines(t *testing.T) {
	r := newPluginRenderers()
	r.RegisterTool(&hookRenderer{name: "calc"})

	ov := r.toolOverrides(ToolExecutionComponent{Name: "calc"}, 40)
	if ov.hasShell || ov.hasCall || ov.hasResult || ov.partial() {
		t.Fatalf("overrides = %#v, want nothing claimed", ov)
	}
}

func TestToolOverridesShellSubsumesFinerHooks(t *testing.T) {
	base := &hookRenderer{
		name:   "calc",
		shell:  rowsFn("whole thing"),
		call:   rowsFn("call only"),
		result: rowsFn("result only"),
	}
	r := newPluginRenderers()
	r.RegisterTool(bothHooksRenderer{base})

	ov := r.toolOverrides(ToolExecutionComponent{Name: "calc"}, 40)
	if !ov.hasShell {
		t.Fatalf("shell hook not honoured: %#v", ov)
	}
	if ov.hasCall || ov.hasResult {
		t.Fatalf("shell replacement should subsume the finer hooks: %#v", ov)
	}
	if ov.partial() {
		t.Fatalf("a full shell replacement is not a partial override")
	}
}

// The point of the split: overriding one half inherits the other from the
// built-in rendering.
func TestPartialToolOverridesInheritBuiltIn(t *testing.T) {
	cases := []struct {
		name        string
		renderer    ToolRenderer
		wantPresent []string
		wantAbsent  []string
	}{
		{
			name:        "call only",
			renderer:    callHookRenderer{&hookRenderer{name: "calc", call: rowsFn("PLUGIN CALL LINE")}},
			wantPresent: []string{"PLUGIN CALL LINE", "built-in-output"},
		},
		{
			name:        "result only",
			renderer:    resultHookRenderer{&hookRenderer{name: "calc", result: rowsFn("PLUGIN RESULT LINE")}},
			wantPresent: []string{"PLUGIN RESULT LINE", "calc"},
			wantAbsent:  []string{"built-in-output"},
		},
		{
			name: "both",
			renderer: bothHooksRenderer{&hookRenderer{
				name:   "calc",
				call:   rowsFn("PLUGIN CALL LINE"),
				result: rowsFn("PLUGIN RESULT LINE"),
			}},
			wantPresent: []string{"PLUGIN CALL LINE", "PLUGIN RESULT LINE"},
			wantAbsent:  []string{"built-in-output"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conv := NewConversationModel()
			conv.AddToolCallWithID("t1", "calc", `{"expr":"1+1"}`)
			conv.AddToolResultWithID("t1", "built-in-output", false)

			log := newTestChatLog(t, conv)
			r := newPluginRenderers()
			r.RegisterTool(tc.renderer)
			log.setPluginRenderers(r)

			log.Layout(tauui.Rect{X: 0, Y: 0, W: 60, H: 100})
			waitForChatLog(t, log)

			log.mu.Lock()
			lines := renderLineTexts(log.lines)
			log.mu.Unlock()

			for _, want := range tc.wantPresent {
				if countLinesContaining(lines, want) == 0 {
					t.Fatalf("missing %q in %#v", want, lines)
				}
			}
			for _, absent := range tc.wantAbsent {
				if countLinesContaining(lines, absent) != 0 {
					t.Fatalf("%q should have been overridden: %#v", absent, lines)
				}
			}
		})
	}
}

// A partial override must not silently disable the tool's own chrome for tools
// that have a specialised renderer — it moves them onto the generic shell,
// which is a defined outcome rather than a broken hybrid.
func TestPartialOverrideMovesSpecialisedToolToGenericShell(t *testing.T) {
	conv := NewConversationModel()
	conv.AddToolCallWithID("t1", "bash", `{"command":"ls"}`)
	conv.AddToolResultWithID("t1", "file-a", false)

	log := newTestChatLog(t, conv)
	log.SetLookName("tau")
	r := newPluginRenderers()
	r.RegisterTool(callHookRenderer{&hookRenderer{name: "bash", call: rowsFn("PLUGIN BASH CALL")}})
	log.setPluginRenderers(r)

	log.Layout(tauui.Rect{X: 0, Y: 0, W: 60, H: 100})
	waitForChatLog(t, log)

	log.mu.Lock()
	lines := renderLineTexts(log.lines)
	log.mu.Unlock()

	if countLinesContaining(lines, "PLUGIN BASH CALL") == 0 {
		t.Fatalf("call hook did not apply: %#v", lines)
	}
	if countLinesContaining(lines, "bash · complete") != 0 {
		t.Fatalf("specialised bash border survived a partial override: %#v", lines)
	}
	if countLinesContaining(lines, "file-a") == 0 {
		t.Fatalf("built-in output should still be inherited: %#v", lines)
	}
}

// Full shell replacement still bypasses the tool path entirely.
func TestShellOverrideReplacesWholeToolRendering(t *testing.T) {
	conv := NewConversationModel()
	conv.AddToolCallWithID("t1", "bash", `{"command":"ls"}`)
	conv.AddToolResultWithID("t1", "file-a", false)

	log := newTestChatLog(t, conv)
	log.SetLookName("tau")
	r := newPluginRenderers()
	r.RegisterTool(&hookRenderer{name: "bash", shell: rowsFn("ENTIRELY PLUGIN")})
	log.setPluginRenderers(r)

	log.Layout(tauui.Rect{X: 0, Y: 0, W: 60, H: 100})
	waitForChatLog(t, log)

	log.mu.Lock()
	lines := renderLineTexts(log.lines)
	log.mu.Unlock()

	if countLinesContaining(lines, "ENTIRELY PLUGIN") == 0 {
		t.Fatalf("shell override did not apply: %#v", lines)
	}
	for _, absent := range []string{"file-a", "bash · complete", "$ ls"} {
		if countLinesContaining(lines, absent) != 0 {
			t.Fatalf("%q survived a full shell replacement: %#v", absent, lines)
		}
	}
}
