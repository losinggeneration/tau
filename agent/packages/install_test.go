package packages

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
)

func TestParseNpmSpec(t *testing.T) {
	tests := []struct {
		name     string
		spec     string
		expected NpmSpec
	}{
		{
			name: "simple package name",
			spec: "lodash",
			expected: NpmSpec{
				Name:    "lodash",
				Version: "",
				Pinned:  false,
			},
		},
		{
			name: "package with version",
			spec: "lodash@4.0.0",
			expected: NpmSpec{
				Name:    "lodash",
				Version: "4.0.0",
				Pinned:  true,
			},
		},
		{
			name: "package with latest",
			spec: "lodash@latest",
			expected: NpmSpec{
				Name:    "lodash",
				Version: "latest",
				Pinned:  false,
			},
		},
		{
			name: "scoped package",
			spec: "@types/node",
			expected: NpmSpec{
				Name:    "@types/node",
				Version: "",
				Pinned:  false,
			},
		},
		{
			name: "scoped package with version",
			spec: "@types/node@18.0.0",
			expected: NpmSpec{
				Name:    "@types/node",
				Version: "18.0.0",
				Pinned:  true,
			},
		},
		{
			name: "simple package with whitespace",
			spec: "  lodash  ",
			expected: NpmSpec{
				Name:    "lodash",
				Version: "",
				Pinned:  false,
			},
		},
		{
			name: "monorepo package",
			spec: "@babel/core@7.22.0",
			expected: NpmSpec{
				Name:    "@babel/core",
				Version: "7.22.0",
				Pinned:  true,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := parseNpmSpec(tc.spec)
			if result.Name != tc.expected.Name {
				t.Errorf("Name: got %q, want %q", result.Name, tc.expected.Name)
			}
			if result.Version != tc.expected.Version {
				t.Errorf("Version: got %q, want %q", result.Version, tc.expected.Version)
			}
			if result.Pinned != tc.expected.Pinned {
				t.Errorf("Pinned: got %v, want %v", result.Pinned, tc.expected.Pinned)
			}
		})
	}
}

func TestNPMCommandUsesConfiguredArgvPrefix(t *testing.T) {
	if got := npmCommand(t.Context(), nil, "install", "x").Args; !slices.Equal(got, []string{"npm", "install", "x"}) {
		t.Fatalf("default args = %q", got)
	}
	configured := make([]string, 5, 8)
	copy(configured, []string{"mise", "exec", "node@20", "--", "npm"})
	first := npmCommand(t.Context(), configured, "pack", "a")
	second := npmCommand(t.Context(), configured, "uninstall", "b", "-g")
	if want := []string{"mise", "exec", "node@20", "--", "npm", "pack", "a"}; !slices.Equal(first.Args, want) {
		t.Fatalf("first args = %q, want %q", first.Args, want)
	}
	if want := []string{"mise", "exec", "node@20", "--", "npm", "uninstall", "b", "-g"}; !slices.Equal(second.Args, want) {
		t.Fatalf("second args = %q, want %q", second.Args, want)
	}
}

func TestInstallRejectsCancelledLocalReplacement(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "packages", "test")
	require.NoError(t, os.MkdirAll(source, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "tau-package.toml"), []byte("[package]\nname = \"test\"\nversion = \"1.0.0\"\n[plugin]\napi_version = \"1\"\n"), 0o600))
	require.NoError(t, os.MkdirAll(destination, 0o700))
	sentinel := filepath.Join(destination, "old")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o600))
	manager := NewInstallManager(filepath.Dir(destination), t.TempDir())
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("install withdrawn")
	cancel(cause)
	err := manager.Install(ctx, Config{Source: source, Yes: true, Force: true})
	require.ErrorIs(t, err, cause)
	require.FileExists(t, sentinel)
}

func TestPromptTrustRejectsCancelledAdmission(t *testing.T) {
	manager := NewInstallManager(t.TempDir(), t.TempDir())
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("trust withdrawn")
	cancel(cause)
	for _, cfg := range []Config{{Yes: true}, {}} {
		accepted, err := manager.PromptTrust(ctx, cfg, &Manifest{}, strings.NewReader("y\n"))
		require.ErrorIs(t, err, cause)
		require.False(t, accepted)
	}
}

type cancellingTrustReader struct {
	cancel context.CancelCauseFunc
	cause  error
}

func (r cancellingTrustReader) Read(p []byte) (int, error) {
	r.cancel(r.cause)
	return copy(p, "y\n"), io.EOF
}

func TestPromptTrustRejectsCancellationDuringResponse(t *testing.T) {
	manager := NewInstallManager(t.TempDir(), t.TempDir())
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	cause := errors.New("trust response withdrawn")
	accepted, err := manager.PromptTrust(ctx, Config{}, &Manifest{}, cancellingTrustReader{cancel: cancel, cause: cause})
	require.ErrorIs(t, err, cause)
	require.False(t, accepted)
}

type failedTrustReader struct{ err error }

func (r failedTrustReader) Read([]byte) (int, error) { return 0, r.err }

func TestPromptTrustRetainsReadFailure(t *testing.T) {
	cause := errors.New("trust input failed")
	manager := NewInstallManager(t.TempDir(), t.TempDir())
	accepted, err := manager.PromptTrust(t.Context(), Config{}, &Manifest{}, failedTrustReader{err: cause})
	require.False(t, accepted)
	require.ErrorIs(t, err, cause)
}

func TestInstallOfflineRejectsGitBeforeFilesystemWork(t *testing.T) {
	t.Setenv("PI_OFFLINE", "true")
	root := t.TempDir()
	installBase := filepath.Join(root, "packages")
	manager := NewInstallManager(installBase, t.TempDir())
	err := manager.Install(t.Context(), Config{Source: "file://" + filepath.Join(root, "missing-repo"), Yes: true, Force: true})
	require.ErrorContains(t, err, "offline")
	require.NoDirExists(t, installBase)
}

