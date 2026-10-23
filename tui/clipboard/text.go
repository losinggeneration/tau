package clipboard

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// WriteText copies locally before emitting OSC 52 for remote or display-less
// sessions. emit must write and flush through the active terminal transport.
func WriteText(ctx context.Context, text string, emit func(string) error) error {
	return (textWriter{platform: runtime.GOOS, getenv: os.Getenv, run: writeCommand, emit: emit}).write(ctx, text)
}

type textWriter struct {
	platform string
	getenv   func(string) string
	run      func(context.Context, string, string, ...string) error
	emit     func(string) error
}

func (w textWriter) write(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("copy clipboard: %w", err)
	}
	var commands [][]string
	switch w.platform {
	case "darwin":
		commands = append(commands, []string{"pbcopy"})
	case "windows":
		commands = append(commands, []string{"clip"})
	default:
		if w.getenv("TERMUX_VERSION") != "" {
			commands = append(commands, []string{"termux-clipboard-set"})
		}
		if w.getenv("WAYLAND_DISPLAY") != "" {
			commands = append(commands, []string{"wl-copy"})
		}
		if w.getenv("DISPLAY") != "" {
			commands = append(commands, []string{"xclip", "-selection", "clipboard"}, []string{"xsel", "--clipboard", "--input"})
		}
	}
	var failures error
	var copied bool
	for _, command := range commands {
		err := w.run(ctx, command[0], text, command[1:]...)
		if err == nil {
			copied = true
			break
		}
		failures = errors.Join(failures, err)
		if ctx.Err() != nil {
			return fmt.Errorf("copy clipboard: %w", errors.Join(failures, ctx.Err()))
		}
	}
	remote := w.getenv("SSH_CONNECTION") != "" || w.getenv("SSH_CLIENT") != "" || w.getenv("MOSH_CONNECTION") != "" || cmp.Or(w.getenv("TAU_CLIPBOARD_OSC52"), w.getenv("PI_CLIPBOARD_OSC52")) == "1"
	headless := w.platform == "linux" && w.getenv("DISPLAY") == "" && w.getenv("WAYLAND_DISPLAY") == "" && w.getenv("TERMUX_VERSION") == ""
	if remote || (!copied && headless) {
		if base64.StdEncoding.EncodedLen(len(text)) <= 100000 {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("copy clipboard: %w", err)
			}
			if err := w.emit("\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07"); err != nil {
				return fmt.Errorf("copy clipboard OSC 52: %w", errors.Join(failures, err))
			}
			return nil
		}
		if !copied {
			return fmt.Errorf("clipboard unavailable: text exceeds the OSC 52 size limit: %w", errors.Join(failures, errors.New("OSC 52 payload too large")))
		}
	}
	if copied {
		return nil
	}
	hint := "clipboard unavailable"
	switch {
	case w.getenv("TERMUX_VERSION") != "":
		hint += ": install the Termux:API app and `termux-api` package"
	case w.platform == "linux" && w.getenv("WAYLAND_DISPLAY") != "":
		hint += ": install `wl-clipboard` (`wl-copy`) or check Wayland access"
	case w.platform == "linux" && w.getenv("DISPLAY") != "":
		hint += ": install `xclip` or `xsel`, or check X11 access"
	}
	if failures == nil {
		return errors.New(hint)
	}
	return fmt.Errorf("%s: %w", hint, failures)
}

func writeCommand(ctx context.Context, name, text string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(text)
	// Clipboard helpers can daemonize; inherited output pipes would delay Wait.
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, errors.Join(err, ctx.Err()))
	}
	return nil
}
