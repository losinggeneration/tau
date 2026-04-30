package packages

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
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
			name:    "missing extensions",
			json:    `{"name":"plain","version":"1.0.0","pi":{"skills":["skills"]}}`,
			wantErr: "pi.extensions is required",
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
