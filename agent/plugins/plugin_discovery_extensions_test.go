package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/losinggeneration/tau/agent/plugins/proto"
	"github.com/losinggeneration/tau/ai"
	"github.com/stretchr/testify/require"
)

func TestUserExtensionsDirResolution(t *testing.T) {
	t.Setenv("TAU_EXTENSIONS_DIR", "")
	cfgDir := t.TempDir()
	mgr := NewManager(cfgDir, nil, nil)

	// Default: <configDir>/extensions.
	if want := filepath.Join(cfgDir, "extensions"); mgr.userExtensionDir() != want {
		t.Fatalf("default = %q, want %q", mgr.userExtensionDir(), want)
	}

	// settings.json extensionsDir wins over the default.
	mgr.SetSettings(&ai.Settings{ExtensionDir: "/settings/ext"})
	if mgr.userExtensionDir() != "/settings/ext" {
		t.Fatalf("settings = %q, want /settings/ext", mgr.userExtensionDir())
	}

	// TAU_EXTENSIONS_DIR wins over settings.
	t.Setenv("TAU_EXTENSIONS_DIR", "/env/ext")
	if mgr.userExtensionDir() != "/env/ext" {
		t.Fatalf("env = %q, want /env/ext", mgr.userExtensionDir())
	}
}

