package plugins

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/losinggeneration/tau/piext"
)

const shimStatusKey = "pi-ext-shim"

// ShimReady returns a channel that resolves when startup shim maintenance has finished.
// Nil means no pi-ext-shim setup is pending.
func (m *Manager) ShimReady() <-chan error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.shimReady
}

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

func (m *Manager) shimStatus(message string) {
	if m.shimStatusFn != nil {
		m.shimStatusFn(message)
		return
	}
	if m.ui != nil {
		m.ui.SetStatus(shimStatusKey, message)
	}
}

func (m *Manager) hasShimManifestLocked() bool {
	for _, manifestPath := range m.manifestPaths {
		manifest, err := parseManifest(manifestPath)
		if err != nil || !manifestNeedsManagedShim(manifest) {
			continue
		}
		return true
	}
	return false
}

func (m *Manager) waitForShimReady(ctx context.Context, manifest *Manifest) error {
	if !manifestNeedsManagedShim(manifest) {
		return nil
	}
	m.startShimEnsureLocked(ctx, m.shimDir())
	ready := m.shimReady
	if ready == nil {
		return nil
	}
	select {
	case err := <-ready:
		return err
	case <-ctx.Done():
		return fmt.Errorf("wait for pi-ext-shim setup: %w", ctx.Err())
	}
}

func (m *Manager) startShimEnsureLocked(ctx context.Context, shimDir string) {
	if m.shimReady != nil && !m.shimReadyClosed {
		return
	}
	ready := make(chan error, 1)
	m.shimReady = ready
	m.shimReadyClosed = false
	install := m.shimInstallFn
	status := m.shimStatus
	status("Setting up pi-ext-shim…")
	go func() {
		err := error(nil)
		_, err = piext.EnsureShim(ctx, shimDir, install, status)
		if err != nil {
			status("pi-ext-shim setup failed")
		} else {
			status("")
		}
		ready <- err
		close(ready)
		m.mu.Lock()
		m.shimReadyClosed = true
		m.mu.Unlock()
	}()
}

func (m *Manager) shimDir() string {
	return filepath.Join(m.configDir, "pi-ext-shim")
}

func manifestNeedsManagedShim(manifest *Manifest) bool {
	if manifest == nil || !manifest.HasRunCommand() {
		return false
	}
	fields := splitCommand(manifest.Run.Command)
	if len(fields) < 2 {
		return false
	}
	return strings.HasSuffix(filepath.ToSlash(fields[1]), "/pi-ext-shim/index.js")
}