func TestInstallOfflineRejectsNativeNPMBeforeCreatingDestination(t *testing.T) {
	t.Setenv("PI_OFFLINE", "true")
	root := t.TempDir()
	base := filepath.Join(root, "packages")
	manager := NewInstallManager(base, t.TempDir())
	require.ErrorContains(t, manager.Install(t.Context(), Config{Source: "npm:test", Yes: true, Force: true}), "offline")
	require.NoDirExists(t, base)
}

func TestUninstallFromNPMRejectsCancelledProjectRemoval(t *testing.T) {
	base := t.TempDir()
	destination := filepath.Join(base, "retained")
	require.NoError(t, os.MkdirAll(destination, 0o700))
	sentinel := filepath.Join(destination, "sentinel")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o600))
	manager := NewInstallManager(t.TempDir(), base)
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("npm removal withdrawn")
	cancel(cause)
	require.ErrorIs(t, manager.UninstallFromNPM(ctx, "retained", ScopeProject), cause)
	require.FileExists(t, sentinel)
}

func TestPackageEntryPointsRejectUnsafeNames(t *testing.T) {
	manager := NewInstallManager(t.TempDir(), t.TempDir())
	for _, name := range []string{"../outside", "@scope/..", "-option"} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, manager.Install(t.Context(), Config{Source: "npm:" + name, Yes: true}), "invalid package name")
			require.ErrorContains(t, manager.UninstallFromNPM(t.Context(), name, ScopeProject), "invalid package name")
		})
	}
}

func TestInstallRejectsEscapingManifestBeforeRemovingDestination(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	base := filepath.Join(root, "packages")
	outside := filepath.Join(root, "outside")
	require.NoError(t, os.MkdirAll(source, 0o700))
	require.NoError(t, os.MkdirAll(outside, 0o700))
	sentinel := filepath.Join(outside, "sentinel")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "tau-package.toml"), []byte("[package]\nname = \"../outside\"\nversion = \"1\"\n[plugin]\napi_version = \"1\"\n"), 0o600))
	manager := NewInstallManager(base, t.TempDir())
	require.ErrorContains(t, manager.Install(t.Context(), Config{Source: source, Yes: true, Force: true}), "invalid package name")
	require.FileExists(t, sentinel)
	require.NoDirExists(t, base)
}

func TestInstallScopedNativePackageCreatesScopeParent(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	base := filepath.Join(root, "packages")
	require.NoError(t, os.MkdirAll(source, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "tau-package.toml"), []byte("[package]\nname = \"@scope/plugin\"\nversion = \"1\"\n[plugin]\napi_version = \"1\"\n"), 0o600))
	manager := NewInstallManager(base, t.TempDir())
	require.NoError(t, manager.Install(t.Context(), Config{Source: source, Yes: true, Link: true}))
	target, err := os.Readlink(filepath.Join(base, "@scope", "plugin", "tau-package.toml"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(source, "tau-package.toml"), target)
}

func TestInstallLocalLinkResolvesRelativeSource(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	require.NoError(t, os.MkdirAll("source", 0o700))
	require.NoError(t, os.WriteFile(filepath.Join("source", "tau-package.toml"), []byte("[package]\nname = \"plugin\"\nversion = \"1\"\n[plugin]\napi_version = \"1\"\n"), 0o600))
	base := filepath.Join(root, "packages")
	manager := NewInstallManager(base, t.TempDir())
	require.NoError(t, manager.Install(t.Context(), Config{Source: "./source", Yes: true, Link: true}))
	target, err := filepath.EvalSymlinks(filepath.Join(base, "plugin", "tau-package.toml"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "source", "tau-package.toml"), target)
}

func TestCopyFileRetainsExecutablePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable permission")
	}
	root := t.TempDir()
	source := filepath.Join(root, "plugin")
	destination := filepath.Join(root, "installed")
	require.NoError(t, os.WriteFile(source, []byte("plugin bytes"), 0o750))
	require.NoError(t, copyFile(source, destination))
	info, err := os.Stat(destination)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o750), info.Mode().Perm())
}

func TestInstallLocalNativeCopyKeepsExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable permission")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source")
	base := filepath.Join(root, "packages")
	require.NoError(t, os.MkdirAll(source, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "tau-package.toml"), []byte("[package]\nname = \"plugin\"\nversion = \"1\"\n[plugin]\napi_version = \"1\"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "plugin"), []byte("plugin bytes"), 0o750))
	manager := NewInstallManager(base, t.TempDir())
	require.NoError(t, manager.Install(t.Context(), Config{Source: source, Yes: true}))
	info, err := os.Stat(filepath.Join(base, "plugin", "plugin"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o750), info.Mode().Perm())
}

func TestFailedForcedLocalInstallRetainsPreviousPackage(t *testing.T) {
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "tau-package.toml"), []byte("[package]\nname = \"plugin\"\nversion = \"1\"\n[plugin]\napi_version = \"1\"\n"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(source, "missing"), filepath.Join(source, "broken")))
	base := t.TempDir()
	destination := filepath.Join(base, "plugin")
	require.NoError(t, os.MkdirAll(destination, 0o700))
	sentinel := filepath.Join(destination, "old")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o600))
	manager := NewInstallManager(base, t.TempDir())
	err := manager.Install(t.Context(), Config{Source: source, Force: true, Yes: true})
	require.Error(t, err)
	require.FileExists(t, sentinel)
	entries, err := packageEntriesWithoutLocks(base)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "plugin", entries[0].Name())
}

func TestStageLocalPackageRetainsPreviousOnAdmissionFailure(t *testing.T) {
	for _, failure := range []string{"cancel", "rename"} {
		t.Run(failure, func(t *testing.T) {
			base := t.TempDir()
			destination := filepath.Join(base, "plugin")
			require.NoError(t, os.MkdirAll(destination, 0o700))
			sentinel := filepath.Join(destination, "old")
			require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o600))
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			cause := errors.New("staged install withdrawn")
			err := stageLocalPackage(ctx, destination, func(path string) error {
				if failure == "cancel" {
					require.NoError(t, os.MkdirAll(path, 0o700))
					cancel(cause)
				}
				return nil
			})
			require.Error(t, err)
			if failure == "cancel" {
				require.ErrorIs(t, err, cause)
			}
			require.FileExists(t, sentinel)
			entries, err := packageEntriesWithoutLocks(base)
			require.NoError(t, err)
			require.Len(t, entries, 1)
		})
	}
}

