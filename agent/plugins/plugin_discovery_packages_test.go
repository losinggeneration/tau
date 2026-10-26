package plugins

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/losinggeneration/tau/agent/packages"

	"github.com/losinggeneration/tau/ai"
)

func TestUserPackagesDirResolution(t *testing.T) {
	t.Setenv("TAU_PACKAGE_DIR", "")
	cfgDir := t.TempDir()
	mgr := NewManager(cfgDir, nil, nil)

	// Default: <configDir>/packages.
	if want := filepath.Join(cfgDir, "packages"); mgr.userPackageDir() != want {
		t.Fatalf("default = %q, want %q", mgr.userPackageDir(), want)
	}

	// settings.json packageDir wins over the default.
	mgr.SetSettings(&ai.Settings{PackageDir: "/settings/pkgs"})
	if mgr.userPackageDir() != "/settings/pkgs" {
		t.Fatalf("settings = %q, want /settings/pkgs", mgr.userPackageDir())
	}

	// TAU_PACKAGE_DIR wins over settings.
	t.Setenv("TAU_PACKAGE_DIR", "/env/pkgs")
	if mgr.userPackageDir() != "/env/pkgs" {
		t.Fatalf("env = %q, want /env/pkgs", mgr.userPackageDir())
	}
}

func TestScanConfiguredPackagesAppliesExtensionPolicyAndLocalSources(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("TAU_PACKAGE_DIR", "")
	cfg := filepath.Join(root, "user")
	pkg := filepath.Join(cfg, "packages", "example")
	local := filepath.Join(cfg, "local")
	for _, dir := range []string{pkg, local} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "tau-package.toml"), []byte("[package]\nname = 'example'\nversion = '1'\n[plugin]\napi_version = '1'\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "plugin"), []byte("binary"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := (packages.InstallSidecar{Source: "npm:example@1"}).Write(pkg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg, "settings.json")
	save := func(raw string) *ai.Settings {
		t.Helper()
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := ai.LoadSettings(path)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	m := NewManager(cfg, nil, nil)
	m.SetSettings(save(`{"packages":[{"source":"npm:example@1","extensions":[]},"./local"]}`))
	if err := m.scanConfiguredPackages(); err != nil {
		t.Fatal(err)
	}
	if len(m.binaries) != 1 {
		t.Fatalf("binaries = %v", m.binaries)
	}
	if _, ok := m.binaries[filepath.Join(local, "plugin")]; !ok {
		t.Fatalf("local binary missing: %v", m.binaries)
	}
	m.SetSettings(save(`{"packages":[{"source":"npm:example@1","autoload":false,"extensions":["+plugin"]},{"source":"./local","autoload":false}]}`))
	if err := m.scanConfiguredPackages(); err != nil {
		t.Fatal(err)
	}
	if len(m.binaries) != 1 {
		t.Fatalf("binaries after replacement = %v", m.binaries)
	}
	if _, ok := m.binaries[filepath.Join(pkg, "plugin")]; !ok {
		t.Fatalf("enabled binary missing: %v", m.binaries)
	}
}

func TestConfiguredPackagePolicyPreservesExplicitCLIExtension(t *testing.T) {
	root := t.TempDir()
	t.Chdir(t.TempDir())
	t.Setenv("TAU_PACKAGE_DIR", "")
	pkg := filepath.Join(root, "packages", "example")
	if err := os.MkdirAll(pkg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "tau-package.toml"), []byte("[package]\nname = 'example'\n[plugin]\napi_version = '1'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(pkg, "plugin")
	if err := os.WriteFile(binary, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	m := NewManager(root, nil, nil)
	m.SetSettings(&ai.Settings{Packages: []ai.PackageSource{{Source: "npm:example", Extensions: []string{}}}})
	m.AddExtensionPath(binary)
	if err := m.scanDir(binary); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := m.scanConfiguredPackages(); err != nil {
			t.Fatal(err)
		}
		if _, ok := m.binaries[binary]; !ok {
			t.Fatal("package filtering removed explicit CLI extension")
		}
	}
}

func TestConfiguredPiEntriesReachLaunchAndReloadIdentity(t *testing.T) {
	cfg := t.TempDir()
	t.Chdir(t.TempDir())
	t.Setenv("TAU_PACKAGE_DIR", "")
	root := filepath.Join(cfg, "packages", "example")
	require.NoError(t, os.MkdirAll(root, 0o700))
	manifestPath := filepath.Join(root, "tau-package.toml")
	original := []byte("[package]\nname='example'\n[plugin]\napi_version='1'\n[run]\ncommand='/bin/sh /managed/pi-ext-shim/index.js keep.js drop.js'\n")
	require.NoError(t, os.WriteFile(manifestPath, original, 0o600))
	m := NewManager(cfg, nil, nil)
	m.SetSettings(&ai.Settings{Packages: []ai.PackageSource{{Source: "npm:example", Extensions: []string{"keep.js"}}}})
	require.NoError(t, m.scanConfiguredPackages())
	hash, manifest, path, err := m.handshakeMetadata(root)
	require.NoError(t, err)
	require.NotNil(t, manifest)
	cmd, err := buildPluginCmd(root, manifest, path, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"sh", "-c", pluginDirectScript, "/bin/sh", "/managed/pi-ext-shim/index.js", "keep.js"}, cmd.Args)
	assert.Equal(t, hash, m.binaries[root], "discovery and launch disagree about identity")
	m.mu.Lock()
	loaded, err := m.packageManifest(manifestPath)
	m.mu.Unlock()
	require.NoError(t, err)
	assert.Equal(t, manifest, loaded, "automatic load lost the filtered manifest")
	m.SetSettings(&ai.Settings{Packages: []ai.PackageSource{{Source: "npm:example", Autoload: new(false), Extensions: []string{"+drop.js"}}}})
	require.NoError(t, m.scanConfiguredPackages())
	nextHash, manifest, path, err := m.handshakeMetadata(root)
	require.NoError(t, err)
	assert.NotEqual(t, hash, nextHash, "changed entry selection did not trigger reload")
	cmd, err = buildPluginCmd(root, manifest, path, "")
	require.NoError(t, err)
	assert.Equal(t, "drop.js", cmd.Args[len(cmd.Args)-1])
	assert.NotContains(t, cmd.Args, "keep.js")
	m.SetSettings(&ai.Settings{Packages: []ai.PackageSource{{Source: "npm:example", Extensions: []string{}}}})
	require.NoError(t, m.scanConfiguredPackages())
	assert.NotContains(t, m.binaries, root, "empty entry selection still launches the shim")
	assert.Empty(t, m.packageManifests, "stale filtered launch configuration survived rescan")
	data, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	assert.Equal(t, original, data, "runtime policy rewrote installed metadata")
}

func TestConfiguredPiEntriesPreserveCLIOverrideAndLiteralArguments(t *testing.T) {
	cfg := t.TempDir()
	t.Chdir(t.TempDir())
	t.Setenv("TAU_PACKAGE_DIR", "")
	root := filepath.Join(cfg, "packages", "example")
	require.NoError(t, os.MkdirAll(root, 0o700))
	manifestPath := filepath.Join(root, "tau-package.toml")
	require.NoError(t, os.WriteFile(manifestPath, []byte("[plugin]\napi_version='1'\n[run]\ncommand=\"/bin/sh /managed/pi-ext-shim/index.js 'literal $name.js' drop.js\"\n"), 0o600))
	m := NewManager(cfg, nil, nil)
	m.SetSettings(&ai.Settings{Packages: []ai.PackageSource{{Source: "npm:example", Extensions: []string{"*.js", "!drop.js"}}}})
	require.NoError(t, m.scanConfiguredPackages())
	hash, manifest, path, err := m.handshakeMetadata(root)
	require.NoError(t, err)
	cmd, err := buildPluginCmd(root, manifest, path, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"sh", "-c", pluginDirectScript, "/bin/sh", "/managed/pi-ext-shim/index.js", "literal $name.js"}, cmd.Args, "selected filenames must not be evaluated by a shell")
	require.NoError(t, m.scanConfiguredPackages())
	assert.Equal(t, hash, m.binaries[root], "unchanged selection should have stable identity")
	m.AddExtensionPath(root)
	m.SetSettings(&ai.Settings{Packages: []ai.PackageSource{{Source: "npm:example", Extensions: []string{}}}})
	for range 2 {
		require.NoError(t, m.scanConfiguredPackages())
		assert.Contains(t, m.binaries, root, "explicit CLI package was disabled")
		assert.Empty(t, m.packageManifests, "CLI override retained a filtered manifest")
		_, manifest, _, err = m.handshakeMetadata(root)
		require.NoError(t, err)
		assert.Nil(t, manifest.Run.args)
		assert.Contains(t, manifest.Run.Command, "drop.js")
	}
}

