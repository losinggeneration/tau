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
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/losinggeneration/tau/agent/session"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/rpc/coord"
	"github.com/losinggeneration/tau/rpc/server"
	"github.com/stretchr/testify/require"
)

func TestServeCoordinatorStopsBridgeWithCoordinator(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "c.sock")
	bridge, err := server.NewCoordinatedBridge("127.0.0.1:0", socket, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	busy := errors.New("coordinator already running")
	err = serveCoordinator(t.Context(), socket, bridge, nil, func(context.Context, string, coord.LaunchFunc) error { return busy })
	if !errors.Is(err, busy) {
		t.Fatalf("serve = %v, want coordinator failure", err)
	}
	if conn, err := net.DialTimeout("tcp", bridge.Addr(), time.Second); err == nil {
		t.Errorf("bridge still accepting after coordinator failure: %v", conn.Close())
	}
}

func TestServeCoordinatorStopsCoordinatorOnCancel(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "c.sock")
	bridge, err := server.NewCoordinatedBridge("127.0.0.1:0", socket, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	running := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- serveCoordinator(ctx, socket, bridge, nil, func(ctx context.Context, _ string, _ coord.LaunchFunc) error {
			close(running)
			<-ctx.Done()
			return nil
		})
	}()
	<-running
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("coordinator and bridge did not stop")
	}
}

func TestCoordinatedBridgeRefusesNonLoopback(t *testing.T) {
	if _, err := server.NewCoordinatedBridge("0.0.0.0:0", filepath.Join(t.TempDir(), "c.sock"), "secret", nil); err == nil {
		t.Fatal("non-loopback coordinated bridge started")
	}
}

func TestCoordinatorNodeIdentityPersists(t *testing.T) {
	dir := t.TempDir()
	first, err := coordinatorNodeID(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := coordinatorNodeID(dir)
	if err != nil || first == "" || first != second {
		t.Fatalf("identity changed: %q -> %q (%v)", first, second, err)
	}
	info, err := os.Stat(filepath.Join(dir, "node-id"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("node identity permissions: %v, %v", info, err)
	}
}

func TestWorkerLaunchDoesNotInheritTrustOverride(t *testing.T) {
	inherited := []string{"TAU_COORD_TOKEN=old", "TAU_COORD_INSTANCE=old", "TAU_COORD_NODE=old", "TAU_COORD_LAUNCH_REQUEST=old", "TAU_COORD_SESSION=old", "TAU_COORD_SESSION_DIR=old", "PATH=/bin"}
	if env := workerEnvironment(inherited, "node", coord.LaunchRequest{Token: "token", InstanceID: "instance"}, ""); strings.Contains(strings.Join(env, "\n"), "TAU_COORD_LAUNCH_REQUEST") || strings.Contains(strings.Join(env, "\n"), "TAU_COORD_SESSION") {
		t.Fatalf("launch without a request ID inherited one: %q", env)
	}
	env := workerEnvironment(inherited, "node", coord.LaunchRequest{Token: "token", InstanceID: "instance", LaunchRequestID: "request", SessionID: "saved"}, "/saved/sessions")
	joined := strings.Join(env, "\n")
	for _, expected := range []string{"TAU_COORD_TOKEN=token", "TAU_COORD_INSTANCE=instance", "TAU_COORD_NODE=node", "TAU_COORD_LAUNCH_REQUEST=request", "TAU_COORD_SESSION=saved", "TAU_COORD_SESSION_DIR=/saved/sessions", "PATH=/bin"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("missing %q in environment", expected)
		}
	}
	if strings.Contains(joined, "=old") {
		t.Fatalf("stale launch credentials retained: %q", joined)
	}
}

func TestWorkerEnvironmentDropsCoordinatorPWD(t *testing.T) {
	env := workerEnvironment([]string{"PWD=/coordinator", "TAU_WEBSOCKET_TOKEN=bridge-only", "PWDX=keep", "PATH=/bin"}, "node", coord.LaunchRequest{Token: "token", InstanceID: "instance"}, "")
	for _, entry := range env {
		if strings.HasPrefix(entry, "PWD=") || strings.HasPrefix(entry, "TAU_WEBSOCKET_TOKEN=") {
			t.Fatalf("coordinator-only environment inherited: %q", env)
		}
	}
	if !slices.Contains(env, "PWDX=keep") {
		t.Fatalf("unrelated variable dropped: %q", env)
	}
}

func TestCoordinatorNodeIdentityRecoversEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node-id")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := coordinatorNodeID(dir)
	if err != nil {
		t.Fatalf("empty identity file: %v", err)
	}
	if _, err := uuid.Parse(id); err != nil {
		t.Fatalf("identity %q: %v", id, err)
	}
	again, err := coordinatorNodeID(dir)
	if err != nil || again != id {
		t.Fatalf("identity changed: %q -> %q (%v)", id, again, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if name := entry.Name(); name != "node-id" && name != "node-id.lock" {
			t.Fatalf("leftover file %q", name)
		}
	}
}

func TestCoordinatorNodeIdentityRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "node-id"), []byte("not-a-uuid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinatorNodeID(dir); err == nil {
		t.Fatal("corrupt identity accepted")
	}
}

