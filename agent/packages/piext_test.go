package packages

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

func TestParsePiPackage(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		files   map[string]string
		want    *PiPackage
		wantErr string
	}{
		{
			name:    "missing pi field",
			json:    `{"name":"plain","version":"1.0.0"}`,
			wantErr: notPiPackageError,
		},
		{
			name: "skills only",
			json: `{"name":"plain","version":"1.0.0","pi":{"skills":["skills"]}}`,
			want: &PiPackage{Name: "plain", Version: "1.0.0", Skills: []string{"skills"}},
		},
		{
			name: "missing skills tolerated",
			json: `{"name":"plain","version":"1.0.0","description":"Plain","pi":{"extensions":["./pi-extension/index.js"]}}`,
			want: &PiPackage{
				Name:        "plain",
				Version:     "1.0.0",
				Description: "Plain",
				Extensions:  []string{"pi-extension/index.js"},
			},
		},
		{
			name: "scoped with deps",
			json: `{"name":"@scope/pkg","version":"2.0.0","dependencies":{"left-pad":"1.3.0"},"peerDependencies":{"@earendil-works/pi-coding-agent":"*"},"devDependencies":{"typescript":"5"},"pi":{"extensions":["./ext.js"],"skills":["./skills"]}}`,
			want: &PiPackage{
				Name:       "@scope/pkg",
				Version:    "2.0.0",
				Extensions: []string{"ext.js"},
				Skills:     []string{"./skills"},
				PeerDeps:   []string{"@earendil-works/pi-coding-agent"},
				HasDeps:    true,
			},
		},
		{
			name: "dev deps do not count",
			json: `{"name":"devonly","version":"1.0.0","devDependencies":{"typescript":"5"},"pi":{"extensions":["./ext.js"]}}`,
			want: &PiPackage{Name: "devonly", Version: "1.0.0", Extensions: []string{"ext.js"}},
		},
		{
			name:  "root directory extension resolves index",
			json:  `{"name":"root-entry","version":"1.0.0","pi":{"extensions":["./"]}}`,
			files: map[string]string{"index.ts": "export default function() {}"},
			want:  &PiPackage{Name: "root-entry", Version: "1.0.0", Extensions: []string{"index.ts"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(tc.json), 0o600); err != nil {
				t.Fatalf("write package.json: %v", err)
			}
			for name, body := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
					t.Fatalf("write fixture file: %v", err)
				}
			}

			got, err := ParsePiPackage(dir)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParsePiPackage() error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePiPackage() error = %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParsePiPackage() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestGenerateManifest(t *testing.T) {
	pkg := &PiPackage{
		Name:        "@scope/pkg",
		Version:     "1.2.3",
		Description: "Useful",
		Extensions:  []string{"./pi-extension/index.js", "other.js"},
		Skills:      []string{"./skills"},
	}

	got, err := GenerateManifest(pkg, "/home/me/.tau/agent/pi-ext-shim", "/usr/bin/node")
	if err != nil {
		t.Fatalf("GenerateManifest() error = %v", err)
	}

	want := `[package]
name = "@scope/pkg"
version = "1.2.3"
description = "Useful (pi extension via pi-ext-shim)"

[plugin]
api_version = "1"

[run]
command = "/usr/bin/node /home/me/.tau/agent/pi-ext-shim/index.js pi-extension/index.js other.js"

[[skills]]
path = "skills"
`
	if string(got) != want {
		t.Fatalf("GenerateManifest() =\n%s\nwant\n%s", got, want)
	}
}

func TestInstallPiLocalOfflineDeps(t *testing.T) {
	t.Setenv("PI_OFFLINE", "1")

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "package.json"), []byte(`{"name":"pi-pkg","version":"1.0.0","dependencies":{"left-pad":"1.3.0"},"pi":{"extensions":["./ext.js"]}}`), 0o600); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "ext.js"), []byte("export default function() {}"), 0o600); err != nil {
		t.Fatalf("write ext.js: %v", err)
	}

	mgr := NewInstallManager(filepath.Join(t.TempDir(), "packages"), filepath.Join(t.TempDir(), "project"))
	mgr.npmInstall = func(context.Context, string, []string) error {
		t.Fatal("npm install should not run offline")
		return nil
	}
	mgr.ensureShim = func(context.Context, string) error { return nil }
	mgr.lookPath = func(string) (string, error) { return "/usr/bin/node", nil }

	err := mgr.Install(context.Background(), Config{Source: src, Yes: true, Pi: true})
	if err == nil || !strings.Contains(err.Error(), "tau install --pi needs network; re-run online") {
		t.Fatalf("Install() error = %v, want offline network error", err)
	}
}