func TestConfiguredPiManifestGlobsReachSelectedLaunch(t *testing.T) {
	cfg := t.TempDir()
	t.Chdir(t.TempDir())
	t.Setenv("TAU_PACKAGE_DIR", "")
	root := filepath.Join(cfg, "packages", "example")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "ext"), 0o700))
	for _, name := range []string{"keep.js", "drop.js"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, "ext", name), []byte("source"), 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "tau-package.toml"), []byte("[plugin]\napi_version='1'\n[run]\ncommand='/bin/sh /managed/pi-ext-shim/index.js stale.js'\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"pi":{"extensions":["ext/*.js","!ext/drop.js"]}}`), 0o600))
	m := NewManager(cfg, nil, nil)
	require.NoError(t, m.scanConfiguredPackages())
	_, manifest, path, err := m.handshakeMetadata(root)
	require.NoError(t, err)
	cmd, err := buildPluginCmd(root, manifest, path, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"sh", "-c", pluginDirectScript, "/bin/sh", "/managed/pi-ext-shim/index.js", filepath.Join(root, "ext", "keep.js")}, cmd.Args)
	require.NoError(t, os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"pi":{"extensions":[]}}`), 0o600))
	require.NoError(t, m.scanConfiguredPackages())
	assert.NotContains(t, m.binaries, root, "empty Pi extensions declaration reused stale generated argv")
}

func TestLocalPiPackageLaunchWithoutGeneratedManifest(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("TAU_PACKAGE_DIR", "")
	cfg := filepath.Join(t.TempDir(), "config with 'quotes' and $dollars")
	root := filepath.Join(t.TempDir(), "local package")
	require.NoError(t, os.MkdirAll(root, 0o700))
	for _, name := range []string{"literal $name.js", "drop.js"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("export default function() {}"), 0o600))
	}
	original := []byte(`{"pi":{"extensions":["*.js"]}}`)
	require.NoError(t, os.WriteFile(filepath.Join(root, "package.json"), original, 0o600))
	m := NewManager(cfg, nil, nil)
	m.SetSettings(&ai.Settings{Packages: []ai.PackageSource{{Source: root, Extensions: []string{"*.js", "!drop.js"}}}})
	require.NoError(t, m.scanConfiguredPackages())
	require.Contains(t, m.binaries, root, "local Pi extension was never registered")
	hash, manifest, path, err := m.handshakeMetadata(root)
	require.NoError(t, err)
	require.NotNil(t, manifest)
	assert.Equal(t, filepath.Join(root, "package.json"), path)
	assert.Equal(t, hash, m.binaries[root])
	cmd, err := buildPluginCmd(root, manifest, path, "")
	require.NoError(t, err)
	assert.Equal(t, root, cmd.Dir)
	require.Len(t, cmd.Args, 6)
	assert.Equal(t, []string{"sh", "-c", pluginDirectScript}, cmd.Args[:3])
	assert.Equal(t, filepath.Join(cfg, "pi-ext-shim", "index.js"), cmd.Args[4])
	assert.Equal(t, filepath.Join(root, "literal $name.js"), cmd.Args[5])
	m.mu.Lock()
	automatic, err := m.packageManifest(path)
	needsShim := m.hasShimManifestLocked()
	m.mu.Unlock()
	require.NoError(t, err)
	assert.Equal(t, manifest, automatic)
	assert.True(t, needsShim, "in-memory Pi manifest skipped shim preparation")
	autoHash, err := m.pluginHash(root, automatic, path)
	require.NoError(t, err)
	assert.Equal(t, hash, autoHash)
	require.NoError(t, m.scanConfiguredPackages())
	assert.Equal(t, hash, m.binaries[root], "unchanged local launch identity changed")
	m.SetSettings(&ai.Settings{Packages: []ai.PackageSource{{Source: root, Extensions: []string{}}}})
	require.NoError(t, m.scanConfiguredPackages())
	assert.NotContains(t, m.binaries, root)
	m.AddExtensionPath(root)
	for range 2 {
		require.NoError(t, m.scanConfiguredPackages())
		_, manifest, path, err = m.handshakeMetadata(root)
		require.NoError(t, err)
		cmd, err = buildPluginCmd(root, manifest, path, "")
		require.NoError(t, err)
		assert.Contains(t, cmd.Args, filepath.Join(root, "drop.js"), "CLI override was filtered")
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"pi":{"extensions":[]}}`), 0o600))
	require.NoError(t, m.scanConfiguredPackages())
	assert.NotContains(t, m.binaries, root, "empty declaration retained stale local launch")
	_, err = os.Stat(filepath.Join(root, "tau-package.toml"))
	assert.ErrorIs(t, err, os.ErrNotExist, "discovery wrote into local package")
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.Len(t, entries, 3, "discovery created package files")
}

func TestLocalPiPackageEmptyResourcesAndMalformedManifest(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("TAU_PACKAGE_DIR", "")
	cfg, root := t.TempDir(), t.TempDir()
	m := NewManager(cfg, nil, nil)
	m.SetSettings(&ai.Settings{Packages: []ai.PackageSource{{Source: root}}})
	for _, body := range []string{`{}`, `{"pi":{}}`, `{"pi":{"extensions":[],"prompts":["prompts"]}}`} {
		require.NoError(t, os.WriteFile(filepath.Join(root, "package.json"), []byte(body), 0o600))
		require.NoError(t, m.scanConfiguredPackages())
		assert.Empty(t, m.binaries)
		m.mu.Lock()
		needsShim := m.hasShimManifestLocked()
		m.mu.Unlock()
		assert.False(t, needsShim, "resource-only package scheduled shim setup")
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"pi":{"extensions":"wrong"}}`), 0o600))
	require.ErrorContains(t, m.scanConfiguredPackages(), "parse Pi manifest")
	assert.Empty(t, m.binaries)
}

func TestGeneratedPiPackageQuotedCommandDiscovery(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("TAU_PACKAGE_DIR", "")
	cfg, root := t.TempDir(), t.TempDir()
	shim := filepath.Join(cfg, "shim's $dir", "pi-ext-shim")
	node := filepath.Join(cfg, "node bin's")
	require.NoError(t, os.WriteFile(node, []byte("node fixture"), 0o700))
	entries := []string{"space name.js", "apostrophe's.js", "$HOME.js", "$(touch injected).js"}
	for _, entry := range entries {
		require.NoError(t, os.WriteFile(filepath.Join(root, entry), []byte("extension"), 0o600))
	}
	data, err := packages.GenerateManifest(&packages.PiPackage{Name: "quoted", Version: "1", Extensions: entries}, shim, node)
	require.NoError(t, err)
	manifestPath := filepath.Join(root, "tau-package.toml")
	require.NoError(t, os.WriteFile(manifestPath, data, 0o600))
	m := NewManager(cfg, nil, nil)
	m.SetSettings(&ai.Settings{Packages: []ai.PackageSource{{Source: root}}})
	require.NoError(t, m.scanConfiguredPackages())
	require.Len(t, m.binaries, 1)
	want := append([]string{node, filepath.Join(shim, "index.js")}, entries...)
	hash, manifest, path, err := m.handshakeMetadata(root)
	require.NoError(t, err)
	require.Equal(t, manifestPath, path)
	assert.Equal(t, m.binaries[root], hash)
	require.Equal(t, want, manifest.Run.args)
	cmd, err := buildPluginCmd(root, manifest, path, "")
	require.NoError(t, err)
	assert.Equal(t, append([]string{"sh", "-c", pluginDirectScript}, want...), cmd.Args)
	m.AddExtensionPath(root)
	for range 2 {
		require.NoError(t, m.scanConfiguredPackages())
		hash, manifest, path, err = m.handshakeMetadata(root)
		require.NoError(t, err)
		assert.Equal(t, m.binaries[root], hash)
		assert.Nil(t, manifest.Run.args)
		assert.Equal(t, append([]string{node, filepath.Join(shim, "index.js")}, entries...), splitCommand(manifest.Run.Command))
		cmd, err = buildPluginCmd(root, manifest, path, "")
		require.NoError(t, err)
		assert.Equal(t, []string{"sh", "-c", pluginShellScript, "sh", manifest.Run.Command, root}, cmd.Args)
	}
	unchanged, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	assert.Equal(t, data, unchanged)
}

func TestPackageDiscoveryRecognizesTOMLPluginTables(t *testing.T) {
	for _, header := range []string{"[plugin] # plugin configuration", `["plugin"]`, `['plugin']`} {
		t.Run(header, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "tau-package.toml"), []byte(header+"\napi_version='1'\n[run]\ncommand='node extension.js'\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(root, "extension.js"), []byte("extension"), 0o600))
			manager := NewManager(t.TempDir(), nil, nil)
			require.NoError(t, manager.scanPackageDir(root))
			require.Contains(t, manager.binaries, root)
			require.Equal(t, filepath.Join(root, "tau-package.toml"), manager.manifestPaths[root])
			nested := NewManager(t.TempDir(), nil, nil)
			require.NoError(t, nested.scanNestedManifestPlugins(filepath.Dir(root)))
			require.Contains(t, nested.binaries, root)
			require.Equal(t, filepath.Join(root, "tau-package.toml"), nested.manifestPaths[root])
		})
	}
}

func TestFindPluginBinaryFollowsSymlinkTargetType(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "support"), 0o700))
	require.NoError(t, os.Symlink("support", filepath.Join(root, "a-support")))
	executable := filepath.Join(root, "z-plugin")
	require.NoError(t, os.WriteFile(executable, []byte("binary"), 0o700))
	selected, err := findPluginBinary(root, nil)
	require.NoError(t, err)
	require.Equal(t, executable, selected)
	require.NoError(t, os.Remove(executable))
	require.NoError(t, os.WriteFile(filepath.Join(root, "support", "real"), []byte("binary"), 0o700))
	require.NoError(t, os.Symlink(filepath.Join("support", "real"), executable))
	selected, err = findPluginBinary(root, nil)
	require.NoError(t, err)
	require.Equal(t, executable, selected)
}

func TestFindPluginBinaryReportsTargetStatFailure(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Symlink("loop", filepath.Join(root, "loop")))
	require.NoError(t, os.WriteFile(filepath.Join(root, "z-plugin"), []byte("binary"), 0o700))
	_, err := findPluginBinary(root, nil)
	require.Error(t, err)
	var pathErr *os.PathError
	require.ErrorAs(t, err, &pathErr)
}

func TestNativePluginHashIncludesManifestChanges(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	root := filepath.Join(cfg, "packages", "native")
	require.NoError(t, os.MkdirAll(root, 0o700))
	binary := filepath.Join(root, "plugin")
	require.NoError(t, os.WriteFile(binary, []byte("binary"), 0o700))
	manifestPath := filepath.Join(root, "tau-package.toml")
	write := func(prefix string) {
		t.Helper()
		require.NoError(t, os.WriteFile(manifestPath, []byte("[plugin]\napi_version='1'\ntool_prefix='"+prefix+"'\n"), 0o600))
	}
	write("old_")
	manager := NewManager(cfg, nil, nil)
	first, err := manager.PrepareReload(t.Context(), &ai.Settings{})
	require.NoError(t, err)
	before := first.candidates.binaries[binary]
	require.NotEmpty(t, before)
	write("new_")
	second, err := manager.PrepareReload(t.Context(), &ai.Settings{})
	require.NoError(t, err)
	after := second.candidates.binaries[binary]
	require.NotEqual(t, before, after)
	hash, _, selected, err := manager.handshakeMetadata(binary)
	require.NoError(t, err)
	require.Equal(t, manifestPath, selected)
	require.Equal(t, after, hash)
	third, err := manager.PrepareReload(t.Context(), &ai.Settings{})
	require.NoError(t, err)
	require.Equal(t, after, third.candidates.binaries[binary])
}
