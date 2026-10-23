package plugins

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/losinggeneration/tau/piext"
)

func TestShimStartupEnsureRunsForManagedManifest(t *testing.T) {
	configDir := t.TempDir()
	pkgDir := filepath.Join(configDir, "packages", "pi-test")
	if err := os.MkdirAll(pkgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(pkgDir, "tau-package.toml")
	manifest := `[package]
name = "pi-test"
version = "0.0.1"

[plugin]
api_version = "1"

[run]
command = "node ` + filepath.ToSlash(filepath.Join(configDir, "pi-ext-shim", "index.js")) + ` pi-extension/index.js"
`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	var installs atomic.Int64
	mgr := NewManager(configDir, nil, nil, WithShimInstall(func(_ context.Context, dir string) error {
		installs.Add(1)
		if err := piext.WriteShimFilesystem(dir); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(dir, "node_modules", "jiti"), 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "node_modules", "jiti", "package.json"), []byte(`{"name":"jiti"}`), 0o600)
	}))
	if err := mgr.scanPackagesDir(filepath.Join(configDir, "packages")); err != nil {
		t.Fatal(err)
	}

	mgr.mu.Lock()
	manifestParsed, err := parseManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	err = mgr.waitForShimReady(context.Background(), manifestParsed)
	mgr.mu.Unlock()
	if err != nil {
		t.Fatalf("waitForShimReady() error = %v", err)
	}
	if installs.Load() != 1 {
		t.Fatalf("install count = %d, want 1", installs.Load())
	}
	if !piext.IsShimCurrent(filepath.Join(configDir, "pi-ext-shim")) {
		t.Fatal("shim is not current")
	}
}

func TestShimStartupStatusMessages(t *testing.T) {
	configDir := t.TempDir()
	var messages []string
	mgr := NewManager(configDir, nil, nil,
		WithShimInstall(func(_ context.Context, dir string) error {
			if err := piext.WriteShimFilesystem(dir); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Join(dir, "node_modules", "jiti"), 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "node_modules", "jiti", "package.json"), []byte(`{"name":"jiti"}`), 0o600)
		}),
		WithShimStatus(func(message string) { messages = append(messages, message) }),
	)

	mgr.mu.Lock()
	mgr.startShimEnsureLocked(context.Background(), filepath.Join(configDir, "pi-ext-shim"))
	setup := mgr.shimSetup
	mgr.mu.Unlock()
	<-setup.done
	if setup.err != nil {
		t.Fatalf("shim setup error = %v", setup.err)
	}
	joined := strings.Join(messages, "\n")
	if !strings.Contains(joined, "Setting up pi-ext-shim") {
		t.Fatalf("status messages %q missing setup line", joined)
	}
}

func TestShimReadyBroadcastsFailure(t *testing.T) {
	installErr := errors.New("installer failed")
	started, release := make(chan struct{}), make(chan struct{})
	mgr := NewManager(t.TempDir(), nil, nil, WithShimInstall(func(context.Context, string) error {
		close(started)
		<-release
		return installErr
	}))
	mgr.mu.Lock()
	mgr.startShimEnsureLocked(t.Context(), mgr.shimDir())
	mgr.mu.Unlock()
	<-started
	first, second := mgr.ShimReady(), mgr.ShimReady()
	close(release)
	for i, ready := range []<-chan error{first, second} {
		if err := <-ready; !errors.Is(err, installErr) {
			t.Errorf("observer %d error = %v, want installer failure", i, err)
		}
	}
}

func TestShimWaitReleasesManagerLock(t *testing.T) {
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr := NewManager(t.TempDir(), nil, nil, WithShimInstall(func(ctx context.Context, _ string) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}))
	manifest := &Manifest{Run: &RunSection{Command: "node /test/pi-ext-shim/index.js extension.js"}}
	finished := make(chan error, 1)
	go func() {
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		finished <- mgr.waitForShimReady(ctx, manifest)
	}()
	<-started
	observed := make(chan (<-chan error), 1)
	go func() { observed <- mgr.ShimReady() }()
	select {
	case ready := <-observed:
		cancel()
		if err := <-ready; !errors.Is(err, context.Canceled) {
			t.Errorf("setup error = %v, want cancellation", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Error("shim wait holds manager lock")
		cancel()
		ready := <-observed
		<-ready
	}
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Errorf("wait error = %v, want cancellation", err)
	}
}

func TestShutdownCancelsAndJoinsShimSetup(t *testing.T) {
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr := NewManager(t.TempDir(), nil, nil, WithShimInstall(func(ctx context.Context, _ string) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		return ctx.Err()
	}))
	mgr.mu.Lock()
	mgr.startShimEnsureLocked(ctx, mgr.shimDir())
	mgr.mu.Unlock()
	<-started
	ready := mgr.ShimReady()
	finished := make(chan error, 1)
	shutdownCtx, cancelShutdown := context.WithCancel(t.Context())
	cancelShutdown()
	go func() { finished <- mgr.Shutdown(shutdownCtx) }()
	select {
	case <-cancelled:
		select {
		case err := <-finished:
			t.Errorf("shutdown returned before installer cleanup: %v", err)
			finished <- err
		default:
		}
	case err := <-finished:
		t.Errorf("shutdown neither cancelled nor joined shim setup: %v", err)
		finished <- err
		cancel()
		<-cancelled
	case <-time.After(250 * time.Millisecond):
		t.Error("shutdown did not cancel shim setup")
		cancel()
		<-cancelled
	}
	close(release)
	if err := <-ready; !errors.Is(err, context.Canceled) {
		t.Errorf("setup error = %v, want cancellation", err)
	}
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Errorf("shutdown error = %v, want cancellation", err)
	}
	mgr.mu.Lock()
	previous := mgr.shimSetup
	mgr.startShimEnsureLocked(t.Context(), mgr.shimDir())
	if mgr.shimSetup != previous {
		t.Error("shutdown admitted another shim setup")
	}
	manifest := &Manifest{Run: &RunSection{Command: "node /test/pi-ext-shim/index.js extension.js"}}
	err := mgr.waitForShimReady(t.Context(), manifest)
	mgr.mu.Unlock()
	if err == nil {
		t.Error("shutdown accepted a managed plugin waiter")
	}
}

