package tui

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/losinggeneration/rovel"
)

// ProgramStatus describes the root terminal activity record.
type ProgramStatus struct {
	State   string
	App     string
	Kind    string
	Message string
}

var programNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.+-]{1,32}$`)

// FormatProgramStatus encodes an OSC 7501 report with bounded, sanitized text.
func FormatProgramStatus(status ProgramStatus) (string, error) {
	switch status.State {
	case "idle", "working", "blocked", "done", "error", "clear":
	default:
		return "", fmt.Errorf("invalid program status %q", status.State)
	}
	pairs := []string{"state=" + status.State}
	if programNamePattern.MatchString(status.App) {
		pairs = append(pairs, "app="+status.App)
	}
	if status.State == "blocked" && (status.Kind == "permission" || status.Kind == "question" || status.Kind == "auth") {
		pairs = append(pairs, "kind="+status.Kind)
	}
	message := strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, status.Message))
	if len(message) > 2048 {
		end := 2048
		for !utf8.RuneStart(message[end]) {
			end--
		}
		message = message[:end]
	}
	if message != "" {
		pairs = append(pairs, "msg="+base64.StdEncoding.EncodeToString([]byte(message)))
	}
	return "\x1b]7501;" + strings.Join(pairs, ":") + "\x1b\\", nil
}

// WriteProgramStatus emits a report only when terminal support is enabled.
func WriteProgramStatus(writer io.Writer, enabled bool, status ProgramStatus) error {
	if !enabled {
		return nil
	}
	sequence, err := FormatProgramStatus(status)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(writer, sequence); err != nil {
		return fmt.Errorf("write program status: %w", err)
	}
	return nil
}

const programStatusQuery = "\x1b]7501;?\x1b\\\x1b[c"

type programStatusState struct {
	latest  string
	enabled bool
	pending bool
	started bool
}

// SetProgramStatus remembers the latest root status and writes it when support
// is confirmed. Call on the app loop, or before Start.
func (a *App) SetProgramStatus(status ProgramStatus) error {
	sequence, err := FormatProgramStatus(status)
	if err != nil {
		return err
	}
	if status.State == "clear" {
		a.programStatus.latest = ""
	} else {
		a.programStatus.latest = sequence
	}
	if !a.programStatus.started || !a.programStatus.enabled {
		return nil
	}
	return a.writeTerminalControl(sequence)
}

func (a *App) writeTerminalControl(sequence string) error {
	err := a.inner.WriteTerminal(sequence)
	if errors.Is(err, rovel.ErrTerminalTransportUnavailable) {
		return nil
	}
	return err
}

func (a *App) startProgramStatus() error {
	override := os.Getenv("TAU_PROGRAM_STATUS")
	if override == "" {
		override = os.Getenv("PI_PROGRAM_STATUS")
	}
	a.programStatus.started = true
	a.programStatus.enabled = override == "1"
	a.programStatus.pending = override != "1" && override != "0"
	if a.programStatus.pending {
		a.terminalColors.sentinels = append(a.terminalColors.sentinels, nil)
		return a.writeTerminalControl(programStatusQuery)
	}
	if a.programStatus.enabled && a.programStatus.latest != "" {
		return a.writeTerminalControl(a.programStatus.latest)
	}
	return nil
}

func (a *App) stopProgramStatus() error {
	var err error
	if a.programStatus.started && a.programStatus.enabled && a.programStatus.latest != "" {
		err = a.writeTerminalControl("\x1b]7501;state=clear\x1b\\")
	}
	a.programStatus.started = false
	a.programStatus.enabled = false
	a.programStatus.pending = false
	return err
}

func (a *App) handleTerminalReply(reply TerminalReplyEvent) {
	sequence := reply.Sequence
	if a.consumeTerminalColorReply(sequence) {
		return
	}
	if sequence == "\x1b[?997;1n" || sequence == "\x1b[?997;2n" {
		if a.opts.OnTerminalColorScheme != nil {
			scheme := "dark"
			if sequence == "\x1b[?997;2n" {
				scheme = "light"
			}
			a.opts.OnTerminalColorScheme(scheme)
		}
	}
	if strings.HasPrefix(sequence, "\x1b]7501;?") {
		if a.programStatus.pending {
			a.programStatus.pending = false
			a.programStatus.enabled = true
			if a.programStatus.latest != "" {
				if err := a.writeTerminalControl(a.programStatus.latest); err != nil {
					fmt.Fprintf(os.Stderr, "tui: program status: %v\n", err)
				}
			}
		}
		return
	}
	if strings.HasPrefix(sequence, "\x1b[?") && strings.HasSuffix(sequence, "c") {
		a.programStatus.pending = false
		return
	}
	if a.opts.OnTerminalReply != nil {
		a.opts.OnTerminalReply(reply)
	}
}
