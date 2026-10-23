package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// BashCancelReason describes why a bash command was cancelled.
type BashCancelReason string

const (
	BashCancelNone      BashCancelReason = "none"
	BashCancelTimeout   BashCancelReason = "timeout"
	BashCancelUserAbort BashCancelReason = "user_abort"
	BashCancelReload    BashCancelReason = "reload"
)

// Truncation defaults matching TS implementation.
const (
	// DefaultMaxLines is the default max lines for truncation.
	DefaultMaxLines = 2000
	// DefaultMaxBytes is the default max bytes for truncation (50KB).
	DefaultMaxBytes = 50 * 1024
)

// TruncationResult contains details about truncation.
type TruncationResult struct {
	// Content is the truncated content.
	Content string
	// Truncated indicates whether truncation occurred.
	Truncated bool
	// TruncatedBy indicates which limit was hit: "lines", "bytes", or "" if not truncated.
	TruncatedBy string
	// TotalLines is the total number of lines in the original content.
	TotalLines int
	// TotalBytes is the total number of bytes in the original content.
	TotalBytes int
	// OutputLines is the number of complete lines in the truncated output.
	OutputLines int
	// OutputBytes is the number of bytes in the truncated output.
	OutputBytes int
	// LastLinePartial indicates whether the last line was partially truncated.
	LastLinePartial bool
	// MaxLines is the max lines limit that was applied.
	MaxLines int
	// MaxBytes is the max bytes limit that was applied.
	MaxBytes int
}

// BashRequest is the input to a BashOperations call.
type BashRequest struct {
	Command string
	Dir     string
	Timeout time.Duration
	Env     []string
	// OnOutput receives raw output chunks as they arrive. Calls are
	// serialized and finish before Run returns.
	OnOutput func(chunk string)
}

// BashResult is the output of a BashOperations call.
type BashResult struct {
	Output       string
	ExitCode     int
	CancelReason BashCancelReason
	// Truncation contains truncation details when output was truncated.
	Truncation *TruncationResult
	// FullOutputPath is the path to the temp file containing full output
	// when truncation occurred. Only set when BashTempFile is enabled.
	FullOutputPath string
}

// BashOperations is the injection seam for bash execution. Default impl
// wraps os/exec; tests use a mock.
type BashOperations interface {
	Run(ctx context.Context, req BashRequest) (BashResult, error)
}

// DefaultBashOps wraps os/exec.Command.
type DefaultBashOps struct {
	Shell        string
	OutputCap    int64
	KillGrace    time.Duration
	StreamWindow time.Duration
	EnvAllowlist []string
	EnvExtra     map[string]string
	// TempFile enables saving full output to a temp file when truncated.
	TempFile bool
	// MaxLines is the max lines limit for truncation.
	MaxLines int
	// MaxBytes is the max bytes limit for truncation.
	MaxBytes int
}

// NewDefaultBashOps returns a DefaultBashOps with sensible defaults.
func NewDefaultBashOps() *DefaultBashOps {
	shell := "/bin/sh"
	if s := os.Getenv("TAU_SHELL"); s != "" {
		shell = s
	}
	return &DefaultBashOps{
		Shell:        shell,
		OutputCap:    100 * 1024, // 100 KiB
		KillGrace:    2 * time.Second,
		StreamWindow: 100 * time.Millisecond,
		TempFile:     true,
		MaxLines:     DefaultMaxLines,
		MaxBytes:     DefaultMaxBytes,
	}
}

func (d *DefaultBashOps) buildEnv() []string {
	if len(d.EnvAllowlist) > 0 {
		env := make([]string, 0, len(d.EnvAllowlist)+len(d.EnvExtra))
		for _, key := range d.EnvAllowlist {
			if val, ok := os.LookupEnv(key); ok {
				env = append(env, key+"="+val)
			}
		}
		for k, v := range d.EnvExtra {
			env = append(env, k+"="+v)
		}
		return env
	}
	env := os.Environ()
	for k, v := range d.EnvExtra {
		env = append(env, k+"="+v)
	}
	return env
}

func (d *DefaultBashOps) makeCmd(req BashRequest) *exec.Cmd {
	cmd := exec.Command(d.Shell, "-c", req.Command)
	cmd.Dir = req.Dir
	cmd.Stdin = nil
	cmd.Env = d.buildEnv()
	return cmd
}