func TestShimSetupRetriesAndRetainsObserverResults(t *testing.T) {
	installErr := errors.New("first install failed")
	var installs atomic.Int32
	mgr := NewManager(t.TempDir(), nil, nil, WithShimInstall(func(_ context.Context, dir string) error {
		if installs.Add(1) == 1 {
			return installErr
		}
		if err := os.MkdirAll(filepath.Join(dir, "node_modules", "jiti"), 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "node_modules", "jiti", "package.json"), []byte(`{"name":"jiti"}`), 0o600)
	}))
	if mgr.ShimReady() != nil {
		t.Fatal("unstarted setup has a subscription")
	}
	mgr.mu.Lock()
	mgr.startShimEnsureLocked(t.Context(), mgr.shimDir())
	mgr.mu.Unlock()
	first := mgr.ShimReady()
	if err := <-first; !errors.Is(err, installErr) {
		t.Fatalf("first install = %v", err)
	}
	old := mgr.ShimReady()
	manifest := &Manifest{Run: &RunSection{Command: "node /test/pi-ext-shim/index.js extension.js"}}
	mgr.mu.Lock()
	err := mgr.waitForShimReady(t.Context(), manifest)
	mgr.mu.Unlock()
	if err != nil {
		t.Fatalf("retry = %v", err)
	}
	if err := <-old; !errors.Is(err, installErr) {
		t.Errorf("old observer result changed: %v", err)
	}
	if err := <-mgr.ShimReady(); err != nil {
		t.Errorf("new observer = %v", err)
	}
	if installs.Load() != 2 || !piext.IsShimCurrent(mgr.shimDir()) {
		t.Error("retry did not install current shim exactly once")
	}
	if err := mgr.Shutdown(t.Context()); err != nil {
		t.Errorf("shutdown after successful retry = %v", err)
	}
}

func TestShimStatusCanReadManager(t *testing.T) {
	var mgr *Manager
	mgr = NewManager(t.TempDir(), nil, nil,
		WithShimInstall(func(context.Context, string) error { return errors.New("not installed") }),
		WithShimStatus(func(string) {
			mgr.mu.RLock()
			mgr.mu.RUnlock()
		}),
	)
	mgr.mu.Lock()
	mgr.startShimEnsureLocked(t.Context(), mgr.shimDir())
	mgr.mu.Unlock()
	if err := <-mgr.ShimReady(); err == nil {
		t.Error("install failure lost")
	}
}

func TestShutdownReportsUncooperativeShimSetup(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	installErr := errors.New("installer cleanup failed")
	mgr := NewManager(t.TempDir(), nil, nil, WithShimInstall(func(context.Context, string) error {
		close(started)
		<-release
		return installErr
	}))
	mgr.cleanupGrace = 10 * time.Millisecond
	mgr.mu.Lock()
	mgr.startShimEnsureLocked(t.Context(), mgr.shimDir())
	mgr.mu.Unlock()
	<-started
	err := mgr.Shutdown(t.Context())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("shutdown = %v, want bounded wait failure", err)
	}
	close(release)
	if err := <-mgr.ShimReady(); !errors.Is(err, installErr) {
		t.Errorf("setup failure = %v", err)
	}
	if err := mgr.Shutdown(t.Context()); !errors.Is(err, installErr) {
		t.Errorf("joined shutdown lost cleanup failure: %v", err)
	}
}

func TestShimWaitersShareInstallFailure(t *testing.T) {
	installErr := errors.New("shared install failure")
	started, release := make(chan struct{}), make(chan struct{})
	var installs atomic.Int32
	mgr := NewManager(t.TempDir(), nil, nil, WithShimInstall(func(context.Context, string) error {
		installs.Add(1)
		close(started)
		<-release
		return installErr
	}))
	mgr.mu.Lock()
	mgr.startShimEnsureLocked(t.Context(), mgr.shimDir())
	mgr.mu.Unlock()
	<-started
	const waiters = 4
	entered := make(chan struct{}, waiters)
	results := make(chan error, waiters)
	manifest := &Manifest{Run: &RunSection{Command: "node /test/pi-ext-shim/index.js extension.js"}}
	for range waiters {
		go func() {
			mgr.mu.Lock()
			entered <- struct{}{}
			err := mgr.waitForShimReady(t.Context(), manifest)
			mgr.mu.Unlock()
			results <- err
		}()
	}
	for range waiters {
		<-entered
	}
	mgr.mu.Lock()
	mgr.mu.Unlock()
	close(release)
	for range waiters {
		if err := <-results; !errors.Is(err, installErr) {
			t.Errorf("waiter lost install failure: %v", err)
		}
	}
	if installs.Load() != 1 {
		t.Errorf("install count = %d, want 1", installs.Load())
	}
}
