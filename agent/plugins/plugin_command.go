package plugins

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func resolveRunCommandPath(pkgDir, runCommand string) string {
	fields := strings.Fields(strings.TrimSpace(runCommand))
	if len(fields) == 0 {
		return ""
	}

	candidate := fields[0]
	if strings.HasPrefix(candidate, "./") || strings.HasPrefix(candidate, "../") {
		candidate = filepath.Join(pkgDir, candidate)
	}
	if !filepath.IsAbs(candidate) {
		// Bare interpreter name (e.g. "node"): resolve via PATH so hashing
		// and launch validation work for run commands like
		// "node /abs/shim/index.js entry.js".
		resolved, err := exec.LookPath(candidate)
		if err != nil {
			return ""
		}
		candidate = resolved
	}

	info, err := os.Stat(candidate)
	if err != nil || info.IsDir() {
		return ""
	}
	return candidate
}

// shellMetachars are the characters that force the sh -c fallback. Simple
// launcher lines (interpreter + args, possibly quoted paths) exec directly
// so the plugin process receives signals — with sh -c, sh forks the real
// process without exec'ing, orphaning it on SIGTERM and keeping the stdout
// pipe open forever (go-plugin's Kill then blocks). Quotes are NOT in this
// set: splitCommand tokenizes quoted segments itself.
const shellMetachars = "|&;<>()$`\\*?[]#~%{}\n\t!"

// splitCommand tokenizes a simple command line: whitespace-separated with
// single- or double-quoted segments (no escapes beyond closing quotes).
// Returns nil when the line uses shell features this tokenizer can't model.
func splitCommand(line string) []string {
	var fields []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	flush := func() {
		if cur.Len() > 0 {
			fields = append(fields, cur.String())
			cur.Reset()
		}
	}
	for _, r := range line {
		switch {
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case (r == ' ' || r == '\t') && !inSingle && !inDouble:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	if inSingle || inDouble {
		return nil
	}
	return fields
}

// Launch wrapper scripts. go-plugin unconditionally assigns the host's
// stdin to the plugin process (cmd.Stdin = os.Stdin), and inside the TUI that
// fd is the terminal. A plugin — or any process an extension spawns from it —
// that reads or reconfigures that fd steals keystrokes from the UI and can
// leave the tty cooked and echoing (a wedged TUI after /reload killed a
// plugin tree that still held the terminal). Both scripts redirect stdin to
// /dev/null and use exec so the wrapper shell replaces itself rather than
// forking, keeping signal delivery to the plugin direct.
const (
	pluginDirectScript = `exec "$0" "$@" < /dev/null`
	pluginShellScript  = `exec sh -c "$1" "$2" < /dev/null`
)

// resolveLaunchBinary resolves a bare interpreter name via PATH so a missing
// binary fails at launch construction instead of as a handshake timeout.
// Paths containing a slash are returned unchanged; sh resolves them against
// the command's working directory.
func resolveLaunchBinary(bin string) (string, error) {
	if strings.ContainsRune(bin, '/') {
		return bin, nil
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("plugin run command %q: %w", bin, err)
	}
	return resolved, nil
}

// buildPluginCmd constructs the exec.Cmd used to launch a plugin, honoring
// the manifest's [run].command when present and injecting TAU_HOST_SOCKET so
// the plugin can dial the Host service. Used by both the discovery-time
// loadLocked path and the CLI-driven Handshake path so they share launch
// semantics (plugin path as $1, cwd = manifest dir).
//
// Every shape launches through an sh -c wrapper that redirects stdin to
// /dev/null (see the script constants above) while exec'ing the real command
// so signals still reach the plugin process.
//
// hostSocket is the path to the Host service unix socket; empty means no
// Host service is available (the env var is not set).
func buildPluginCmd(path string, manifest *Manifest, manifestPath, hostSocket string) (*exec.Cmd, error) {
	dir := filepath.Dir(path)
	if manifestPath != "" {
		dir = filepath.Dir(manifestPath)
	}

	withEnv := func(cmd *exec.Cmd) *exec.Cmd {
		cmd.Dir = dir
		if hostSocket != "" {
			cmd.Env = append(cmd.Env, "TAU_HOST_SOCKET="+hostSocket)
		}
		return cmd
	}

	if manifest != nil && manifest.HasRunCommand() {
		runCmd := manifest.Run.Command
		if !strings.ContainsAny(runCmd, shellMetachars) {
			if fields := splitCommand(runCmd); len(fields) > 0 {
				bin, err := resolveLaunchBinary(fields[0])
				if err != nil {
					return nil, err
				}
				// Direct exec: the plugin process gets signals directly.
				wrapperArgs := append([]string{"-c", pluginDirectScript, bin}, fields[1:]...)
				return withEnv(exec.Command("sh", wrapperArgs...)), nil
			}
		}
		// Shell fallback for complex commands. The plugin package dir is
		// passed as $0 for scripts that want it.
		return withEnv(exec.Command("sh", "-c", pluginShellScript, "sh", runCmd, path)), nil
	}
	return withEnv(exec.Command("sh", "-c", pluginDirectScript, path)), nil
}