func TestForcedLocalInstallPublishesCompleteReplacement(t *testing.T) {
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "tau-package.toml"), []byte("[package]\nname = \"plugin\"\nversion = \"1\"\n[plugin]\napi_version = \"1\"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "new"), []byte("new"), 0o600))
	base := t.TempDir()
	destination := filepath.Join(base, "plugin")
	require.NoError(t, os.MkdirAll(destination, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(destination, "old"), []byte("old"), 0o600))
	manager := NewInstallManager(base, t.TempDir())
	calls := 0
	manager.ArtifactReloader = func(context.Context) error {
		calls++
		require.NoFileExists(t, filepath.Join(destination, "old"))
		require.FileExists(t, filepath.Join(destination, "new"))
		_, err := LoadSidecar(destination)
		require.NoError(t, err)
		return nil
	}
	require.NoError(t, manager.Install(t.Context(), Config{Source: source, Yes: true, Force: true}))
	require.Equal(t, 1, calls)
	entries, err := packageEntriesWithoutLocks(base)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestFailedGitReleaseReplacementRetainsPreviousPackage(t *testing.T) {
	t.Setenv("PI_OFFLINE", "")
	repoDir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	manifest := fmt.Sprintf("[package]\nname = \"plugin\"\nversion = \"1\"\n[plugin]\napi_version = \"1\"\n[[release]]\nplatform = %q\nurl = %q\nsha256 = \"invalid\"\n", currentPlatform(), server.URL)
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "tau-package.toml"), []byte(manifest), 0o600))
	repo, err := git.PlainInit(repoDir, false)
	require.NoError(t, err)
	worktree, err := repo.Worktree()
	require.NoError(t, err)
	_, err = worktree.Add("tau-package.toml")
	require.NoError(t, err)
	_, err = worktree.Commit("package", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()}})
	require.NoError(t, err)
	base := t.TempDir()
	destination := filepath.Join(base, "plugin")
	require.NoError(t, os.MkdirAll(destination, 0o700))
	sentinel := filepath.Join(destination, "old")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o600))
	manager := NewInstallManager(base, t.TempDir())
	require.Error(t, manager.Install(t.Context(), Config{Source: "file://" + repoDir, Yes: true, Force: true}))
	require.FileExists(t, sentinel)
	entries, err := packageEntriesWithoutLocks(base)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestNativeNPMInstallUsesStagedArchive(t *testing.T) {
	t.Setenv("PI_OFFLINE", "")
	base := t.TempDir()
	manager := NewInstallManager(base, t.TempDir())
	calls := 0
	manager.npmPack = func(ctx context.Context, spec, dst string) (string, error) {
		calls++
		require.Equal(t, "@scope/plugin@1.2.3", spec)
		return writePiTarball(t, dst, `{"name":"@scope/plugin","version":"1.2.3"}`), nil
	}
	require.NoError(t, manager.Install(t.Context(), Config{Source: "npm:@scope/plugin@1.2.3", Yes: true}))
	require.Equal(t, 1, calls)
	destination := filepath.Join(base, "@scope", "plugin")
	require.FileExists(t, filepath.Join(destination, "package.json"))
	sidecar, err := LoadSidecar(destination)
	require.NoError(t, err)
	require.Equal(t, "1.2.3", sidecar.Ref)
	require.True(t, sidecar.Pinned)
	require.NotEmpty(t, sidecar.InstalledAt)
}

func TestNativeNPMFailureRetainsPreviousPackage(t *testing.T) {
	t.Setenv("PI_OFFLINE", "")
	for _, phase := range []string{"pack", "identity", "dependencies"} {
		t.Run(phase, func(t *testing.T) {
			base := t.TempDir()
			destination := filepath.Join(base, "plugin")
			require.NoError(t, os.MkdirAll(destination, 0o700))
			sentinel := filepath.Join(destination, "old")
			require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o600))
			manager := NewInstallManager(base, t.TempDir())
			cause := errors.New("npm setup rejected")
			manager.npmPack = func(ctx context.Context, spec, dst string) (string, error) {
				if phase == "pack" {
					return "", cause
				}
				metadata := `{"name":"different","version":"1.0.0"}`
				if phase == "dependencies" {
					metadata = `{"name":"plugin","version":"1.0.0","dependencies":{"dependency":"1"}}`
				}
				return writePiTarball(t, dst, metadata), nil
			}
			manager.npmInstall = func(context.Context, string, []string) error { return cause }
			err := manager.Install(t.Context(), Config{Source: "npm:plugin", Yes: true, Force: true})
			require.Error(t, err)
			if phase != "identity" {
				require.ErrorIs(t, err, cause)
			}
			require.FileExists(t, sentinel)
			entries, err := packageEntriesWithoutLocks(base)
			require.NoError(t, err)
			require.Len(t, entries, 1)
		})
	}
}

func TestNativeNPMArchiveRemovalDoesNotInvokeGlobalNPM(t *testing.T) {
	base := t.TempDir()
	destination := filepath.Join(base, "plugin")
	require.NoError(t, os.MkdirAll(destination, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(destination, "package.json"), []byte(`{"name":"plugin","version":"1"}`), 0o600))
	require.NoError(t, (InstallSidecar{Source: "npm:plugin", Scope: string(ScopeUser)}).Write(destination))
	manager := NewInstallManager(base, t.TempDir())
	manager.NPMCommand = []string{filepath.Join(t.TempDir(), "must-not-execute")}
	require.NoError(t, manager.UninstallFromNPM(t.Context(), "plugin", ScopeUser))
	require.NoDirExists(t, destination)
}

