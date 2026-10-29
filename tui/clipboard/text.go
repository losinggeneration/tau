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
	w := textWriter{platform: runtime.GOOS, getenv: os.Getenv, run: writeCommand, output: windowsPath, emit: emit}
	w.wsl = w.platform == "linux" && isWSL(os.Getenv)
	return w.write(ctx, text)
}

type textWriter struct {
	platform string
	wsl      bool
	tempDir  string
	getenv   func(string) string
	run      func(context.Context, string, string, ...string) error
	output   func(context.Context, string, ...string) ([]byte, error)
	emit     func(string) error
}

// isWSL reports Windows Subsystem for Linux, where Windows executables are
// reachable through interop.
func isWSL(getenv func(string) string) bool {
	if getenv("WSL_DISTRO_NAME") != "" || getenv("WSLENV") != "" {
		return true
	}
	release, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	lower := strings.ToLower(string(release))
	return strings.Contains(lower, "microsoft") || strings.Contains(lower, "wsl")
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
	if !copied && w.wsl {
		// Windows Terminal supports OSC 52, which is faster than the PowerShell round trip.
		if w.getenv("WT_SESSION") != "" && base64.StdEncoding.EncodedLen(len(text)) <= 100000 {
			if err := w.emitOSC52(ctx, text); err != nil {
				return errors.Join(failures, err)
			}
			return nil
		}
		err := w.copyViaWindowsClipboard(ctx, text)
		if err == nil {
			copied = true
		} else {
			failures = errors.Join(failures, err)
		}
	}
	remote := w.getenv("SSH_CONNECTION") != "" || w.getenv("SSH_CLIENT") != "" || w.getenv("MOSH_CONNECTION") != "" || cmp.Or(w.getenv("TAU_CLIPBOARD_OSC52"), w.getenv("PI_CLIPBOARD_OSC52")) == "1"
	headless := w.platform == "linux" && w.getenv("DISPLAY") == "" && w.getenv("WAYLAND_DISPLAY") == "" && w.getenv("TERMUX_VERSION") == ""
	if remote || (!copied && headless) {
		if base64.StdEncoding.EncodedLen(len(text)) <= 100000 {
			if err := w.emitOSC52(ctx, text); err != nil {
				return errors.Join(failures, err)
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

func (w textWriter) emitOSC52(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("copy clipboard: %w", err)
	}
	if err := w.emit("\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07"); err != nil {
		return fmt.Errorf("copy clipboard OSC 52: %w", err)
	}
	return nil
}

// copyViaWindowsClipboard writes the Windows clipboard through interop.
// PowerShell reads a UTF-8 file because clip.exe and PowerShell stdin decode
// piped bytes with the console code page, which mangles non-ASCII text.
func (w textWriter) copyViaWindowsClipboard(ctx context.Context, text string) (err error) {
	f, err := os.CreateTemp(w.tempDir, "tau-wsl-clip-*.txt")
	if err != nil {
		return fmt.Errorf("wsl clipboard: %w", err)
	}
	defer func() { err = errors.Join(err, os.Remove(f.Name())) }()
	_, err = f.WriteString(text)
	if err = errors.Join(err, f.Close()); err != nil {
		return fmt.Errorf("wsl clipboard: %w", err)
	}
	pathCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	out, err := w.output(pathCtx, "wslpath", "-w", f.Name())
	if err != nil {
		return err
	}
	winPath := strings.TrimSpace(string(out))
	if winPath == "" {
		return errors.New("wslpath: empty path")
	}
	script := "Set-Clipboard -Value ([System.IO.File]::ReadAllText('" + strings.ReplaceAll(winPath, "'", "''") + "', [System.Text.Encoding]::UTF8))"
	return w.run(ctx, "powershell.exe", "", "-NoProfile", "-Command", script)
}

func windowsPath(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, ok, err := commandOutput(ctx, name, args...)
	if err == nil && !ok {
		err = fmt.Errorf("%s: %w", name, exec.ErrNotFound)
	}
	return out, err
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
