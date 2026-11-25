package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sessionpkg "github.com/losinggeneration/tau/agent/session"
	"github.com/losinggeneration/tau/rpc/client"
	"github.com/losinggeneration/tau/rpc/coord"
	"github.com/losinggeneration/tau/rpc/server"
	"github.com/losinggeneration/tau/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/losinggeneration/tau/agent/artifacts"
	"github.com/losinggeneration/tau/internal/app/tui"
	"github.com/urfave/cli/v3"
	"golang.org/x/sys/unix"
)

func TestFindInteractiveCoordinator(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "socket")
	c, err := findInteractiveCoordinator(t.Context(), path)
	if c != nil || err != nil {
		t.Fatalf("missing socket: client=%v err=%v", c, err)
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	peerDone := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err == nil {
			// A running but incompatible daemon must not trigger fallback.
			err = conn.Close()
		}
		peerDone <- err
	}()
	c, err = findInteractiveCoordinator(t.Context(), path)
	if c != nil || err == nil {
		t.Errorf("incompatible listener: client=%v err=%v", c, err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-peerDone; err != nil {
		t.Fatal(err)
	}
	c, err = findInteractiveCoordinator(t.Context(), path)
	if c != nil || err != nil {
		t.Fatalf("stale socket: client=%v err=%v", c, err)
	}
}

func TestStandaloneAliases(t *testing.T) {
	for _, flag := range []string{"--standalone", "-so"} {
		t.Run(flag, func(t *testing.T) {
			var reached bool
			cmd := newRootCommand()
			cmd.Action = func(_ context.Context, cmd *cli.Command) error {
				reached = true
				if !cmd.Bool("standalone") || len(cmd.StringSlice("message")) != 1 || cmd.StringSlice("message")[0] != "test prompt" {
					t.Errorf("standalone=%t message=%q", cmd.Bool("standalone"), cmd.StringSlice("message"))
				}
				return nil
			}
			if err := cmd.Run(t.Context(), []string{"tau", flag, "--message", "test prompt"}); err != nil || !reached {
				t.Fatalf("parse: reached=%t err=%v", reached, err)
			}
		})
	}
}

func TestCoordinatedStartupAction(t *testing.T) {
	cmd := newRootCommand()
	if action, arg := coordinatedStartupAction(cmd, false); action != "" || arg != "" {
		t.Fatalf("bare client: %q %q", action, arg)
	}
	if action, arg := coordinatedStartupAction(cmd, true); action != "new" || arg != "" {
		t.Fatalf("startup prompt: %q %q", action, arg)
	}
	if err := cmd.Set("resume", "saved"); err != nil {
		t.Fatal(err)
	}
	if action, arg := coordinatedStartupAction(cmd, true); action != "resume" || arg != "saved" {
		t.Fatalf("explicit resume: %q %q", action, arg)
	}
}

func TestCoordinatedStartupFlagsOffline(t *testing.T) {
	t.Setenv("TAU_OFFLINE", "1")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "environment only"},
		{name: "explicit disable", args: []string{"--offline=false"}, want: "--offline"},
		{name: "explicit enable", args: []string{"--offline"}, want: "--offline"},
		{name: "model override", args: []string{"--model", "example"}, want: "--models"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newRootCommand()
			cmd.Action = func(_ context.Context, c *cli.Command) error { return coordinatedStartupFlags(c) }
			err := cmd.Run(t.Context(), append([]string{"tau"}, tc.args...))
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestCoordinatedThemesRespectProjectTrust(t *testing.T) {
	cfg := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", cfg)
	t.Setenv("TAU_THEMES_DIR", filepath.Join(cfg, "themes"))
	t.Setenv("TAU_PACKAGE_DIR", filepath.Join(cfg, "packages"))
	for _, dir := range []string{filepath.Join(cfg, "themes"), filepath.Join(cwd, ".tau", "themes")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	raw := string(artifacts.BuiltinThemes()[0].Raw)
	for name, dir := range map[string]string{"user-fixture": filepath.Join(cfg, "themes"), "project-fixture": filepath.Join(cwd, ".tau", "themes")} {
		raw := strings.Replace(raw, `"name": "dark"`, `"name": "`+name+`"`, 1)
		if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, policy := range []string{"never", "always"} {
		t.Run(policy, func(t *testing.T) {
			projectTrustDecisions.Delete(cwd)
			t.Cleanup(func() { projectTrustDecisions.Delete(cwd) })
			if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"defaultProjectTrust":"`+policy+`"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			settings, err := coordinatedViewSettings(t.Context(), cwd)
			if err != nil {
				t.Fatal(err)
			}
			loader, err := coordinatedThemes(cwd, settings)
			if err != nil {
				t.Fatal(err)
			}
			lookup := themeLookup(loader)
			if _, ok := lookup("user-fixture"); !ok {
				t.Fatal("user theme not loaded")
			}
			if _, ok := lookup("project-fixture"); ok != (policy == "always") {
				t.Fatalf("project theme loaded=%t under %s trust", ok, policy)
			}
		})
	}
}

func TestCoordinatedResumeListsDaemonStorageWithoutAWorker(t *testing.T) {
	cfg := t.TempDir()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	cwd := filepath.Join(root, "test project's directory")
	daemonDir := filepath.Join(cwd, "daemon-sessions")
	t.Setenv("TAU_SESSION_DIR", t.TempDir())
	storage, err := sessionpkg.NewStorageAt(daemonDir)
	if err != nil {
		t.Fatal(err)
	}
	header := sessionpkg.NewHeader(cwd, nil, nil, "daemon session")
	w, err := storage.Create(header)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	socketDir := t.TempDir()
	require.NoError(t, os.Chmod(socketDir, 0o700))
	path := filepath.Join(socketDir, "c.sock")
	done := make(chan error, 1)
	var starts atomic.Int32
	go func() {
		done <- coord.RunRecoverable(ctx, path, filepath.Join(cfg, "receipts"), func(context.Context, coord.LaunchRequest) error {
			starts.Add(1)
			return errors.New("unexpected launch")
		}, coord.WithSessionDirectory(func(_ context.Context, project string) (string, error) {
			if project != cwd {
				return "", fmt.Errorf("unexpected project %q", project)
			}
			return daemonDir, nil
		}), coord.WithSessionLocator(func(_ context.Context, ref, hint string) (string, string, error) {
			located, err := sessionpkg.NewStorageAt(daemonDir)
			if err != nil {
				return "", "", err
			}
			found, project, err := located.LocateSession(ref, hint)
			return found.ID, project, err
		}))
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	var c *client.CoordinatedClient
	require.Eventually(t, func() bool {
		c, err = client.NewCoordinatedClient(ctx, path)
		return err == nil
	}, time.Second, 5*time.Millisecond)
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	saved, err := listSavedTUISessions(ctx, c, cwd)
	require.NoError(t, err)
	require.Len(t, saved, 1)
	require.Equal(t, header.ID, saved[0].ID)
	resolved, project, err := resolveSavedTUISession(ctx, c, cwd, header.ID[:12])
	require.NoError(t, err)
	require.Equal(t, header.ID, resolved)
	require.Equal(t, cwd, project)
	dir, err := lookupSavedSessionDir(ctx, c, path, cwd)
	require.NoError(t, err)
	savedDir := savedSessionDir{project: cwd, path: dir}

	for _, tc := range []struct {
		name, launchCWD, sessionID, prefix string
	}{
		{name: "same project", launchCWD: cwd, sessionID: header.ID, prefix: "Resume this session with: tau --resume "},
		{name: "different project", launchCWD: cfg, sessionID: header.ID, prefix: "Resume this session with: cd -- '" + root + `/test project'"'"'s directory' && tau --resume `},
		{name: "not saved", launchCWD: cwd, sessionID: sessionpkg.NewHeader(cwd, nil, nil, "").ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			require.NoError(t, printCoordinatedResumeInstructions(&out, savedDir, tc.launchCWD, sdk.NewSessionState(tc.sessionID, "", cwd)))
			if tc.prefix == "" {
				require.Empty(t, out.String())
				return
			}
			require.Contains(t, out.String(), "Session ID: "+header.ID+"\n")
			require.Contains(t, out.String(), tc.prefix+header.ID+"\n")
		})
	}
	require.Zero(t, starts.Load())
}

func TestCoordinatedResumeByIDIgnoresClientDirectory(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	project, elsewhere, sessions := filepath.Join(root, "project"), filepath.Join(root, "elsewhere"), filepath.Join(root, "sessions")
	require.NoError(t, os.Mkdir(project, 0o700))
	require.NoError(t, os.Mkdir(elsewhere, 0o700))
	t.Setenv("HOME", root)
	storage, err := sessionpkg.NewStorageAt(sessions)
	require.NoError(t, err)
	header := sessionpkg.NewHeader(project, nil, nil, "saved")
	w, err := storage.Create(header)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	sessionDirectory := func(context.Context, string) (string, error) { return sessions, nil }

	ctx, cancel := context.WithCancel(t.Context())
	socketDir := t.TempDir()
	require.NoError(t, os.Chmod(socketDir, 0o700))
	path := filepath.Join(socketDir, "c.sock")
	done := make(chan error, 1)
	go func() {
		done <- coord.RunRecoverable(ctx, path, filepath.Join(root, "receipts"), func(context.Context, coord.LaunchRequest) error {
			return errors.New("unexpected launch")
		}, coord.WithSessionDirectory(sessionDirectory), coord.WithSessionLocator(func(ctx context.Context, ref, hint string) (string, string, error) {
			return locateDaemonSession(ctx, sessionDirectory, ref, hint)
		}))
	}()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
	var c *client.CoordinatedClient
	require.Eventually(t, func() bool {
		c, err = client.NewCoordinatedClient(ctx, path)
		return err == nil
	}, time.Second, 5*time.Millisecond)
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	for name, hint := range map[string]string{
		"other directory":         elsewhere,
		"directory not on daemon": filepath.Join(root, "only-on-client"),
	} {
		t.Run(name, func(t *testing.T) {
			id, got, err := resolveSavedTUISession(ctx, c, hint, header.ID[:13])
			require.NoError(t, err)
			require.Equal(t, header.ID, id)
			require.Equal(t, project, got)
		})
	}
	_, _, err = resolveSavedTUISession(ctx, c, elsewhere, "ffffffff-missing")
	require.ErrorContains(t, err, "not found")
}

func TestCoordinatedResumeHintAfterDaemonLoss(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	daemonDir := filepath.Join(root, "sessions")
	storage, err := sessionpkg.NewStorageAt(daemonDir)
	require.NoError(t, err)
	header := sessionpkg.NewHeader(root, nil, nil, "daemon session")
	w, err := storage.Create(header)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	socketDir := t.TempDir()
	require.NoError(t, os.Chmod(socketDir, 0o700))
	path := filepath.Join(socketDir, "c.sock")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- coord.RunRecoverable(ctx, path, filepath.Join(socketDir, "receipts"), func(context.Context, coord.LaunchRequest) error {
			return errors.New("unexpected launch")
		}, coord.WithSessionDirectory(func(context.Context, string) (string, error) { return daemonDir, nil }))
	}()
	var c *client.CoordinatedClient
	require.Eventually(t, func() bool {
		c, err = client.NewCoordinatedClient(ctx, path)
		return err == nil
	}, time.Second, 5*time.Millisecond)
	require.NoError(t, c.Close())

	// The picker link is already gone, as after a coordinator reconnect.
	dir, err := lookupSavedSessionDir(ctx, c, path, root)
	require.NoError(t, err)
	require.Equal(t, daemonDir, dir)
	saved := savedSessionDir{project: root, path: dir}

	cancel()
	require.NoError(t, <-done)

	var out strings.Builder
	require.NoError(t, printCoordinatedResumeInstructions(&out, saved, root, sdk.NewSessionState(header.ID, "", root)))
	require.Contains(t, out.String(), "Resume this session with: tau --resume "+header.ID+"\n")

	out.Reset()
	other := filepath.Join(root, "other")
	require.NoError(t, printCoordinatedResumeInstructions(&out, saved, root, sdk.NewSessionState(header.ID, "", other)))
	require.Empty(t, out.String(), "a directory cached for one project must not vouch for another")
}

func TestRestartTUIInstancePreservesUntouchedSession(t *testing.T) {
	cwd, dir, sockDir := t.TempDir(), t.TempDir(), t.TempDir()
	require.NoError(t, os.Chmod(sockDir, 0o700))
	path := filepath.Join(sockDir, "c.sock")
	receipts := filepath.Join(sockDir, "receipts")
	require.NoError(t, os.Mkdir(receipts, 0o700))
	// A freshly created store rejects launch IDs from its cutoff millisecond.
	require.NoError(t, os.WriteFile(filepath.Join(receipts, "cutoff"), []byte("0"), 0o600))
	storage, err := sessionpkg.NewStorageAt(dir)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	var workers sync.WaitGroup
	done := make(chan error, 1)
	replacementRequested, releaseReplacement := make(chan struct{}), make(chan struct{})
	go func() {
		done <- coord.RunRecoverable(ctx, path, receipts, func(launchCtx context.Context, req coord.LaunchRequest) error {
			if req.SessionID != "" {
				close(replacementRequested)
				select {
				case <-releaseReplacement:
				case <-launchCtx.Done():
					return launchCtx.Err()
				}
			}
			lifetime, err := unix.Dup(int(req.Lifetime.Fd()))
			if err != nil {
				return err
			}
			workers.Go(func() {
				defer func() { assert.NoError(t, unix.Close(lifetime)) }()
				rt, err := sdk.NewRuntime(sdk.RuntimeOptions{SessionsDir: dir})
				if !assert.NoError(t, err) {
					return
				}
				assert.NoError(t, server.NewWorker(rt, nil, nil).Run(ctx, path, coord.Instance{
					ID: req.InstanceID, Name: req.Name, NodeID: "node", CWD: req.CWD, SessionID: req.SessionID, PID: os.Getpid(), LaunchRequestID: req.LaunchRequestID,
				}, req.Token))
			})
			return nil
		}, coord.WithSessionDirectory(func(context.Context, string) (string, error) { return dir, nil }))
	}()
	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-done)
		workers.Wait()
	})
	op, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var c *client.CoordinatedClient
	require.Eventually(t, func() bool {
		c, err = client.NewCoordinatedClient(op, path)
		return err == nil
	}, time.Second, 5*time.Millisecond)
	t.Cleanup(func() { assert.NoError(t, c.Close()) })
	original, err := c.StartInstance(op, cwd)
	require.NoError(t, err)
	require.NoError(t, c.RenameInstance(op, original.ID, "API work"))
	original.Name = "API work"
	_, err = storage.ResolveHeader(original.SessionID)
	require.Error(t, err, "untouched session should not be on disk yet")
	require.ErrorContains(t, c.StopInstanceForRestart(op, original.ID, "wrong-session"), "switched sessions")
	drafts := map[string]string{original.ID: "unsent draft"}
	picker := tui.InstancePickerState{FocusID: original.ID, Filter: "my filter"}
	choice := tui.InstanceSelection{Action: tui.InstanceActionRestart, Targets: []coord.Instance{*original}}
	job := startTUIInstanceManagement(op, path, choice, func(result instanceManagementResult) (string, string, map[string]string) {
		return result.apply(choice.Action, original.ID, drafts, &picker), result.notice, result.changes
	})
	select {
	case <-replacementRequested:
	case <-op.Done():
		t.Fatal("replacement was not requested")
	}
	select {
	case <-job.Done:
		t.Fatal("restart completed before the replacement was released")
	default:
	}
	_, err = c.ListInstances(op)
	require.NoError(t, err, "the picker connection must remain usable during restart")
	require.Equal(t, "unsent draft", drafts[original.ID])
	close(releaseReplacement)
	select {
	case <-job.Done:
	case <-op.Done():
		t.Fatal("restart did not finish")
	}
	current, notice, changes := job.Finish()
	require.Equal(t, "Restarted 1 of 1 instances", notice)
	require.NotEmpty(t, current)
	require.NotEqual(t, original.ID, current)
	require.Equal(t, current, changes[original.ID])
	require.Equal(t, map[string]string{current: "unsent draft"}, drafts)
	require.Equal(t, tui.InstancePickerState{FocusID: current, Filter: "my filter"}, picker)
	replacement, err := c.StartSavedSession(op, cwd, original.SessionID)
	require.NoError(t, err)
	require.Equal(t, current, replacement.ID)
	require.Equal(t, original.SessionID, replacement.SessionID)
	require.Eventually(t, func() bool {
		instances, err := c.ListInstances(op)
		return err == nil && slices.ContainsFunc(instances, func(inst coord.Instance) bool {
			return inst.ID == current && inst.Name == "API work"
		})
	}, time.Second, 5*time.Millisecond, "restart should keep the instance name")
	header, err := storage.ResolveHeader(original.SessionID)
	require.NoError(t, err)
	require.Equal(t, original.SessionID, header.ID)
}