func TestInstallRejectsScopedSymlinkEscape(t *testing.T) {
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "tau-package.toml"), []byte("[package]\nname = \"@scope/plugin\"\nversion = \"1\"\n[plugin]\napi_version = \"1\"\n"), 0o600))
	base := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(base, "@scope")))
	destination := filepath.Join(outside, "plugin")
	require.NoError(t, os.MkdirAll(destination, 0o700))
	sentinel := filepath.Join(destination, "sentinel")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o600))
	manager := NewInstallManager(base, t.TempDir())
	require.Error(t, manager.Install(t.Context(), Config{Source: source, Force: true, Yes: true}))
	require.FileExists(t, sentinel)
}

func TestGitInstallChecksOutRecordedRevision(t *testing.T) {
	t.Setenv("PI_OFFLINE", "")
	for _, selection := range []string{"tag", "commit", "latest"} {
		t.Run(selection, func(t *testing.T) {
			source := t.TempDir()
			repo, err := git.PlainInit(source, false)
			require.NoError(t, err)
			tree, err := repo.Worktree()
			require.NoError(t, err)
			manifest := []byte("[package]\nname = \"revision\"\nversion = \"1\"\n[plugin]\napi_version = \"1\"\n")
			require.NoError(t, os.WriteFile(filepath.Join(source, "tau-package.toml"), manifest, 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(source, "content"), []byte("selected"), 0o600))
			_, err = tree.Add(".")
			require.NoError(t, err)
			selected, err := tree.Commit("selected", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()}})
			require.NoError(t, err)
			_, err = repo.CreateTag("v1.0.0", selected, nil)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(source, "content"), []byte("branch head"), 0o600))
			_, err = tree.Add(".")
			require.NoError(t, err)
			_, err = tree.Commit("head", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()}})
			require.NoError(t, err)
			url := "file://" + source
			switch selection {
			case "tag":
				url += "@v1.0.0"
			case "commit":
				url += "@" + selected.String()
			}
			base := t.TempDir()
			manager := NewInstallManager(base, t.TempDir())
			require.NoError(t, manager.Install(t.Context(), Config{Source: url, Yes: true}))
			destination := filepath.Join(base, "revision")
			content, err := os.ReadFile(filepath.Join(destination, "content"))
			require.NoError(t, err)
			require.Equal(t, "selected", string(content))
			sidecar, err := LoadSidecar(destination)
			require.NoError(t, err)
			require.Equal(t, selected.String(), sidecar.CommitSHA)
			installed, err := git.PlainOpen(destination)
			require.NoError(t, err)
			head, err := installed.Head()
			require.NoError(t, err)
			require.Equal(t, selected, head.Hash())
		})
	}
}

func TestParseSourceRefPreservesURLAuthoritiesAndScopePaths(t *testing.T) {
	for _, test := range []struct{ source, url, ref string }{
		{"file:///tmp/@scope/plugin", "file:///tmp/@scope/plugin", ""},
		{"file:///tmp/@scope/plugin@feature/fix", "file:///tmp/@scope/plugin", "feature/fix"},
		{"https://git@example.invalid/team/repo", "https://git@example.invalid/team/repo", ""},
		{"https://git@example.invalid/team/repo@main", "https://git@example.invalid/team/repo", "main"},
		{"git@example.invalid:team/repo", "git@example.invalid:team/repo", ""},
		{"git@example.invalid:team/repo@main", "git@example.invalid:team/repo", "main"},
	} {
		t.Run(test.source, func(t *testing.T) {
			url, ref := parseSourceRef(test.source)
			require.Equal(t, test.url, url)
			require.Equal(t, test.ref, ref)
		})
	}
}

func TestCopyDirRejectsSourceDestinationOverlap(t *testing.T) {
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "file"), []byte("original"), 0o600))
	require.Error(t, copyDir(source, source))
	content, err := os.ReadFile(filepath.Join(source, "file"))
	require.NoError(t, err)
	require.Equal(t, "original", string(content))
}

func TestCopyDirRejectsNestedAndRedirectedDestinations(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		t.Run(fmt.Sprint(redirect), func(t *testing.T) {
			source := t.TempDir()
			destination := filepath.Join(source, "nested", "package")
			if redirect {
				link := filepath.Join(t.TempDir(), "source")
				require.NoError(t, os.Symlink(source, link))
				destination = filepath.Join(link, "nested", "package")
			}
			require.ErrorContains(t, copyDir(source, destination), "inside its source")
			require.NoDirExists(t, filepath.Join(source, "nested"))
		})
	}
}

func TestForcedLocalLinkRejectsSourceInsideDestination(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprint(nested), func(t *testing.T) {
			base := t.TempDir()
			destination := filepath.Join(base, "plugin")
			source := destination
			if nested {
				source = filepath.Join(destination, "source")
			}
			require.NoError(t, os.MkdirAll(source, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(source, "tau-package.toml"), []byte("[package]\nname = \"plugin\"\nversion = \"1\"\n[plugin]\napi_version = \"1\"\n"), 0o600))
			sentinel := filepath.Join(source, "keep")
			require.NoError(t, os.WriteFile(sentinel, []byte("original"), 0o600))
			manager := NewInstallManager(base, t.TempDir())
			require.Error(t, manager.Install(t.Context(), Config{Source: source, Link: true, Force: true, Yes: true}))
			require.FileExists(t, sentinel)
			info, err := os.Lstat(destination)
			require.NoError(t, err)
			require.Zero(t, info.Mode()&os.ModeSymlink)
		})
	}
}