func TestInstallPiInstallsPeerDependencies(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "package.json"), []byte(`{"name":"pi-pkg","version":"1.0.0","peerDependencies":{"@earendil-works/pi-coding-agent":"*"},"pi":{"extensions":["./ext.js"]}}`), 0o600); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "ext.js"), []byte("export default function() {}"), 0o600); err != nil {
		t.Fatalf("write ext.js: %v", err)
	}

	mgr := NewInstallManager(filepath.Join(t.TempDir(), "packages"), filepath.Join(t.TempDir(), "project"))
	mgr.npmInstall = func(_ context.Context, _ string, peerDeps []string) error {
		if !reflect.DeepEqual(peerDeps, []string{"@earendil-works/pi-coding-agent"}) {
			t.Fatalf("peer deps = %#v", peerDeps)
		}
		return nil
	}
	mgr.ensureShim = func(context.Context, string) error { return nil }
	mgr.lookPath = func(string) (string, error) { return "/usr/bin/node", nil }

	if err := mgr.Install(context.Background(), Config{Source: src, Yes: true, Pi: true}); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
}

func TestInstallPiLocalWritesManifestAndSidecar(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "package.json"), []byte(`{"name":"@scope/pi-pkg","version":"1.0.0","pi":{"extensions":["./ext.js"],"skills":["./skills"]}}`), 0o600); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "ext.js"), []byte("export default function() {}"), 0o600); err != nil {
		t.Fatalf("write ext.js: %v", err)
	}

	configDir := t.TempDir()
	userDir := filepath.Join(configDir, "packages")
	mgr := NewInstallManager(userDir, filepath.Join(t.TempDir(), "project"))
	mgr.npmInstall = func(context.Context, string, []string) error {
		t.Fatal("npm install should not run without deps")
		return nil
	}
	mgr.ensureShim = func(_ context.Context, dir string) error {
		if dir != filepath.Join(configDir, "pi-ext-shim") {
			t.Fatalf("ensure shim dir = %q", dir)
		}
		return nil
	}
	mgr.lookPath = func(name string) (string, error) {
		if name != "node" {
			t.Fatalf("lookPath(%q), want node", name)
		}
		return "/usr/bin/node", nil
	}

	if err := mgr.Install(context.Background(), Config{Source: src, Yes: true, Pi: true}); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	installPath := filepath.Join(userDir, "@scope", "pi-pkg")
	manifest, err := os.ReadFile(filepath.Join(installPath, "tau-package.toml"))
	if err != nil {
		t.Fatalf("read generated manifest: %v", err)
	}
	if !strings.Contains(string(manifest), `command = "/usr/bin/node `+filepath.Join(configDir, "pi-ext-shim", "index.js")+` ext.js"`) {
		t.Fatalf("manifest command shape wrong:\n%s", manifest)
	}

	sidecar, err := LoadSidecar(installPath)
	if err != nil {
		t.Fatalf("LoadSidecar() error = %v", err)
	}
	if !sidecar.Pi {
		t.Fatalf("sidecar Pi = false, want true")
	}
}

