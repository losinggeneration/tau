package plugins

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

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