func TestLinkedInstallReplacesSidecarWithoutWritingThroughRedirect(t *testing.T) {
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "tau-package.toml"), []byte("[package]\nname = \"plugin\"\nversion = \"1\"\n[plugin]\napi_version = \"1\"\n"), 0o600))
	outside := filepath.Join(t.TempDir(), "keep")
	require.NoError(t, os.WriteFile(outside, []byte("original"), 0o600))
	require.NoError(t, os.Symlink(outside, sidecarPath(source)))
	base := t.TempDir()
	manager := NewInstallManager(base, t.TempDir())
	require.NoError(t, manager.Install(t.Context(), Config{Source: source, Link: true, Yes: true}))
	content, err := os.ReadFile(outside)
	require.NoError(t, err)
	require.Equal(t, "original", string(content))
	sidecar, err := LoadSidecar(filepath.Join(base, "plugin"))
	require.NoError(t, err)
	require.Equal(t, source, sidecar.Source)
}

func TestTauOfflineRejectsRemotePackageAdmission(t *testing.T) {
	t.Setenv("PI_OFFLINE", "")
	t.Setenv("TAU_OFFLINE", "true")
	require.True(t, IsOfflineMode())
	base := filepath.Join(t.TempDir(), "packages")
	manager := NewInstallManager(base, t.TempDir())
	for _, source := range []string{"npm:plugin", "file:///missing/repository"} {
		require.ErrorContains(t, manager.Install(t.Context(), Config{Source: source, Yes: true}), "offline")
	}
	require.NoDirExists(t, base)
}

func TestLocalCopyInstallFollowsLinkedSourceDirectory(t *testing.T) {
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "tau-package.toml"), []byte("[package]\nname = \"plugin\"\nversion = \"1\"\n[plugin]\napi_version = \"1\"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "content"), []byte("copied"), 0o600))
	link := filepath.Join(t.TempDir(), "source")
	require.NoError(t, os.Symlink(source, link))
	base := t.TempDir()
	manager := NewInstallManager(base, t.TempDir())
	require.NoError(t, manager.Install(t.Context(), Config{Source: link, Yes: true}))
	content, err := os.ReadFile(filepath.Join(base, "plugin", "content"))
	require.NoError(t, err)
	require.Equal(t, "copied", string(content))
	info, err := os.Lstat(filepath.Join(base, "plugin"))
	require.NoError(t, err)
	require.True(t, info.IsDir())
	require.Zero(t, info.Mode()&os.ModeSymlink)
}

func TestPackageCopiesRejectCancelledAdmissionBeforeCreatingDestination(t *testing.T) {
	source := t.TempDir()
	file := filepath.Join(source, "content")
	require.NoError(t, os.WriteFile(file, []byte("original"), 0o600))
	cause := errors.New("cancel package copy")
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(cause)
	destination := filepath.Join(t.TempDir(), "copy")
	require.ErrorIs(t, copyDirContext(ctx, source, destination), cause)
	require.NoDirExists(t, destination)
	require.ErrorIs(t, copyFileContext(ctx, file, destination), cause)
	require.NoFileExists(t, destination)
}

func TestCopyFileRejectsDirectoryBeforeCreatingDestination(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "copy")
	require.ErrorContains(t, copyFile(t.TempDir(), destination), "regular file")
	require.NoFileExists(t, destination)
}

func TestNativeNPMRejectsConflictingNativeManifest(t *testing.T) {
	t.Setenv("TAU_OFFLINE", "")
	t.Setenv("PI_OFFLINE", "")
	manager := NewInstallManager(t.TempDir(), t.TempDir())
	destination := filepath.Join(manager.userDir, "plugin")
	require.NoError(t, os.MkdirAll(destination, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(destination, "keep"), []byte("original"), 0o600))
	manager.npmPack = func(ctx context.Context, spec, dir string) (string, error) {
		archive := filepath.Join(dir, "package.tgz")
		file, err := os.Create(archive)
		require.NoError(t, err)
		compressed := gzip.NewWriter(file)
		writer := tar.NewWriter(compressed)
		for name, body := range map[string]string{
			"package/package.json":     `{"name":"plugin","version":"2.0.0"}`,
			"package/tau-package.toml": "[package]\nname = \"different\"\nversion = \"2.0.0\"\n[plugin]\napi_version = \"1\"\n",
		} {
			require.NoError(t, writer.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}))
			_, err := writer.Write([]byte(body))
			require.NoError(t, err)
		}
		require.NoError(t, writer.Close())
		require.NoError(t, compressed.Close())
		require.NoError(t, file.Close())
		return archive, nil
	}
	err := manager.Install(t.Context(), Config{Source: "npm:plugin", Yes: true, Force: true})
	require.ErrorContains(t, err, "identity")
	require.FileExists(t, filepath.Join(destination, "keep"))
}

func TestNpmPinsOnlyExactVersions(t *testing.T) {
	for _, version := range []string{"1.2.3", "v1.2.3", "1.2.3-beta.1", "1.2.3+build.01", "1.2.3-beta.1+build"} {
		for _, name := range []string{"plugin", "@scope/plugin"} {
			spec := parseNpmSpec(name + "@" + version)
			require.True(t, spec.Pinned, version)
			require.Equal(t, version, spec.Version)
		}
	}
	for _, version := range []string{"", "=1.2.3", "latest", "next", "^1.2.3", "~1.2.3", ">=1.0.0", "1", "1.2", "1.2.x", "01.2.3", "1.02.3", "1.2.03", "1.2.3-01", "9007199254740992.0.0", "1.2.3-", "1.2.3+", "1.2.3-a..b", "1.2.3 || 2.0.0"} {
		for _, name := range []string{"plugin", "@scope/plugin"} {
			require.False(t, parseNpmSpec(name+"@"+version).Pinned, version)
		}
	}
}