func TestInstallPiNPMUsesPackAndWritesPackage(t *testing.T) {
	configDir := t.TempDir()
	userDir := filepath.Join(configDir, "packages")
	mgr := NewInstallManager(userDir, "")
	mgr.npmPack = func(_ context.Context, spec, dst string) (string, error) {
		if spec != "@scope/pi-pkg@1.2.3" {
			t.Fatalf("npm pack spec = %q", spec)
		}
		return writePiTarball(t, dst, `{"name":"@scope/pi-pkg","version":"1.2.3","pi":{"extensions":["./ext.js"]}}`), nil
	}
	mgr.npmInstall = func(context.Context, string, []string) error {
		t.Fatal("npm install should not run without deps")
		return nil
	}
	mgr.ensureShim = func(context.Context, string) error { return nil }
	mgr.lookPath = func(string) (string, error) { return "/usr/bin/node", nil }

	if err := mgr.Install(context.Background(), Config{Source: "npm:@scope/pi-pkg@1.2.3", Yes: true, Pi: true}); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	installPath := filepath.Join(userDir, "@scope", "pi-pkg")
	if _, err := os.Stat(filepath.Join(installPath, "tau-package.toml")); err != nil {
		t.Fatalf("stat manifest: %v", err)
	}
	sidecar, err := LoadSidecar(installPath)
	if err != nil {
		t.Fatalf("LoadSidecar() error = %v", err)
	}
	if !sidecar.Pi || !sidecar.Pinned || sidecar.Ref != "1.2.3" {
		t.Fatalf("sidecar = %#v, want pi pinned ref 1.2.3", sidecar)
	}
}

func TestInstallPiNPMOfflineSkipsPack(t *testing.T) {
	t.Setenv("PI_OFFLINE", "1")
	mgr := NewInstallManager(t.TempDir(), "")
	mgr.npmPack = func(context.Context, string, string) (string, error) {
		t.Fatal("npm pack should not run offline")
		return "", nil
	}
	err := mgr.Install(context.Background(), Config{Source: "npm:pi-pkg", Yes: true, Pi: true})
	if err == nil || !strings.Contains(err.Error(), "tau install --pi needs network; re-run online") {
		t.Fatalf("Install() error = %v, want offline network error", err)
	}
}

func writePiTarball(t *testing.T, dst, packageJSON string) string {
	t.Helper()
	path := filepath.Join(dst, "pi-pkg.tgz")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create tarball: %v", err)
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	files := map[string]string{
		"package/package.json": packageJSON,
		"package/ext.js":       "export default function() {}",
	}
	for name, body := range files {
		hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("write tar body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close tarball: %v", err)
	}
	return path
}

func TestInstallPiGitInstallsPackage(t *testing.T) {
	tmpDir := t.TempDir()
	bareDir := filepath.Join(tmpDir, "repo.git")
	_, err := git.PlainInit(bareDir, true)
	if err != nil {
		t.Fatalf("init bare repo: %v", err)
	}
	workDir := filepath.Join(tmpDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("create work dir: %v", err)
	}
	repo, err := git.PlainInit(workDir, false)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "package.json"), []byte(`{"name":"git-pi","version":"1.0.0","pi":{"extensions":["./ext.js"]}}`), 0o600); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "ext.js"), []byte("export default function() {}"), 0o600); err != nil {
		t.Fatalf("write ext.js: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if _, err := wt.Add("."); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := wt.Commit("add pi", &git.CommitOptions{Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()}}); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bareDir}}); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	if err := repo.Push(&git.PushOptions{RemoteName: "origin"}); err != nil {
		t.Fatalf("git push: %v", err)
	}

	configDir := t.TempDir()
	userDir := filepath.Join(configDir, "packages")
	mgr := NewInstallManager(userDir, "")
	mgr.npmInstall = func(context.Context, string, []string) error {
		t.Fatal("npm install should not run without deps")
		return nil
	}
	mgr.ensureShim = func(context.Context, string) error { return nil }
	mgr.lookPath = func(string) (string, error) { return "/usr/bin/node", nil }

	if err := mgr.Install(context.Background(), Config{Source: "file://" + bareDir, Yes: true, Pi: true}); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	installPath := filepath.Join(userDir, "git-pi")
	if _, err := os.Stat(filepath.Join(installPath, "tau-package.toml")); err != nil {
		t.Fatalf("stat manifest: %v", err)
	}
	sidecar, err := LoadSidecar(installPath)
	if err != nil {
		t.Fatalf("LoadSidecar() error = %v", err)
	}
	if !sidecar.Pi || sidecar.CommitSHA == "" {
		t.Fatalf("sidecar = %#v, want pi with commit", sidecar)
	}
}