func TestExtensionSettingsPathsAndModifiers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, "agent")
	extra := filepath.Join(cfgDir, "extra")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"keep", "drop", "last"} {
		if err := os.WriteFile(filepath.Join(extra, name), []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	settingsFile := filepath.Join(cfgDir, "settings.json")
	if err := os.WriteFile(settingsFile, []byte(`{"extensions":["extra","!~/agent/extra/*","+extra/keep","+extra/last","-extra/last"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := ai.LoadSettings(settingsFile)
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(cfgDir, nil, nil)
	mgr.SetSettings(settings)
	for _, dir := range mgr.pluginDirs() {
		if err := mgr.scanDir(dir); err != nil {
			t.Fatal(err)
		}
	}
	mgr.filterConfiguredExtensions()
	if len(mgr.binaries) != 1 {
		t.Fatalf("binaries = %v", mgr.binaries)
	}
	if _, ok := mgr.binaries[filepath.Join(extra, "keep")]; !ok {
		t.Fatalf("missing keep: %v", mgr.binaries)
	}
	// Explicit CLI files bypass configured exclusions.
	mgr.AddExtensionPath("~/agent/extra/drop")
	for _, dir := range mgr.pluginDirs() {
		if err := mgr.scanDir(dir); err != nil {
			t.Fatal(err)
		}
	}
	mgr.filterConfiguredExtensions()
	if _, ok := mgr.binaries[filepath.Join(extra, "drop")]; !ok {
		t.Fatalf("missing CLI file: %v", mgr.binaries)
	}
}

func TestExtensionSettingsKeepScopeFiltersIndependent(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	load := func(scope string) *ai.Settings {
		t.Helper()
		dir := filepath.Join(root, scope)
		if err := os.MkdirAll(filepath.Join(dir, "extra"), 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"keep", "drop"} {
			if err := os.WriteFile(filepath.Join(dir, "extra", name), []byte("binary"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		data := fmt.Sprintf(`{"extensions":["extra","extra/keep*","!%s"]}`, filepath.ToSlash(filepath.Join(root, "project", "extra", "drop")))
		if scope == "project" {
			data = fmt.Sprintf(`{"extensions":["extra","extra/drop*","!%s"]}`, filepath.ToSlash(filepath.Join(root, "user", "extra", "keep")))
		}
		path := filepath.Join(dir, "settings.json")
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		settings, err := ai.LoadSettings(path)
		if err != nil {
			t.Fatal(err)
		}
		return settings
	}
	mgr := NewManager(filepath.Join(root, "user"), nil, nil)
	mgr.SetSettings(ai.MergeSettings(load("user"), load("project")))
	for _, dir := range mgr.pluginDirs() {
		if err := mgr.scanDir(dir); err != nil {
			t.Fatal(err)
		}
	}
	mgr.filterConfiguredExtensions()
	want := []string{filepath.Join(root, "user", "extra", "keep"), filepath.Join(root, "project", "extra", "drop")}
	if len(mgr.binaries) != len(want) {
		t.Fatalf("binaries = %v, want %v", mgr.binaries, want)
	}
	for _, path := range want {
		if _, ok := mgr.binaries[path]; !ok {
			t.Fatalf("missing %s: %v", path, mgr.binaries)
		}
	}
}

func TestLoosePiExtensionsUseManagedShim(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "extensions")
	require.NoError(t, os.Mkdir(dir, 0o700))
	for _, name := range []string{"plain.js", "space's $.ts"} {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte("export default () => {};"), 0o600))
	}
	m := NewManager(cfg, nil, nil)
	require.NoError(t, m.scanDir(dir))
	require.Len(t, m.binaries, 2)
	require.NoError(t, m.scanConfiguredPackages())
	for path, hash := range m.binaries {
		got, manifest, manifestPath, err := m.handshakeMetadata(path)
		require.NoError(t, err)
		require.NotNil(t, manifest)
		require.Equal(t, path, manifestPath)
		require.Equal(t, hash, got)
		want := []string{"node", filepath.Join(m.shimDir(), "index.js"), path}
		require.Equal(t, want, manifest.Run.args)
		node := filepath.Join(cfg, "node fixture")
		require.NoError(t, os.WriteFile(node, []byte("fixture"), 0o700))
		launch := *manifest
		run := *manifest.Run
		run.args = append([]string{node}, manifest.Run.args[1:]...)
		launch.Run = &run
		cmd, err := buildPluginCmd(path, &launch, manifestPath, "")
		require.NoError(t, err)
		require.Equal(t, append([]string{"sh", "-c", pluginDirectScript, node}, want[1:]...), cmd.Args)
		require.Equal(t, dir, cmd.Dir)
		automatic, err := m.packageManifest(manifestPath)
		require.NoError(t, err)
		require.Same(t, manifest, automatic)
		got, err = m.pluginHash(path, automatic, manifestPath)
		require.NoError(t, err)
		require.Equal(t, hash, got)
		m.mu.Lock()
		require.True(t, m.hasShimManifestLocked())
		m.mu.Unlock()
		require.NoError(t, os.WriteFile(path, []byte("export default () => { /* changed */ };"), 0o600))
		got, _, _, err = m.handshakeMetadata(path)
		require.NoError(t, err)
		require.NotEqual(t, hash, got)
	}
}

func TestLoosePiExtensionSettingsAndExplicitCLI(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "extensions")
	require.NoError(t, os.Mkdir(dir, 0o700))
	path := filepath.Join(dir, "entry.ts")
	require.NoError(t, os.WriteFile(path, []byte("export default () => {};"), 0o600))
	m := NewManager(cfg, nil, nil)
	m.SetSettings(&ai.Settings{Extensions: []string{"!" + path}})
	require.NoError(t, m.scanDir(path))
	m.filterConfiguredExtensions()
	require.Empty(t, m.binaries)
	m.AddExtensionPath(path)
	for range 2 {
		require.NoError(t, m.scanDir(path))
		require.NoError(t, m.scanConfiguredPackages())
		m.filterConfiguredExtensions()
		require.Contains(t, m.binaries, path)
		_, manifest, _, err := m.handshakeMetadata(path)
		require.NoError(t, err)
		require.NotNil(t, manifest)
	}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestExtensionRescanRemovesDeletedAndUnconfiguredCandidates(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "extensions")
	extra := t.TempDir()
	require.NoError(t, os.Mkdir(dir, 0o700))
	removed := filepath.Join(dir, "removed.ts")
	retained := filepath.Join(dir, "retained")
	unconfigured := filepath.Join(extra, "other.js")
	require.NoError(t, os.WriteFile(removed, []byte("export default () => {};"), 0o600))
	require.NoError(t, os.WriteFile(retained, []byte("binary"), 0o700))
	require.NoError(t, os.WriteFile(unconfigured, []byte("export default () => {};"), 0o600))
	m := NewManager(cfg, nil, nil)
	m.SetSettings(&ai.Settings{Extensions: []string{extra}})
	require.NoError(t, m.scanExtensionCandidates())
	require.Len(t, m.binaries, 3)
	require.NoError(t, os.Remove(removed))
	m.SetSettings(&ai.Settings{})
	require.NoError(t, m.scanExtensionCandidates())
	require.Len(t, m.binaries, 1)
	require.Contains(t, m.binaries, retained)
	require.NotContains(t, m.manifestPaths, removed)
	require.NotContains(t, m.packageManifests, removed)
	require.NotContains(t, m.manifestPaths, unconfigured)
	require.NotContains(t, m.packageManifests, unconfigured)
}

func TestDirectoryPiExtensionsUseSharedResourceDiscovery(t *testing.T) {
	for _, mode := range []string{"children", "root index", "root manifest"} {
		t.Run(mode, func(t *testing.T) {
			t.Chdir(t.TempDir())
			cfg := t.TempDir()
			dir := filepath.Join(cfg, "extensions")
			write := func(name, data string) string {
				t.Helper()
				path := filepath.Join(dir, name)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
				require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
				return path
			}
			child := write("child/index.ts", "export default () => {};")
			write("child/helper.ts", "not an entry")
			declared := write("declared/main.js", "export default () => {};")
			write("declared/package.json", `{"pi":{"extensions":["main.js"]}}`)
			write(".hidden/index.ts", "hidden")
			write("node_modules/dependency/index.js", "dependency")
			write("ignored/index.ts", "ignored")
			write(".gitignore", "ignored/\n")
			want := []string{child, declared}
			if mode == "root index" {
				want = []string{write("index.ts", "export default () => {};")}
			}
			if mode == "root manifest" {
				write("package.json", `{"pi":{"extensions":["declared/main.js"]}}`)
				want = []string{declared}
			}
			m := NewManager(cfg, nil, nil)
			require.NoError(t, m.scanDir(dir))
			require.Len(t, m.binaries, len(want))
			for _, path := range want {
				hash, ok := m.binaries[path]
				require.True(t, ok, "missing %s", path)
				got, manifest, manifestPath, err := m.handshakeMetadata(path)
				require.NoError(t, err)
				require.Equal(t, hash, got)
				require.Equal(t, path, manifestPath)
				require.Equal(t, []string{"node", filepath.Join(m.shimDir(), "index.js"), path}, manifest.Run.args)
			}
			require.NoError(t, m.scanConfiguredPackages())
			m.filterConfiguredExtensions()
			require.Len(t, m.binaries, len(want))
		})
	}
}

func TestDirectoryPiExtensionDiscoveryReportsMalformedMetadata(t *testing.T) {
	dir := t.TempDir()
	child := filepath.Join(dir, "child")
	require.NoError(t, os.Mkdir(child, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(child, "package.json"), []byte(`{"pi":`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(child, "index.ts"), []byte("entry"), 0o600))
	m := NewManager(t.TempDir(), nil, nil)
	require.ErrorContains(t, m.scanDir(dir), "parse Pi manifest")
	require.Empty(t, m.binaries)
}

func TestReloadNamedRetainsDiscoveryStateOnFailure(t *testing.T) {
	for _, mode := range []string{"extension", "package", "cancellation"} {
		t.Run(mode, func(t *testing.T) {
			t.Chdir(t.TempDir())
			cfg := t.TempDir()
			m := NewManager(cfg, nil, nil)
			original := filepath.Join(cfg, "original.ts")
			require.NoError(t, os.WriteFile(original, []byte("original"), 0o600))
			require.NoError(t, m.scanPiExtensionFile(original))
			hash := m.binaries[original]
			manifest := m.packageManifests[original]
			m.packageBinaries = map[string]bool{original: true}
			ctx := t.Context()
			var want string
			switch mode {
			case "extension":
				dir := filepath.Join(cfg, "extensions", "bad")
				require.NoError(t, os.MkdirAll(dir, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"pi":`), 0o600))
				want = "parse Pi manifest"
			case "package":
				dir := filepath.Join(cfg, "packages", "bad")
				require.NoError(t, os.MkdirAll(dir, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"pi":`), 0o600))
				want = "parse Pi manifest"
			case "cancellation":
				var cancel context.CancelCauseFunc
				ctx, cancel = context.WithCancelCause(ctx)
				cause := errors.New("discovery canceled")
				cancel(cause)
				dir := filepath.Join(cfg, "extensions")
				require.NoError(t, os.MkdirAll(dir, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "native"), []byte("binary"), 0o700))
				want = cause.Error()
			}
			require.ErrorContains(t, m.discoverReloadCandidates(ctx), want)
			require.Equal(t, map[string]string{original: hash}, m.binaries)
			require.Equal(t, map[string]string{original: original}, m.manifestPaths)
			require.Equal(t, map[string]bool{original: true}, m.packageBinaries)
			require.Equal(t, map[string]*Manifest{original: manifest}, m.packageManifests)
		})
	}
}

func TestPreparedReloadDefersAndRetainsDiscoverySnapshot(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	m := NewManager(cfg, nil, nil)
	original := filepath.Join(cfg, "original.ts")
	require.NoError(t, os.WriteFile(original, []byte("original"), 0o600))
	require.NoError(t, m.scanPiExtensionFile(original))
	originalSettings := m.getSettings()
	next := filepath.Join(cfg, "next.ts")
	require.NoError(t, os.WriteFile(next, []byte("next"), 0o600))
	settings := &ai.Settings{Extensions: []string{next}}
	prepared, err := m.PrepareReload(t.Context(), settings)
	require.NoError(t, err)
	require.Same(t, originalSettings, m.getSettings())
	require.Contains(t, m.binaries, original)
	require.NotContains(t, m.binaries, next)
	extra := filepath.Join(cfg, "later.ts")
	require.NoError(t, os.WriteFile(extra, []byte("later"), 0o600))
	var called string
	m.connectPlugin = func(_ context.Context, path string, _ *Manifest, manifestPath string, _ string) (proto.PluginClient, func(), error) {
		called = path
		require.Equal(t, next, manifestPath)
		return &mockPluginClient{gracefulResponse: true}, func() {}, nil
	}
	t.Cleanup(func() { require.NoError(t, m.Shutdown(t.Context())) })
	require.NoError(t, prepared.Reload(t.Context(), "next.ts"))
	require.Equal(t, next, called)
	require.NotContains(t, m.binaries, extra)
	require.Same(t, settings, m.getSettings())
	require.NotContains(t, m.binaries, original)
	require.Contains(t, m.binaries, next)
	require.NotNil(t, m.packageManifests[next])
}

func TestPreparedReloadRejectsCancellationBeforeAdmission(t *testing.T) {
	t.Chdir(t.TempDir())
	m := NewManager(t.TempDir(), nil, nil)
	previous := m.getSettings()
	prepared, err := m.PrepareReload(t.Context(), &ai.Settings{Theme: "new"})
	require.NoError(t, err)
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("reload canceled before commit")
	cancel(cause)
	require.ErrorIs(t, prepared.Reload(ctx, ""), cause)
	require.Same(t, previous, m.getSettings())
	require.Empty(t, m.binaries)
}

func TestPrepareReloadRejectsInvalidNativePluginManifests(t *testing.T) {
	for _, location := range []string{"extensions", "packages"} {
		for _, failure := range []string{"parse", "read", "missing binary"} {
			t.Run(location+"/"+failure, func(t *testing.T) {
				t.Chdir(t.TempDir())
				cfg := t.TempDir()
				root := filepath.Join(cfg, location, "broken")
				require.NoError(t, os.MkdirAll(root, 0o700))
				manifestPath := filepath.Join(root, "tau-package.toml")
				switch failure {
				case "parse":
					require.NoError(t, os.WriteFile(manifestPath, []byte("[plugin]\napi_version = ["), 0o600))
				case "read":
					require.NoError(t, os.Symlink("tau-package.toml", manifestPath))
				case "missing binary":
					require.NoError(t, os.WriteFile(manifestPath, []byte("[plugin]\napi_version = '1'\n"), 0o600))
				}
				manager := NewManager(cfg, nil, nil)
				manager.binaries["retained"] = "hash"
				_, err := manager.PrepareReload(t.Context(), &ai.Settings{})
				require.Error(t, err)
				require.Equal(t, map[string]string{"retained": "hash"}, manager.binaries)
			})
		}
	}
}

func TestNestedNativeRunPluginDiscoveryMatchesStartupIdentity(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	root := filepath.Join(cfg, "extensions", "native-run")
	require.NoError(t, os.MkdirAll(root, 0o700))
	manifestPath := filepath.Join(root, "tau-package.toml")
	require.NoError(t, os.WriteFile(manifestPath, []byte("[plugin]\napi_version='1'\n[run]\ncommand='missing-runtime main.script'\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.script"), []byte("original"), 0o600))
	manager := NewManager(cfg, nil, nil)
	prepared, err := manager.PrepareReload(t.Context(), &ai.Settings{})
	require.NoError(t, err)
	candidates := prepared.candidates
	hash, exists := candidates.binaries[root]
	require.True(t, exists)
	manifest, err := parseManifest(manifestPath)
	require.NoError(t, err)
	expected, err := manager.pluginHash(root, manifest, manifestPath)
	require.NoError(t, err)
	require.Equal(t, expected, hash)
	require.Equal(t, manifestPath, candidates.manifestPaths[root])
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.script"), []byte("changed"), 0o600))
	next, err := manager.PrepareReload(t.Context(), &ai.Settings{})
	require.NoError(t, err)
	require.NotEqual(t, hash, next.candidates.binaries[root])
}

func TestNativeExtensionScanFollowsTargetTypeAndReportsFailures(t *testing.T) {
	for _, scenario := range []string{"linked directory", "stat failure", "broken link"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			manager := NewManager(t.TempDir(), nil, nil)
			path := filepath.Join(root, "candidate")
			switch scenario {
			case "linked directory":
				require.NoError(t, os.Mkdir(filepath.Join(root, "support"), 0o700))
				require.NoError(t, os.Symlink("support", path))
			case "stat failure":
				require.NoError(t, os.Symlink("candidate", path))
			case "broken link":
				require.NoError(t, os.Symlink("missing", path))
			}
			err := manager.scanDir(root)
			switch scenario {
			case "stat failure":
				require.Error(t, err)
			default:
				require.NoError(t, err)
			}
			require.Empty(t, manager.binaries)
		})
	}
}

func TestPreparedReloadKeepsLivePolicyWhileShimSetupWaits(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	root := filepath.Join(cfg, "packages", "shim-plugin")
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "extension.js"), []byte("extension"), 0o600))
	manifest := fmt.Sprintf("[plugin]\napi_version='1'\n[run]\ncommand='node %s extension.js'\n", filepath.Join(cfg, "pi-ext-shim", "index.js"))
	require.NoError(t, os.WriteFile(filepath.Join(root, "tau-package.toml"), []byte(manifest), 0o600))
	entered, release := make(chan struct{}), make(chan struct{})
	failure := errors.New("shim installation rejected")
	manager := NewManager(cfg, nil, nil, WithShimInstall(func(context.Context, string) error { close(entered); <-release; return failure }))
	old := &ai.Settings{Theme: "original"}
	manager.SetSettings(old)
	prepared, err := manager.PrepareReload(t.Context(), &ai.Settings{Theme: "proposed"})
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- prepared.Reload(t.Context(), "") }()
	<-entered
	observed := manager.getSettings()
	close(release)
	err = <-done
	require.ErrorIs(t, err, failure)
	require.Same(t, old, observed)
	require.Same(t, old, manager.getSettings())
	require.Empty(t, manager.plugins)
}

func TestNamedPreparedReloadDoesNotInstallUnselectedShim(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	root := filepath.Join(cfg, "packages", "unselected")
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "extension.js"), []byte("extension"), 0o600))
	manifest := fmt.Sprintf("[plugin]\napi_version='1'\n[run]\ncommand='node %s extension.js'\n", filepath.Join(cfg, "pi-ext-shim", "index.js"))
	require.NoError(t, os.WriteFile(filepath.Join(root, "tau-package.toml"), []byte(manifest), 0o600))
	calls := 0
	manager := NewManager(cfg, nil, nil, WithShimInstall(func(context.Context, string) error { calls++; return errors.New("unselected shim") }))
	prepared, err := manager.PrepareReload(t.Context(), &ai.Settings{})
	require.NoError(t, err)
	require.NoError(t, prepared.Reload(t.Context(), "selected"))
	require.Equal(t, 0, calls)
}

func TestExplicitNativeExtensionDirectoryUsesOwnManifest(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	root := filepath.Join(t.TempDir(), "native")
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "tau-package.toml"), []byte("[plugin]\napi_version='1'\n[run]\ncommand='node main.js'\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.js"), []byte("extension"), 0o600))
	manager := NewManager(cfg, nil, nil)
	manager.AddExtensionPath(root)
	prepared, err := manager.PrepareReload(t.Context(), &ai.Settings{})
	require.NoError(t, err)
	require.Contains(t, prepared.candidates.binaries, root)
	require.Equal(t, filepath.Join(root, "tau-package.toml"), prepared.candidates.manifestPaths[root])
	require.Len(t, prepared.candidates.binaries, 1)
}

func TestPreparedReloadRestartsPluginWhenItsConfigurationChanges(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "extensions")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "plugin")
	require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
	calls := 0
	manager := NewManager(cfg, nil, nil, WithPluginConnector(func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		calls++
		return &mockPluginClient{gracefulResponse: true}, func() {}, nil
	}))
	old := &ai.Settings{Plugins: map[string]json.RawMessage{"plugin": json.RawMessage(`{"mode":"old"}`)}}
	manager.SetSettings(old)
	_, err := manager.Load(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Shutdown(t.Context())) })
	next := &ai.Settings{Plugins: map[string]json.RawMessage{"plugin": json.RawMessage(`{"mode":"new"}`)}}
	skipped, err := manager.PrepareReload(t.Context(), next)
	require.NoError(t, err)
	require.NoError(t, skipped.Reload(t.Context(), "other"))
	require.Equal(t, 1, calls)

	prepared, err := manager.PrepareReload(t.Context(), next)
	require.NoError(t, err)
	require.NoError(t, prepared.Reload(t.Context(), ""))
	require.Equal(t, 2, calls)
	unchanged, err := manager.PrepareReload(t.Context(), next)
	require.NoError(t, err)
	require.NoError(t, unchanged.Reload(t.Context(), ""))
	require.Equal(t, 2, calls)
}