func TestNPMExactVersionRejectsDifferentArchiveBeforeSetup(t *testing.T) {
	t.Setenv("TAU_OFFLINE", "")
	t.Setenv("PI_OFFLINE", "")
	for _, pi := range []bool{false, true} {
		t.Run(fmt.Sprint(pi), func(t *testing.T) {
			base := t.TempDir()
			destination := filepath.Join(base, "plugin")
			require.NoError(t, os.Mkdir(destination, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(destination, "keep"), []byte("old"), 0o600))
			manager := NewInstallManager(base, t.TempDir())
			manager.npmPack = func(ctx context.Context, spec, dir string) (string, error) {
				return writePiTarball(t, dir, `{"name":"plugin","version":"2.0.0","dependencies":{"dep":"*"},"pi":{"prompts":["prompt.md"]}}`), nil
			}
			manager.npmInstall = func(context.Context, string, []string) error {
				t.Error("set up rejected archive")
				return nil
			}
			err := manager.Install(t.Context(), Config{Source: "npm:plugin@1.0.0", Force: true, Yes: true, Pi: pi})
			require.ErrorContains(t, err, "version")
			require.FileExists(t, filepath.Join(destination, "keep"))
		})
	}
}

func TestValidateNPMExactArchiveVersion(t *testing.T) {
	for _, tc := range []struct {
		requested, actual string
		valid             bool
	}{
		{"1.2.3", "1.2.3", true},
		{"v1.2.3", "1.2.3", true},
		{"1.2.3+one", "1.2.3+two", true},
		{"1.2.3-beta.1", "1.2.3-beta.1+build", true},
		{"1.2.3", "1.2.4", false},
		{"1.2.3", "1.2.3-beta.1", false},
		{"1.2.3-beta.1", "1.2.3-beta.2", false},
		{"latest", "1.2.3", true},
		{"latest", "", false},
		{"latest", "1.2", false},
		{"latest", "1.2.3-01", false},
	} {
		t.Run(tc.requested+"/"+tc.actual, func(t *testing.T) {
			err := validateNPMVersion(parseNpmSpec("plugin@"+tc.requested), tc.actual)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestStagePackageWaitCancellationDoesNotPrepare(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "plugin")
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		finished <- stageLocalPackage(t.Context(), destination, func(staged string) error {
			close(entered)
			<-release
			return os.MkdirAll(staged, 0o700)
		})
	}()
	<-entered
	ctx, cancel := context.WithTimeoutCause(t.Context(), 50*time.Millisecond, errors.New("package writer wait cancelled"))
	defer cancel()
	prepared := false
	err := stageLocalPackage(ctx, destination, func(staged string) error {
		prepared = true
		return os.MkdirAll(staged, 0o700)
	})
	close(release)
	require.NoError(t, <-finished)
	require.ErrorIs(t, err, context.Cause(ctx))
	require.False(t, prepared, "waiting writer prepared before acquiring ownership")
}

func TestPackageWriterCanonicalParentAndWaiterCleanup(t *testing.T) {
	base := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(base, alias))
	destination := filepath.Join(base, "plugin")
	unlock, err := lockPackageWriter(t.Context(), destination)
	require.NoError(t, err)
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("cancel queued package writer")
	cancel(cause)
	_, err = lockPackageWriter(ctx, filepath.Join(alias, "plugin"))
	require.ErrorIs(t, err, cause)
	waitCtx, waitCancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer waitCancel()
	_, err = lockPackageWriter(waitCtx, filepath.Join(alias, "plugin"))
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, unlock())
	unlockAgain, err := lockPackageWriter(t.Context(), filepath.Join(alias, "plugin"))
	require.NoError(t, err)
	require.NoError(t, unlockAgain())
}

func TestConcurrentNPMInstallDoesNotPrepareWaitingReplacement(t *testing.T) {
	t.Setenv("PI_OFFLINE", "")
	t.Setenv("TAU_OFFLINE", "")
	base := t.TempDir()
	first, second := NewInstallManager(base, t.TempDir()), NewInstallManager(base, t.TempDir())
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	first.npmPack = func(ctx context.Context, spec, dst string) (string, error) {
		close(entered)
		<-release
		return writePiTarball(t, dst, `{"name":"plugin","version":"1.0.0"}`), nil
	}
	second.npmPack = func(context.Context, string, string) (string, error) {
		t.Error("waiting or duplicate install must not pack")
		return "", errors.New("unexpected pack")
	}
	go func() { finished <- first.Install(t.Context(), Config{Source: "npm:plugin", Yes: true}) }()
	<-entered
	ctx, cancel := context.WithTimeoutCause(t.Context(), 30*time.Millisecond, errors.New("second install cancelled"))
	defer cancel()
	err := second.Install(ctx, Config{Source: "npm:plugin", Yes: true})
	close(release)
	require.NoError(t, <-finished)
	require.ErrorIs(t, err, context.Cause(ctx))
	var duplicate ErrAlreadyInstalled
	require.ErrorAs(t, second.Install(t.Context(), Config{Source: "npm:plugin", Yes: true}), &duplicate)
	sidecar, err := LoadSidecar(filepath.Join(base, "plugin"))
	require.NoError(t, err)
	require.Equal(t, "1.0.0", sidecar.Ref)
}

func TestStagePackageRejectsConcurrentNonForcedLocalInstall(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	require.NoError(t, os.MkdirAll(source, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "tau-package.toml"), []byte("[package]\nname = 'plugin'\nversion = '1.0.0'\n[[prompts]]\npath = 'prompt.md'\n"), 0o600))
	base := t.TempDir()
	destination := filepath.Join(base, "plugin")
	manager := NewInstallManager(base, t.TempDir())
	unlock, err := lockPackageWriter(t.Context(), destination)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	err = manager.Install(ctx, Config{Source: source, Yes: true})
	require.NoError(t, unlock())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoDirExists(t, destination)
}

func TestNPMRemovalWaitsForPackageWriter(t *testing.T) {
	base := t.TempDir()
	destination := filepath.Join(base, "plugin")
	require.NoError(t, os.MkdirAll(destination, 0o700))
	unlock, err := lockPackageWriter(t.Context(), destination)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	err = NewInstallManager(base, t.TempDir()).UninstallFromNPM(ctx, "plugin", ScopeUser)
	require.NoError(t, unlock())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.DirExists(t, destination)
	require.NoError(t, NewInstallManager(base, t.TempDir()).UninstallFromNPM(t.Context(), "plugin", ScopeUser))
	require.NoDirExists(t, destination)
}