func TestCoordinatorNodeIdentityConcurrentInit(t *testing.T) {
	dir := t.TempDir()
	const callers = 8
	ids := make(chan string, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			id, err := coordinatorNodeID(dir)
			if err != nil {
				if !strings.Contains(err.Error(), "being initialized") {
					t.Errorf("concurrent init: %v", err)
				}
				return
			}
			ids <- id
		})
	}
	wg.Wait()
	close(ids)
	final, err := coordinatorNodeID(dir)
	if err != nil {
		t.Fatal(err)
	}
	for id := range ids {
		if id != final {
			t.Fatalf("concurrent init produced %q, saved %q", id, final)
		}
	}
}

type fakeStart struct {
	mu     sync.Mutex
	calls  []string
	exited bool
	reaped chan struct{}
}

func newFakeStart() *fakeStart {
	return &fakeStart{reaped: make(chan struct{})}
}

func (f *fakeStart) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeStart) history() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeStart) worker() startedWorker {
	return startedWorker{
		pid: 4242,
		exited: func() (bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.exited {
				f.calls = append(f.calls, "observed exit")
				return true, errors.New("exit status 3")
			}
			return false, nil
		},
		kill: func(pid int, sig syscall.Signal) error {
			f.record(fmt.Sprintf("kill %d %v", pid, sig))
			return nil
		},
		reap: func() {
			f.record("reap")
			close(f.reaped)
		},
	}
}

func (f *fakeStart) waitReaped(t *testing.T) {
	t.Helper()
	select {
	case <-f.reaped:
	case <-time.After(2 * time.Second):
		t.Fatal("worker was not reaped")
	}
}

func TestWorkerStartReadyLeavesWorkerRunning(t *testing.T) {
	f := newFakeStart()
	ready := make(chan struct{})
	close(ready)
	if err := superviseWorkerStart(t.Context(), f.worker(), ready, "w.log"); err != nil {
		t.Fatal(err)
	}
	f.waitReaped(t)
	if got := f.history(); !slices.Equal(got, []string{"reap"}) {
		t.Fatalf("calls = %q, want only background reap", got)
	}
}

