package plugins

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/losinggeneration/tau/piext"
)

const shimStatusKey = "pi-ext-shim"

type shimSetup struct {
	done   chan struct{}
	cancel context.CancelFunc
	err    error
}

// ShimReady returns an independent result subscription for the latest shim setup.
// Nil means no pi-ext-shim setup has been started.
func (m *Manager) ShimReady() <-chan error {
	m.mu.RLock()
	setup := m.shimSetup
	m.mu.RUnlock()
	if setup == nil {
		return nil
	}
	ready := make(chan error, 1)
	go func() {
		<-setup.done
		ready <- setup.err
		close(ready)
	}()
	return ready
}

// prepareShimIfNeeded inspects discovered manifests with m.mu held.
func (m *Manager) prepareShimIfNeeded(ctx context.Context) {
	if !m.hasShimManifestLocked() {
		return
	}
	shimDir := m.shimDir()
	if piext.IsShimCurrent(shimDir) {
		return
	}
	m.startShimEnsureLocked(ctx, shimDir)
}

func (m *Manager) hasShimManifestLocked() bool {
	for _, manifestPath := range m.manifestPaths {
		manifest, err := m.packageManifest(manifestPath)
		if err != nil || !manifestNeedsManagedShim(manifest) {
			continue
		}
		return true
	}
	return false
}

// waitForShimReady releases m.mu while waiting and reacquires it before returning.
// Caller must hold m.mu.
func (m *Manager) waitForShimReady(ctx context.Context, manifest *Manifest) error {
	if !manifestNeedsManagedShim(manifest) {
		return nil
	}
	if err := context.Cause(ctx); err != nil {
		return fmt.Errorf("wait for pi-ext-shim setup: %w", err)
	}
	if m.shimClosed {
		return errors.New("pi-ext-shim setup manager is shut down")
	}
	if piext.IsShimCurrent(m.shimDir()) {
		return nil
	}
	m.startShimEnsureLocked(ctx, m.shimDir())
	setup := m.shimSetup
	m.mu.Unlock()
	select {
	case <-setup.done:
	case <-ctx.Done():
	}
	m.mu.Lock()
	if err := context.Cause(ctx); err != nil {
		return fmt.Errorf("wait for pi-ext-shim setup: %w", err)
	}
	if m.shimClosed {
		return errors.New("pi-ext-shim setup manager is shut down")
	}
	return setup.err
}

func (m *Manager) startShimEnsureLocked(ctx context.Context, shimDir string) {
	if m.shimClosed {
		return
	}
	if m.shimSetup != nil {
		select {
		case <-m.shimSetup.done:
		default:
			return
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	setup := &shimSetup{done: make(chan struct{}), cancel: cancel}
	m.shimSetup = setup
	status := m.shimStatusFn
	if status == nil {
		status = func(message string) { m.uiSetStatus(shimStatusKey, message) }
	}
	install := m.shimInstallFn
	go func() {
		defer close(setup.done)
		defer cancel()
		status("Setting up pi-ext-shim…")
		_, setup.err = piext.EnsureShim(ctx, shimDir, install, status)
		if setup.err != nil {
			status("pi-ext-shim setup failed")
		} else {
			status("")
		}
	}()
}

// stopShimSetupLocked closes admission and joins setup outside m.mu so status
// publication and existing waiters can finish during shutdown.
func (m *Manager) stopShimSetupLocked(ctx context.Context) error {
	m.shimClosed = true
	setup := m.shimSetup
	if setup == nil {
		return nil
	}
	setup.cancel()
	m.mu.Unlock()
	defer m.mu.Lock()
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.cleanupGrace)
	defer cancel()
	select {
	case <-setup.done:
		return setup.err
	case <-waitCtx.Done():
		return fmt.Errorf("pi-ext-shim setup still running: %w", waitCtx.Err())
	}
}

func (m *Manager) shimDir() string {
	return filepath.Join(m.configDir, "pi-ext-shim")
}

func manifestNeedsManagedShim(manifest *Manifest) bool {
	if manifest == nil || !manifest.HasRunCommand() {
		return false
	}
	fields := manifest.Run.args
	if fields == nil {
		fields = splitCommand(manifest.Run.Command)
	}
	if len(fields) < 2 {
		return false
	}
	return strings.HasSuffix(filepath.ToSlash(fields[1]), "/pi-ext-shim/index.js")
}