func TestStagePackageSerializesCompleteReplacements(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "plugin")
	const writers = 12
	results := make(chan error, writers)
	for i := range writers {
		go func() {
			results <- stageLocalPackage(t.Context(), destination, func(staged string) error {
				if err := os.MkdirAll(staged, 0o700); err != nil {
					return err
				}
				value := fmt.Sprint(i)
				if err := os.WriteFile(filepath.Join(staged, "content"), []byte(value), 0o600); err != nil {
					return err
				}
				return (InstallSidecar{Source: value}).Write(staged)
			})
		}()
	}
	for range writers {
		require.NoError(t, <-results)
	}
	content, err := os.ReadFile(filepath.Join(destination, "content"))
	require.NoError(t, err)
	sidecar, err := LoadSidecar(destination)
	require.NoError(t, err)
	require.Equal(t, string(content), sidecar.Source)
	entries, err := packageEntriesWithoutLocks(filepath.Dir(destination))
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestPackageWriterHonorsIndependentAdvisoryLock(t *testing.T) {
	base := t.TempDir()
	destination := filepath.Join(base, "plugin")
	lock := flock.New(filepath.Join(base, ".tau-install-lock-plugin"))
	require.NoError(t, lock.Lock())
	ctx, cancel := context.WithTimeoutCause(t.Context(), 30*time.Millisecond, errors.New("external package writer cancelled"))
	defer cancel()
	prepared := false
	err := stageLocalPackage(ctx, destination, func(staged string) error {
		prepared = true
		return os.MkdirAll(staged, 0o700)
	})
	require.NoError(t, lock.Close())
	require.ErrorIs(t, err, context.Cause(ctx))
	require.False(t, prepared)
}

func packageEntriesWithoutLocks(base string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(entries, func(entry os.DirEntry) bool {
		return strings.HasPrefix(entry.Name(), ".tau-install-lock-") && entry.Type().IsRegular()
	}), nil
}

func TestPackageWriterRejectsRedirectedLockFile(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.WriteFile(outside, []byte("keep"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(base, ".tau-install-lock-plugin")))
	prepared := false
	err := stageLocalPackage(t.Context(), filepath.Join(base, "plugin"), func(string) error {
		prepared = true
		return nil
	})
	require.ErrorContains(t, err, "regular file")
	require.False(t, prepared)
	content, err := os.ReadFile(outside)
	require.NoError(t, err)
	require.Equal(t, "keep", string(content))
}

func TestPackageWriterLockSurvivesRemovalAndReinstallation(t *testing.T) {
	base := t.TempDir()
	destination := filepath.Join(base, "plugin")
	prepare := func(staged string) error { return os.MkdirAll(staged, 0o700) }
	require.NoError(t, stageLocalPackage(t.Context(), destination, prepare))
	lockPath := filepath.Join(base, ".tau-install-lock-plugin")
	before, err := os.Stat(lockPath)
	require.NoError(t, err)
	require.NoError(t, NewManager(base, t.TempDir()).Uninstall(t.Context(), "plugin", ScopeUser))
	require.NoError(t, stageLocalPackage(t.Context(), destination, prepare))
	after, err := os.Stat(lockPath)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after), "removing a lock file would split active waiters across inodes")
	roots, err := InstalledRoots(base)
	require.NoError(t, err)
	require.Equal(t, []string{destination}, roots)
}

func TestStagePackageSnapshotConflictRetainsCurrentInstallation(t *testing.T) {
	for _, phase := range []string{"entry", "prepare", "removed"} {
		t.Run(phase, func(t *testing.T) {
			base := t.TempDir()
			destination := filepath.Join(base, "plugin")
			require.NoError(t, os.MkdirAll(destination, 0o700))
			previous := InstallSidecar{Source: "npm:plugin", Ref: "1.0.0"}
			current := previous
			current.Ref = "2.0.0"
			require.NoError(t, previous.Write(destination))
			if phase == "entry" {
				require.NoError(t, current.Write(destination))
			}
			prepared := false
			err := stagePackageExpected(t.Context(), destination, &previous, func(staged string) error {
				prepared = true
				if err := os.MkdirAll(staged, 0o700); err != nil {
					return err
				}
				if phase == "removed" {
					return os.RemoveAll(destination)
				}
				return current.Write(destination)
			})
			require.ErrorContains(t, err, "changed during update")
			require.Equal(t, phase != "entry", prepared)
			if phase == "removed" {
				require.NoDirExists(t, destination)
			} else {
				after, err := LoadSidecar(destination)
				require.NoError(t, err)
				require.Equal(t, current, *after)
			}
			entries, err := packageEntriesWithoutLocks(base)
			require.NoError(t, err)
			if phase == "removed" {
				require.Empty(t, entries)
			} else {
				require.Len(t, entries, 1)
			}
		})
	}
}

func TestStagePackageSnapshotAllowsUnchangedReplacement(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "plugin")
	require.NoError(t, os.MkdirAll(destination, 0o700))
	previous := InstallSidecar{Source: "npm:plugin", Ref: "1.0.0"}
	require.NoError(t, previous.Write(destination))
	next := previous
	next.Ref = "1.1.0"
	require.NoError(t, stagePackageExpected(t.Context(), destination, &previous, func(staged string) error {
		if err := os.MkdirAll(staged, 0o700); err != nil {
			return err
		}
		return next.Write(staged)
	}))
	after, err := LoadSidecar(destination)
	require.NoError(t, err)
	require.Equal(t, next, *after)
}

func TestStagePackageRecoversInterruptedReplacementBeforePreparing(t *testing.T) {
	base := t.TempDir()
	destination := filepath.Join(base, "plugin")
	recovery := filepath.Join(base, ".tau-install-recovery-plugin")
	previous := filepath.Join(recovery, "previous")
	require.NoError(t, os.MkdirAll(previous, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(previous, "keep"), []byte("original"), 0o600))
	cause := errors.New("new preparation rejected")
	err := stageLocalPackage(t.Context(), destination, func(string) error {
		_, err := os.Stat(filepath.Join(destination, "keep"))
		if err != nil {
			return err
		}
		return cause
	})
	require.ErrorIs(t, err, cause)
	require.FileExists(t, filepath.Join(destination, "keep"))
	require.NoDirExists(t, recovery)
}

