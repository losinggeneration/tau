package plugins

import (
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

// buildPluginCmd constructs the exec.Cmd used to launch a plugin, honoring
// the manifest's [run].command when present and injecting TAU_HOST_SOCKET so
// the plugin can dial the Host service. Used by both the discovery-time
// loadLocked path and the CLI-driven Handshake path so they share launch
// semantics (plugin path as $1, cwd = manifest dir).
//
// Simple launcher lines (e.g. "node /abs/shim/index.js entry.js") exec the
// interpreter directly so signals reach the plugin process. Lines with shell
// metacharacters fall back to sh -c.
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
				// Direct exec: the plugin process gets signals directly.
				return withEnv(exec.Command(fields[0], fields[1:]...)), nil
			}
		}
		// Shell fallback for complex commands. The plugin package dir is
		// passed as $1 for scripts that want it.
		cmd := exec.Command("sh", "-c", runCmd)
		cmd.Args = append(cmd.Args, path)
		return withEnv(cmd), nil
	}
	return withEnv(exec.Command(path)), nil
}