func TestWorkerStartCancelKillsGroupBeforeReap(t *testing.T) {
	f := newFakeStart()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := superviseWorkerStart(ctx, f.worker(), make(chan struct{}), "w.log")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want canceled", err)
	}
	f.waitReaped(t)
	want := []string{fmt.Sprintf("kill -4242 %v", syscall.SIGKILL), "reap"}
	if got := f.history(); !slices.Equal(got, want) {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestWorkerStartCancelAfterReadyDoesNotKill(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ready := make(chan struct{})
	close(ready)
	for range 20 {
		f := newFakeStart()
		if err := superviseWorkerStart(ctx, f.worker(), ready, "w.log"); err != nil {
			t.Fatalf("registered worker reported failure: %v", err)
		}
		f.waitReaped(t)
		if got := f.history(); !slices.Equal(got, []string{"reap"}) {
			t.Fatalf("registered worker signaled: %q", got)
		}
	}
}

func TestWorkerStartEarlyExitKillsGroupBeforeReap(t *testing.T) {
	f := newFakeStart()
	f.exited = true
	err := superviseWorkerStart(t.Context(), f.worker(), make(chan struct{}), "w.log")
	if err == nil || !strings.Contains(err.Error(), "w.log") || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("error = %v", err)
	}
	f.waitReaped(t)
	want := []string{"observed exit", fmt.Sprintf("kill -4242 %v", syscall.SIGKILL), "reap"}
	if got := f.history(); !slices.Equal(got, want) {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestWorkerLaunchPathFollowsManagedActivation(t *testing.T) {
	root := newManagedTestRoot(t, "1.0.0", "1.1.0")
	old := filepath.Join(root, "releases", "1.0.0", managedBinaryName())
	current := filepath.Join(root, "current")
	require.NoError(t, os.Symlink(filepath.Join("releases", "1.0.0"), current))
	launch, err := workerLaunchPath(root, old)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(current, managedBinaryName()), launch)
	require.Equal(t, coord.ExecutableBuildID(old), coord.ExecutableBuildID(launch))
	pending := current + ".next"
	require.NoError(t, os.Symlink(filepath.Join("releases", "1.1.0"), pending))
	require.NoError(t, os.Rename(pending, current))
	require.Equal(t, coord.ExecutableBuildID(filepath.Join(root, "releases", "1.1.0", managedBinaryName())), coord.ExecutableBuildID(launch))
	require.NotEqual(t, coord.ExecutableBuildID(old), coord.ExecutableBuildID(launch))
}

func TestCancelledWorkerLaunchDoesNotSpawn(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := launchWorkerProcess(ctx, "unused", "unused", "node", coord.LaunchRequest{InstanceID: "instance", Token: "token", CWD: t.TempDir()}, ""); err == nil {
		t.Fatal("cancelled launch succeeded")
	}
}

func TestWorkerUsesSavedHostTrustWithoutImplicitApproval(t *testing.T) {
	cfg := t.TempDir()
	home := t.TempDir()
	project := t.TempDir()
	protected := filepath.Join(project, ".tau")
	if err := os.Mkdir(protected, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(protected, "settings.json"), []byte(`{"defaultProvider":"project","defaultModel":"protected-model"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"defaultProvider":"global","defaultModel":"safe-model"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := ai.LoadTrustedSettings(t.Context(), cfg, project, home, ai.ProjectTrustOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if settings.ProjectResourcesTrusted() || settings.DefaultModel != "safe-model" {
		t.Fatalf("headless launch trusted project resources: %+v", settings)
	}
	store := ai.ProjectTrustStore{Path: filepath.Join(cfg, "trust.json")}
	if err := store.Set(t.Context(), project, new(true)); err != nil {
		t.Fatal(err)
	}
	settings, err = ai.LoadTrustedSettings(t.Context(), cfg, project, home, ai.ProjectTrustOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !settings.ProjectResourcesTrusted() || settings.DefaultModel != "protected-model" {
		t.Fatalf("saved host trust was not respected: %+v", settings)
	}
}

func TestWorkerLogsRejectSymlinkDirectory(t *testing.T) {
	base := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(base, "workers")); err != nil {
		t.Fatal(err)
	}
	if _, err := workerLogDirectory(base); err == nil {
		t.Fatal("symlink worker log directory accepted")
	}
}

func TestCheckSavedSessionReportsWriterBeforeLaunch(t *testing.T) {
	dir, cwd := t.TempDir(), t.TempDir()
	storage, err := session.NewStorageAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	header := session.NewHeader(cwd, nil, nil)
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatal(err)
	}
	busy := checkSavedSession(dir, cwd, header.ID)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if busy == nil || !strings.Contains(busy.Error(), "open in") {
		t.Fatalf("preflight did not report the live writer: %v", busy)
	}
	if err := checkSavedSession(dir, cwd, header.ID); err != nil {
		t.Fatalf("preflight after writer closed: %v", err)
	}
}
