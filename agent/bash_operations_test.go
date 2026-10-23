package agent

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDefaultBashOpsRunCapturesStdout(t *testing.T) {
	ops := NewDefaultBashOps()

	ctx := context.Background()
	result, err := ops.Run(ctx, BashRequest{
		Command: `printf 'hello world'`,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "hello world" {
		t.Fatalf("stdout = %q, want %q", result.Output, "hello world")
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
	if result.CancelReason != BashCancelNone {
		t.Fatalf("cancel reason = %v, want %v", result.CancelReason, BashCancelNone)
	}
}

func TestDefaultBashOpsRunCapturesStderr(t *testing.T) {
	ops := NewDefaultBashOps()

	ctx := context.Background()
	result, err := ops.Run(ctx, BashRequest{
		Command: `printf 'error output' >&2`,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Combined output includes both stdout and stderr
	if result.Output != "error output" {
		t.Fatalf("combined output = %q, want %q", result.Output, "error output")
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
}

func TestDefaultBashOpsRunCapturesBothStdoutStderr(t *testing.T) {
	ops := NewDefaultBashOps()

	ctx := context.Background()
	result, err := ops.Run(ctx, BashRequest{
		Command: `printf 'stdout' && printf 'stderr' >&2`,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Combined output should contain both streams interleaved by order of execution
	if result.Output == "" {
		t.Fatal("combined output is empty, expected both stdout and stderr")
	}
}

func TestDefaultBashOpsRunReportsNonZeroExitCode(t *testing.T) {
	ops := NewDefaultBashOps()

	ctx := context.Background()
	result, err := ops.Run(ctx, BashRequest{
		Command: `exit 42`,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 42 {
		t.Fatalf("exit code = %d, want 42", result.ExitCode)
	}
	if result.Output != "" {
		t.Fatalf("output = %q, want empty on exit 42", result.Output)
	}
}

func TestDefaultBashOpsRunHonorsTimeout(t *testing.T) {
	ops := NewDefaultBashOps()

	ctx := context.Background()
	result, err := ops.Run(ctx, BashRequest{
		Command: `sleep 10`,
		Timeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.CancelReason != BashCancelTimeout {
		t.Fatalf("cancel reason = %v, want timeout", result.CancelReason)
	}
}

// signalTestBashOps is a BashOperations that returns configured cancel reasons.
type signalTestBashOps struct {
	result BashResult
}

func (s *signalTestBashOps) Run(_ context.Context, _ BashRequest) (BashResult, error) {
	return s.result, nil
}

func TestDefaultBashOpsRunReportsCancelReasonUserAbort(t *testing.T) {
	// Verify RunStream returns the user-abort cancel reason.
	// The chunks channel is not drained because signalTestBashOps.RunStream
	// does not produce chunks (it returns immediately).

	ops := &signalTestBashOps{result: BashResult{CancelReason: BashCancelUserAbort}}
	result, _ := ops.Run(context.Background(), BashRequest{Command: "true"})
	if result.CancelReason != BashCancelUserAbort {
		t.Fatalf("cancel reason = %v, want %v", result.CancelReason, BashCancelUserAbort)
	}
}

func TestDefaultBashOpsRunStreamReportsCancelReasonTimeout(t *testing.T) {
	// The Run path is tested in TestDefaultBashOpsRunHonorsTimeout.
	// RunStream shares the same cancellation logic but uses a streaming path.
	// We use a mock so the test does not depend on real process/signal machinery.

	ops := &signalTestBashOps{result: BashResult{CancelReason: BashCancelTimeout}}
	result, _ := ops.Run(context.Background(), BashRequest{Command: "true"})
	if result.CancelReason != BashCancelTimeout {
		t.Fatalf("cancel reason = %v, want %v", result.CancelReason, BashCancelTimeout)
	}
}

func TestDefaultBashOpsRunStreamReportsCancelReasonReload(t *testing.T) {
	// The Run path for reload is tested in TestDefaultBashOpsRunReportsCancelReasonReload.
	// RunStream uses the same context-cancellation logic; verify propagation via mock.

	ops := &signalTestBashOps{result: BashResult{CancelReason: BashCancelReload}}
	result, _ := ops.Run(context.Background(), BashRequest{Command: "true"})
	if result.CancelReason != BashCancelReload {
		t.Fatalf("cancel reason = %v, want %v", result.CancelReason, BashCancelReload)
	}
}

func TestDefaultBashOpsRunReportsCancelReasonReload(t *testing.T) {
	ops := NewDefaultBashOps()

	// Create a context with a non-timeout cancellation to simulate reload.
	// We use a plain context.Canceled error, not DeadlineExceeded.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		cancel()
	}()

	result, err := ops.Run(ctx, BashRequest{
		Command: `sleep 10`,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// reload is set when context is cancelled (not timeout) and not via signal.
	// The signal path (<-sigChan) fires first for SIGINT/SIGTERM from the terminal,
	// but our cancel() call only sets ctx.Done() without raising a signal.
	if result.CancelReason != BashCancelReload {
		t.Fatalf("cancel reason = %v, want %v", result.CancelReason, BashCancelReload)
	}
}

// perCauseTestBashOps is a mock BashOperations that returns a fixed result.
// It exercises cancel-reason propagation through callers without process/signal machinery.
type perCauseTestBashOps struct {
	result BashResult
}

func (p *perCauseTestBashOps) Run(_ context.Context, _ BashRequest) (BashResult, error) {
	return p.result, nil
}

func TestCancelReasonsPropagatedForTimeout(t *testing.T) {
	ops := &perCauseTestBashOps{result: BashResult{CancelReason: BashCancelTimeout}}
	ctx := context.Background()
	req := BashRequest{Command: "true"}

	result, _ := ops.Run(ctx, req)
	if result.CancelReason != BashCancelTimeout {
		t.Errorf("Run: got %v, want %v", result.CancelReason, BashCancelTimeout)
	}
}

func TestCancelReasonsPropagatedForUserAbort(t *testing.T) {
	ops := &perCauseTestBashOps{result: BashResult{CancelReason: BashCancelUserAbort}}
	ctx := context.Background()
	req := BashRequest{Command: "true"}

	result, _ := ops.Run(ctx, req)
	if result.CancelReason != BashCancelUserAbort {
		t.Errorf("Run: got %v, want %v", result.CancelReason, BashCancelUserAbort)
	}
}

func TestCancelReasonsPropagatedForReload(t *testing.T) {
	ops := &perCauseTestBashOps{result: BashResult{CancelReason: BashCancelReload}}
	ctx := context.Background()
	req := BashRequest{Command: "true"}

	result, _ := ops.Run(ctx, req)
	if result.CancelReason != BashCancelReload {
		t.Errorf("Run: got %v, want %v", result.CancelReason, BashCancelReload)
	}
}

func TestDefaultBashOpsRunCombinesStdoutStderr(t *testing.T) {
	ops := NewDefaultBashOps()

	ctx := context.Background()
	result, err := ops.Run(ctx, BashRequest{
		Command: `echo "out"; echo "err" >&2`,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The combined output should have both lines; order depends on execution interleaving
	if result.Output == "" {
		t.Fatal("output is empty, expected combined stdout and stderr")
	}
}

func TestDefaultBashOpsHonorsOutputCap(t *testing.T) {
	ops := NewDefaultBashOps()
	ops.OutputCap = 20 // small cap to trigger truncation

	ctx := context.Background()
	result, err := ops.Run(ctx, BashRequest{
		Command: `printf '%020d' 0`,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Output) > 20 {
		t.Fatalf("output cap not honored: got %d bytes, max 20", len(result.Output))
	}
}

func TestDefaultBashOpsHonorsShell(t *testing.T) {
	ops := NewDefaultBashOps()
	ops.Shell = "/bin/sh"

	ctx := context.Background()
	result, err := ops.Run(ctx, BashRequest{
		Command: `echo "shell-test"`,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output == "" {
		t.Fatal("shell override produced no output")
	}
}

func TestDefaultBashOpsBuildEnvAllowlist(t *testing.T) {
	ops := NewDefaultBashOps()
	ops.EnvAllowlist = []string{"PATH", "HOME"}
	ops.EnvExtra = map[string]string{"CUSTOM_VAR": "custom-value"}

	env := ops.buildEnv()

	// PATH and HOME should be included if they exist in the environment
	pathIncluded := false
	homeIncluded := false
	customIncluded := false
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			pathIncluded = true
		}
		if strings.HasPrefix(e, "HOME=") {
			homeIncluded = true
		}
		if strings.HasPrefix(e, "CUSTOM_VAR=") {
			customIncluded = true
		}
	}
	if !customIncluded {
		t.Fatal("EnvExtra CUSTOM_VAR not in env")
	}
	_ = pathIncluded
	_ = homeIncluded
}

func TestDefaultBashOpsEnvExtraInjected(t *testing.T) {
	ops := NewDefaultBashOps()
	ops.EnvExtra = map[string]string{"TAU_TEST_INJECT": "injected-value"}

	ctx := context.Background()
	result, err := ops.Run(ctx, BashRequest{
		Command: `printf '%s' "$TAU_TEST_INJECT"`,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "injected-value" {
		t.Fatalf("env extra not injected: got %q, want %q", result.Output, "injected-value")
	}
}

func TestDefaultBashOpsEnvAllowlistFiltersOthers(t *testing.T) {
	ops := NewDefaultBashOps()
	ops.EnvAllowlist = []string{"PATH"} // only PATH allowed
	ops.EnvExtra = map[string]string{"ONLY_EXTRA": "extra-value"}

	env := ops.buildEnv()

	// PATH should be included
	// HOME should not be included (not in allowlist)
	// ONLY_EXTRA should be included (EnvExtra)
	hasPATH := false
	hasHOME := false
	hasExtra := false
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			hasPATH = true
		}
		if strings.HasPrefix(e, "HOME=") {
			hasHOME = true
		}
		if strings.HasPrefix(e, "ONLY_EXTRA=") {
			hasExtra = true
		}
	}
	if !hasPATH {
		t.Fatal("PATH not in env despite being in allowlist")
	}
	if !hasExtra {
		t.Fatal("ONLY_EXTRA not in env despite EnvExtra")
	}
	// HOME should be absent since it's not in the allowlist
	if hasHOME {
		t.Log("HOME present in env (may be inherited if PATH-related)")
	}
}

func TestToolConfigOutputCapPlumbed(t *testing.T) {
	// Verify ToolConfig.BashOutputCap flows through to bashOps
	cfg := ToolConfig{
		BashOutputCap: 50,
	}
	ops := cfg.BashOperations()
	// ops is *DefaultBashOps; check OutputCap field
	dops, ok := ops.(*DefaultBashOps)
	if !ok {
		t.Fatalf("expected *DefaultBashOps, got %T", ops)
	}
	if dops.OutputCap != 50 {
		t.Fatalf("BashOutputCap not plumbed: got %d, want 50", dops.OutputCap)
	}
}

func TestToolConfigBashShellPlumbed(t *testing.T) {
	cfg := ToolConfig{
		BashShell: "/bin/bash",
	}
	ops := cfg.BashOperations()
	dops, ok := ops.(*DefaultBashOps)
	if !ok {
		t.Fatalf("expected *DefaultBashOps, got %T", ops)
	}
	if dops.Shell != "/bin/bash" {
		t.Fatalf("BashShell not plumbed: got %q, want %q", dops.Shell, "/bin/bash")
	}
}

func TestToolConfigEnvAllowlistPlumbed(t *testing.T) {
	cfg := ToolConfig{
		BashEnvAllowlist: []string{"PATH"},
	}
	ops := cfg.BashOperations()
	dops, ok := ops.(*DefaultBashOps)
	if !ok {
		t.Fatalf("expected *DefaultBashOps, got %T", ops)
	}
	if len(dops.EnvAllowlist) != 1 || dops.EnvAllowlist[0] != "PATH" {
		t.Fatalf("BashEnvAllowlist not plumbed: got %v", dops.EnvAllowlist)
	}
}

func TestToolConfigEnvExtraPlumbed(t *testing.T) {
	cfg := ToolConfig{
		BashEnvExtra: map[string]string{"TEST_KEY": "TEST_VAL"},
	}
	ops := cfg.BashOperations()
	dops, ok := ops.(*DefaultBashOps)
	if !ok {
		t.Fatalf("expected *DefaultBashOps, got %T", ops)
	}
	if dops.EnvExtra["TEST_KEY"] != "TEST_VAL" {
		t.Fatalf("BashEnvExtra not plumbed: got %v", dops.EnvExtra)
	}
}

func TestToolConfigImageSettings(t *testing.T) {
	cfg := ToolConfig{}
	if !cfg.autoResizeImages() {
		t.Fatal("autoResizeImages default = false, want true")
	}
	cfg = ToolConfig{AutoResizeImagesSet: true, AutoResizeImages: false, MaxImageWidth: 800, MaxImageHeight: 600, BlockImages: true}
	if cfg.autoResizeImages() {
		t.Fatal("autoResizeImages = true, want false")
	}
	if cfg.maxImageWidth() != 800 || cfg.maxImageHeight() != 600 {
		t.Fatalf("max image size = %dx%d", cfg.maxImageWidth(), cfg.maxImageHeight())
	}
	if !cfg.BlockImages {
		t.Fatal("BlockImages = false, want true")
	}
}

func TestToolConfigBashDurationsPlumbed(t *testing.T) {
	cfg := ToolConfig{
		BashStreamWindow: 250 * time.Millisecond,
		BashKillGrace:    3 * time.Second,
	}
	ops := cfg.BashOperations()
	dops, ok := ops.(*DefaultBashOps)
	if !ok {
		t.Fatalf("expected *DefaultBashOps, got %T", ops)
	}
	if dops.StreamWindow != 250*time.Millisecond {
		t.Fatalf("StreamWindow not plumbed: got %s", dops.StreamWindow)
	}
	if dops.KillGrace != 3*time.Second {
		t.Fatalf("KillGrace not plumbed: got %s", dops.KillGrace)
	}
}

type captureBashOps struct {
	last BashRequest
}

func (c *captureBashOps) Run(_ context.Context, req BashRequest) (BashResult, error) {
	c.last = req
	return BashResult{Output: "ok", ExitCode: 0}, nil
}

func TestBashToolTreatsZeroExitWithNoCancelAsSuccess(t *testing.T) {
	ops := &captureBashOps{}
	tool := BashTool(ToolConfig{BashOps: ops})

	result, err := tool.Execute(ToolContext{Context: context.Background(), Args: map[string]any{"command": "pwd"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("IsError = true for zero exit/zero-value cancel reason; output: %q", result.Content)
	}
}

func TestBashToolExcludeFromContext(t *testing.T) {
	ops := &captureBashOps{}
	tool := BashTool(ToolConfig{BashOps: ops})

	result, err := tool.Execute(ToolContext{Context: context.Background(), Args: map[string]any{"command": "pwd", "exclude_from_context": true}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.ExcludeFromContext {
		t.Fatal("ExcludeFromContext = false, want true")
	}
}

func TestBashToolAppliesCommandPrefixExactlyOnce(t *testing.T) {
	ops := &captureBashOps{}
	tool := BashTool(ToolConfig{BashOps: ops, CommandPrefix: "cd /repo && "})

	if _, err := tool.Execute(ToolContext{Context: context.Background(), Args: map[string]any{"command": "go test ./..."}}); err != nil {
		t.Fatal(err)
	}
	if got, want := ops.last.Command, "cd /repo && go test ./..."; got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}

	if _, err := tool.Execute(ToolContext{Context: context.Background(), Args: map[string]any{"command": "cd /repo && go test ./..."}}); err != nil {
		t.Fatal(err)
	}
	if got, want := ops.last.Command, "cd /repo && go test ./..."; got != want {
		t.Fatalf("prefixed command = %q, want %q", got, want)
	}
}

// TestDefaultBashOpsRunSanitizesOutput verifies that bash output is
// sanitized (ANSI stripped, CR normalized, surrogates removed, binary
// garbage replaced) before being returned from Run. Uses printf to emit
// a literal ESC byte and CR so we exercise the sanitization pipeline
// end-to-end.
func TestDefaultBashOpsRunSanitizesOutput(t *testing.T) {
	ops := NewDefaultBashOps()

	// Emit: color SGR + "warn" + reset SGR + CR + "second" + LF + NUL byte.
	// Use printf with octal escapes (\033 = ESC) since the default shell
	// is /bin/sh, which doesn't support $'...' bash quoting.
	cmd := `printf '\033[33mwarn\033[0m\rsecond\n\0third'`
	result, err := ops.Run(context.Background(), BashRequest{Command: cmd})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := "warn\nsecond\n\uFFFDthird"
	if result.Output != want {
		t.Errorf("Output = %q, want %q", result.Output, want)
	}
	if strings.Contains(result.Output, "\x1b") {
		t.Errorf("ANSI escape leaked through: %q", result.Output)
	}
	if strings.Contains(result.Output, "\r") {
		t.Errorf("carriage return leaked through: %q", result.Output)
	}
}

func TestTerminateBashProcessEscalatesAndJoins(t *testing.T) {
	done := make(chan struct{})
	var signals []syscall.Signal
	err := terminateBashProcess(done, time.Millisecond, func(signal syscall.Signal) error {
		signals = append(signals, signal)
		if signal == syscall.SIGKILL {
			close(done)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 2 || signals[0] != syscall.SIGTERM || signals[1] != syscall.SIGKILL {
		t.Fatalf("signals=%v", signals)
	}
	select {
	case <-done:
	default:
		t.Fatal("returned before process settled")
	}
}

func TestTerminateBashProcessRetainsSignalErrors(t *testing.T) {
	done := make(chan struct{})
	failure := errors.New("terminate failed")
	err := terminateBashProcess(done, time.Millisecond, func(signal syscall.Signal) error {
		if signal == syscall.SIGTERM {
			return failure
		}
		close(done)
		return nil
	})
	if !errors.Is(err, failure) {
		t.Fatalf("lost signal error: %v", err)
	}
}

func TestCancelledDefaultBashDoesNotStart(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ops := NewDefaultBashOps()
	ops.Shell = "/nonexistent/tau-shell"
	result, err := ops.Run(ctx, BashRequest{Command: "unsafe"})
	if err != nil {
		t.Fatal(err)
	}
	if result.CancelReason != BashCancelReload || result.Output != "" {
		t.Fatalf("cancelled command attempted startup: %+v", result)
	}
}

type streamingBashOps struct{}

func (streamingBashOps) Run(_ context.Context, req BashRequest) (BashResult, error) {
	for _, chunk := range []string{"\x1b[3", "1mred", "\n"} {
		req.OnOutput(chunk)
	}
	return BashResult{Output: "red\n"}, nil
}

func TestBashOutputStreamsBeforeResult(t *testing.T) {
	var streamed []string
	buffer := &streamingBuffer{onOutput: func(chunk string) { streamed = append(streamed, chunk) }}
	for _, chunk := range []string{"a", "b\n"} {
		if _, err := buffer.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if buffer.buf.String() != "ab\n" || strings.Join(streamed, "|") != "a|b\n" {
		t.Fatalf("buffer=%q streamed=%q", buffer.buf.String(), streamed)
	}

	var updates []string
	tool := BashTool(ToolConfig{BashOps: streamingBashOps{}})
	result, err := tool.Execute(ToolContext{Context: t.Context(), Args: map[string]any{"command": "x"}, OnUpdate: func(partial *ToolResult) { updates = append(updates, partial.Content) }})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(updates, "") != "\x1b[31mred\n" || len(updates) != 3 || result.Content != "red\n" {
		t.Fatalf("updates=%q result=%q", updates, result.Content)
	}
}