func TestPiResourceOnlyInstallDoesNotRequireShim(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"name":"resources","version":"1.0.0","pi":{"prompts":["custom"],"themes":["looks"],"skills":[]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pkg, err := ParsePiPackage(root)
	if err != nil {
		t.Fatal(err)
	}
	data, err := GenerateManifest(pkg, "", "")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.HasPlugin() || manifest.Run.Command != "" {
		t.Fatal("resource-only manifest launches a plugin")
	}
	if !reflect.DeepEqual(manifest.AllPrompts(), []ArtifactPath{{Path: "custom"}}) || !reflect.DeepEqual(manifest.AllThemes(), []ArtifactPath{{Path: "looks"}}) {
		t.Fatalf("lost resources: %s", data)
	}
	m := NewInstallManager(t.TempDir(), t.TempDir())
	m.ensureShim = func(context.Context, string) error { t.Fatal("resource-only install prepared shim"); return nil }
	m.lookPath = func(string) (string, error) { t.Fatal("resource-only install looked up node"); return "", nil }
	if err := m.Install(t.Context(), Config{Source: root, Pi: true, Yes: true}); err != nil {
		t.Fatal(err)
	}
}

func TestParsePiPackageResolvesExtensionGlobs(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"keep.js", "drop.js"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("source"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte("\xef\xbb\xbf"+`{"name":"example","version":"1.0.0","pi":{"extensions":["*.js","!drop.js"],"prompts":[],"themes":["themes"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pkg, err := ParsePiPackage(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pkg.Extensions, []string{"keep.js"}) || pkg.Prompts == nil || !reflect.DeepEqual(pkg.Themes, []string{"themes"}) {
		t.Fatalf("lost manifest policy: %#v", pkg)
	}
	data, err := GenerateManifest(pkg, "/managed/pi-ext-shim", "/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "drop.js") || strings.Contains(string(data), "*.js") {
		t.Fatalf("generated command contains unresolved sources: %s", data)
	}
}

func TestGenerateManifestQuotesLaunchArguments(t *testing.T) {
	pkg := &PiPackage{Name: "quoted", Version: "1", Extensions: []string{
		"plain.js", "space name.js", "apostrophe's.js", `double"quote.js`, "$HOME.js", "$(touch injected).js", "semi;colon.js", "line\nbreak.js", "日本語.js", "back\\slash.js",
	}}
	data, err := GenerateManifest(pkg, "/shim's $dir/pi-ext-shim", "/node bin/node")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	want := `'/node bin/node' '/shim'"'"'s $dir/pi-ext-shim/index.js' plain.js 'space name.js' 'apostrophe'"'"'s.js' 'double"quote.js' '$HOME.js' '$(touch injected).js' 'semi;colon.js' 'line` + "\n" + `break.js' '日本語.js' 'back\slash.js'`
	if manifest.Run.Command != want {
		t.Fatalf("command = %q, want %q", manifest.Run.Command, want)
	}
}

func TestParsePiPackageRejectsEscapingName(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"name":"../outside","version":"1","pi":{"prompts":[]}}`), 0o600))
	_, err := ParsePiPackage(root)
	require.Error(t, err)
}

func TestInstallPiOfflineDependenciesRetainExistingDestination(t *testing.T) {
	t.Setenv("PI_OFFLINE", "1")
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "package.json"), []byte(`{"name":"pi-pkg","version":"1","dependencies":{"dependency":"1"},"pi":{"prompts":[]}}`), 0o600))
	base := t.TempDir()
	destination := filepath.Join(base, "pi-pkg")
	require.NoError(t, os.MkdirAll(destination, 0o700))
	sentinel := filepath.Join(destination, "sentinel")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o600))
	manager := NewInstallManager(base, t.TempDir())
	require.ErrorContains(t, manager.Install(t.Context(), Config{Source: source, Yes: true, Pi: true, Force: true}), "needs network")
	require.FileExists(t, sentinel)
}

func TestFailedPiReplacementRetainsPreviousPackage(t *testing.T) {
	t.Setenv("PI_OFFLINE", "")
	for _, phase := range []string{"dependencies", "shim", "node"} {
		t.Run(phase, func(t *testing.T) {
			source := t.TempDir()
			dependencies := ""
			if phase == "dependencies" {
				dependencies = `,"dependencies":{"dependency":"1"}`
			}
			require.NoError(t, os.WriteFile(filepath.Join(source, "package.json"), []byte(`{"name":"pi-pkg","version":"1","pi":{"extensions":["./ext.js"]}`+dependencies+`}`), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(source, "ext.js"), []byte("export default function() {}"), 0o600))
			base := t.TempDir()
			destination := filepath.Join(base, "pi-pkg")
			require.NoError(t, os.MkdirAll(destination, 0o700))
			sentinel := filepath.Join(destination, "old")
			require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o600))
			cause := errors.New("injected setup failure")
			manager := NewInstallManager(base, t.TempDir())
			manager.npmInstall = func(context.Context, string, []string) error { return cause }
			manager.ensureShim = func(context.Context, string) error {
				if phase == "shim" {
					return cause
				}
				return nil
			}
			manager.lookPath = func(string) (string, error) { return "", cause }
			require.ErrorIs(t, manager.Install(t.Context(), Config{Source: source, Pi: true, Force: true, Yes: true}), cause)
			require.FileExists(t, sentinel)
			entries, err := packageEntriesWithoutLocks(base)
			require.NoError(t, err)
			require.Len(t, entries, 1)
		})
	}
}

func TestExtractNpmPackageRejectsEscapingPathsAndRedirects(t *testing.T) {
	for _, entry := range []string{"package/../../escape", "../escape", "/absolute", "package/redirect/escape"} {
		t.Run(entry, func(t *testing.T) {
			var archive bytes.Buffer
			gz := gzip.NewWriter(&archive)
			tw := tar.NewWriter(gz)
			require.NoError(t, tw.WriteHeader(&tar.Header{Name: entry, Mode: 0o600, Size: 1, Typeflag: tar.TypeReg}))
			_, err := tw.Write([]byte("x"))
			require.NoError(t, err)
			require.NoError(t, tw.Close())
			require.NoError(t, gz.Close())
			base := t.TempDir()
			tarball := filepath.Join(base, "package.tgz")
			require.NoError(t, os.WriteFile(tarball, archive.Bytes(), 0o600))
			destination := filepath.Join(base, "destination")
			require.NoError(t, os.MkdirAll(destination, 0o700))
			outside := t.TempDir()
			require.NoError(t, os.Symlink(outside, filepath.Join(destination, "redirect")))
			require.Error(t, extractNpmPackage(tarball, destination))
			require.NoFileExists(t, filepath.Join(base, "escape"))
			require.NoFileExists(t, filepath.Join(outside, "escape"))
		})
	}
}

func TestPiNPMRejectsArchiveWithDifferentIdentity(t *testing.T) {
	t.Setenv("PI_OFFLINE", "")
	base := filepath.Join(t.TempDir(), "packages")
	manager := NewInstallManager(base, t.TempDir())
	manager.npmPack = func(ctx context.Context, spec, dst string) (string, error) {
		return writePiTarball(t, dst, `{"name":"different","version":"1","pi":{"prompts":["prompt.md"]}}`), nil
	}
	require.ErrorContains(t, manager.Install(t.Context(), Config{Source: "npm:requested", Pi: true, Yes: true}), "identity")
	require.NoDirExists(t, base)
}

func TestNpmExtractionRejectsCancelledAdmission(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("npm extraction cancelled")
	cancel(cause)
	destination := filepath.Join(t.TempDir(), "package")
	err := extractNpmPackageContext(ctx, "missing.tar.gz", destination)
	require.ErrorIs(t, err, cause)
	require.NoDirExists(t, destination)
}