func (d *DefaultBashOps) Run(ctx context.Context, req BashRequest) (BashResult, error) {
	var cancel context.CancelFunc
	if req.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	if err := ctx.Err(); err != nil {
		reason := BashCancelReload
		if errors.Is(err, context.DeadlineExceeded) {
			reason = BashCancelTimeout
		}
		return BashResult{CancelReason: reason}, nil
	}
	cmd := d.makeCmd(req)

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// One comparable writer for both streams makes exec serialize writes.
	buf := &streamingBuffer{onOutput: req.OnOutput}
	cmd.Stdout = buf
	cmd.Stderr = buf

	if err := cmd.Start(); err != nil {
		return BashResult{Output: err.Error(), ExitCode: 1}, nil
	}

	// Wait for process with cancellation support
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()

	// Set up signal handling for user abort detection
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	var cancelledDueToTimeout bool
	var cancelledDueToSignal bool
	var terminateErr error
	terminate := func(signal syscall.Signal) error { return syscall.Kill(-cmd.Process.Pid, signal) }
	select {
	case <-done:
	case <-sigChan:
		cancelledDueToSignal = true
		terminateErr = terminateBashProcess(done, d.KillGrace, terminate)
	case <-ctx.Done():
		cancelledDueToTimeout = errors.Is(ctx.Err(), context.DeadlineExceeded)
		terminateErr = terminateBashProcess(done, d.KillGrace, terminate)
	}

	result := BashResult{Output: buf.buf.String(), CancelReason: BashCancelNone}

	if cancelledDueToTimeout {
		result.CancelReason = BashCancelTimeout
	} else if cancelledDueToSignal {
		result.CancelReason = BashCancelUserAbort
	} else if ctx.Err() != nil {
		result.CancelReason = BashCancelReload
	}

	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		}
	}

	// Sanitize the captured output (strip ANSI, normalize CR, remove lone
	// surrogates, replace binary garbage) before truncation so the cap is
	// measured against the cleaned text the model will see.
	output := SanitizeBashOutput(result.Output)
	result.Output = output

	// Apply tail truncation with temp file support
	truncatedContent, truncation := d.truncateTail(output)

	if truncation != nil {
		result.Truncation = truncation
		result.Output = truncatedContent

		// Write full output to temp file if enabled
		if d.TempFile && truncation.Truncated {
			if tempPath, err := writeTempFile(output); err == nil {
				result.FullOutputPath = tempPath
				// Add full output reference to the truncated content
				startLine := truncation.TotalLines - truncation.OutputLines + 1
				endLine := truncation.TotalLines
				result.Output += fmt.Sprintf(
					"\n\n[Showing lines %d-%d of %d. Full output: %s]",
					startLine, endLine, truncation.TotalLines, tempPath,
				)
			}
		}
	} else {
		result.Output = truncateOutput(output, d.OutputCap)
	}

	return result, terminateErr
}

// ponytail: holds the full output in memory before tail truncation; spill to
// the temp file past MaxBytes if huge command output becomes a memory problem.
type streamingBuffer struct {
	buf      bytes.Buffer
	onOutput func(string)
}

func (b *streamingBuffer) Write(p []byte) (int, error) {
	if b.onOutput != nil {
		b.onOutput(string(p))
	}
	return b.buf.Write(p)
}

func terminateBashProcess(done <-chan struct{}, grace time.Duration, kill func(syscall.Signal) error) error {
	var result error
	if err := kill(syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		result = fmt.Errorf("terminate shell process group: %w", err)
	}
	timer := time.NewTimer(max(0, grace))
	defer timer.Stop()
	select {
	case <-done:
		return result
	case <-timer.C:
		if err := kill(syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			result = errors.Join(result, fmt.Errorf("kill shell process group: %w", err))
		}
		<-done
		return result
	}
}

func truncateOutput(output string, cap int64) string {
	if int64(len(output)) <= cap {
		return output
	}
	truncated := output[:cap]
	total := len(output)
	return truncated + fmt.Sprintf("\n... [truncated, %d bytes total]", total)
}

// truncateTail truncates content from the tail (keep last N lines/bytes).
// This matches the TS truncateTail behavior.
func (d *DefaultBashOps) truncateTail(content string) (string, *TruncationResult) {
	maxLines := d.MaxLines
	if maxLines <= 0 {
		maxLines = DefaultMaxLines
	}
	maxBytes := d.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}

	totalBytes := len(content)
	lines := strings.Split(content, "\n")
	totalLines := len(lines)

	// Check if no truncation needed
	if totalLines <= maxLines && totalBytes <= maxBytes {
		return content, nil
	}

	// Work backwards from the end
	var outputLines []string
	outputBytesCount := 0
	truncatedBy := "lines"
	lastLinePartial := false

	for i := len(lines) - 1; i >= 0 && len(outputLines) < maxLines; i-- {
		line := lines[i]
		lineBytes := len(line)
		if len(outputLines) > 0 {
			lineBytes++ // +1 for newline
		}

		if outputBytesCount+lineBytes > maxBytes {
			truncatedBy = "bytes"
			// Edge case: if we haven't added ANY lines yet and this line exceeds maxBytes,
			// take the end of the line (partial)
			if len(outputLines) == 0 {
				truncatedLine := truncateStringToBytesFromEnd(line, maxBytes)
				outputLines = append([]string{truncatedLine}, outputLines...)
				outputBytesCount = len(truncatedLine)
				lastLinePartial = true
			}
			break
		}

		outputLines = append([]string{line}, outputLines...)
		outputBytesCount += lineBytes
	}

	// If we exited due to line limit
	if len(outputLines) >= maxLines && outputBytesCount <= maxBytes {
		truncatedBy = "lines"
	}

	outputContent := strings.Join(outputLines, "\n")
	finalOutputBytes := len(outputContent)

	return outputContent, &TruncationResult{
		Content:         outputContent,
		Truncated:       true,
		TruncatedBy:     truncatedBy,
		TotalLines:      totalLines,
		TotalBytes:      totalBytes,
		OutputLines:     len(outputLines),
		OutputBytes:     finalOutputBytes,
		LastLinePartial: lastLinePartial,
		MaxLines:        maxLines,
		MaxBytes:        maxBytes,
	}
}

// truncateStringToBytesFromEnd truncates a string to fit within a byte limit (from the end).
// Handles multi-byte UTF-8 characters correctly.
func truncateStringToBytesFromEnd(str string, maxBytes int) string {
	buf := []byte(str)
	if len(buf) <= maxBytes {
		return str
	}

	// Start from the end, skip maxBytes back
	start := len(buf) - maxBytes

	// Find a valid UTF-8 boundary (start of a character)
	for start < len(buf) && (buf[start]&0xc0) == 0x80 {
		start++
	}

	return string(buf[start:])
}

// getTempFilePath generates a unique temp file path for bash output.
func getTempFilePath() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return filepath.Join(os.TempDir(), fmt.Sprintf("tau-bash-%x.log", b))
}

// writeTempFile writes content to a temp file and returns the path.
func writeTempFile(content string) (string, error) {
	path := getTempFilePath()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	return path, nil
}