func TestPackageRecoveryKeepsAdmittedDestination(t *testing.T) {
	base := t.TempDir()
	destination := filepath.Join(base, "plugin")
	require.NoError(t, os.MkdirAll(destination, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(destination, "new"), []byte("admitted"), 0o600))
	recovery := packageRecoveryPath(destination)
	require.NoError(t, os.MkdirAll(filepath.Join(recovery, "previous"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(recovery, "previous", "old"), []byte("obsolete"), 0o600))
	unlock, err := lockPackageWriter(t.Context(), destination)
	require.NoError(t, err)
	require.NoError(t, unlock())
	require.FileExists(t, filepath.Join(destination, "new"))
	require.NoFileExists(t, filepath.Join(destination, "old"))
	require.NoDirExists(t, recovery)
}

func TestPackageRecoveryBeforeAndAfterPreviousMove(t *testing.T) {
	for _, phase := range []string{"prepared", "saved", "saved-link"} {
		t.Run(phase, func(t *testing.T) {
			base := t.TempDir()
			destination := filepath.Join(base, "plugin")
			recovery := packageRecoveryPath(destination)
			require.NoError(t, os.MkdirAll(filepath.Join(recovery, "package"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(recovery, "package", "candidate"), []byte("unadmitted"), 0o600))
			switch phase {
			case "prepared":
				require.NoError(t, os.MkdirAll(destination, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(destination, "old"), []byte("keep"), 0o600))
			case "saved":
				require.NoError(t, os.MkdirAll(filepath.Join(recovery, "previous"), 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(recovery, "previous", "old"), []byte("keep"), 0o600))
			case "saved-link":
				target := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(target, "old"), []byte("keep"), 0o600))
				require.NoError(t, os.Symlink(target, filepath.Join(recovery, "previous")))
			}
			unlock, err := lockPackageWriter(t.Context(), destination)
			require.NoError(t, err)
			require.NoError(t, unlock())
			require.FileExists(t, filepath.Join(destination, "old"))
			require.NoFileExists(t, filepath.Join(destination, "candidate"))
			require.NoDirExists(t, recovery)
		})
	}
}

func TestPackageRecoveryRejectsRedirectedDirectory(t *testing.T) {
	base := t.TempDir()
	destination := filepath.Join(base, "plugin")
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "keep"), []byte("outside"), 0o600))
	require.NoError(t, os.Symlink(outside, packageRecoveryPath(destination)))
	_, err := lockPackageWriter(t.Context(), destination)
	require.ErrorContains(t, err, "real directory")
	require.FileExists(t, filepath.Join(outside, "keep"))
	require.NoDirExists(t, destination)
}

func TestNPMArchiveLimitRetainsInstalledPackage(t *testing.T) {
	t.Setenv("PI_OFFLINE", "")
	t.Setenv("TAU_OFFLINE", "")
	for _, pi := range []bool{false, true} {
		t.Run(fmt.Sprintf("pi=%t", pi), func(t *testing.T) {
			base := t.TempDir()
			destination := filepath.Join(base, "plugin")
			require.NoError(t, os.MkdirAll(destination, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(destination, "sentinel"), []byte("old"), 0o600))
			previous := InstallSidecar{Source: "npm:plugin", Ref: "1.0.0"}
			require.NoError(t, previous.Write(destination))
			manager := NewInstallManager(base, t.TempDir())
			manager.npmPack = func(ctx context.Context, spec, dir string) (string, error) {
				var raw bytes.Buffer
				tw := tar.NewWriter(&raw)
				require.NoError(t, tw.WriteHeader(&tar.Header{Name: "package/oversized", Mode: 0o600, Size: maxArchiveExpanded + 1}))
				var compressed bytes.Buffer
				gz := gzip.NewWriter(&compressed)
				_, err := gz.Write(raw.Bytes())
				require.NoError(t, err)
				require.NoError(t, gz.Close())
				archive := filepath.Join(dir, "package.tgz")
				require.NoError(t, os.WriteFile(archive, compressed.Bytes(), 0o600))
				return archive, nil
			}
			err := manager.Install(t.Context(), Config{Source: "npm:plugin", Yes: true, Force: true, Pi: pi})
			require.ErrorContains(t, err, "archive expanded size limit")
			content, err := os.ReadFile(filepath.Join(destination, "sentinel"))
			require.NoError(t, err)
			require.Equal(t, "old", string(content))
			sidecar, err := LoadSidecar(destination)
			require.NoError(t, err)
			require.Equal(t, previous, *sidecar)
			entries, err := os.ReadDir(base)
			require.NoError(t, err)
			for _, entry := range entries {
				require.False(t, strings.HasPrefix(entry.Name(), ".tau-install-stage-"))
			}
		})
	}
}

func TestLinkRejectsAllRemoteSourcesBeforePreparation(t *testing.T) {
	t.Setenv("PI_OFFLINE", "")
	t.Setenv("TAU_OFFLINE", "")
	for _, source := range []string{"npm:plugin", "npm:@scope/plugin@1.0.0", "https://example.com/team/repo", "git@github.com:team/repo", "github.com/team/repo"} {
		for _, pi := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/pi=%t", source, pi), func(t *testing.T) {
				base := filepath.Join(t.TempDir(), "packages")
				manager := NewInstallManager(base, t.TempDir())
				manager.npmPack = func(context.Context, string, string) (string, error) {
					t.Error("remote link attempted packing")
					return "", errors.New("unexpected packing")
				}
				err := manager.Install(t.Context(), Config{Source: source, Link: true, Pi: pi, Yes: true})
				require.ErrorContains(t, err, "--link is only supported for local paths")
				require.NoDirExists(t, base)
			})
		}
	}
}
