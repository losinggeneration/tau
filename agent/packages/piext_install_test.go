package packages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPiLinkedSetupDoesNotMutateSource(t *testing.T) {
	t.Setenv("PI_OFFLINE", "")
	for _, phase := range []string{"dependencies", "shim", "node", "cancel", "success"} {
		t.Run(phase, func(t *testing.T) {
			source := t.TempDir()
			original := `{"name":"linked-pi","version":"1.0.0","dependencies":{"dep":"1"},"pi":{"extensions":["ext.js"],"prompts":["prompts"]}}`
			require.NoError(t, os.WriteFile(filepath.Join(source, "package.json"), []byte(original), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(source, "ext.js"), []byte("original extension"), 0o600))
			require.NoError(t, os.Mkdir(filepath.Join(source, "prompts"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(source, "prompts", "test.md"), []byte("original prompt"), 0o600))
			base := t.TempDir()
			destination := filepath.Join(base, "linked-pi")
			require.NoError(t, os.Mkdir(destination, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(destination, "old"), []byte("keep"), 0o600))
			manager := NewInstallManager(base, t.TempDir())
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			cause := errors.New("setup rejected")
			manager.npmInstall = func(_ context.Context, dir string, _ []string) error {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte(original+"\n"), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte("generated lock"), 0o600))
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "node_modules"), 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "node_modules", "dep"), []byte("installed dependency"), 0o600))
				if phase == "dependencies" {
					return cause
				}
				return nil
			}
			manager.ensureShim = func(context.Context, string) error {
				if phase == "shim" {
					return cause
				}
				return nil
			}
			manager.lookPath = func(string) (string, error) {
				if phase == "node" {
					return "", cause
				}
				if phase == "cancel" {
					cancel(cause)
				}
				return "/node", nil
			}
			err := manager.Install(ctx, Config{Source: source, Pi: true, Link: true, Force: true, Yes: true})
			if phase == "success" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, cause)
				require.FileExists(t, filepath.Join(destination, "old"))
			}
			data, readErr := os.ReadFile(filepath.Join(source, "package.json"))
			require.NoError(t, readErr)
			require.Equal(t, original, string(data))
			require.NoFileExists(t, filepath.Join(source, "package-lock.json"))
			require.NoDirExists(t, filepath.Join(source, "node_modules"))
			require.NoFileExists(t, filepath.Join(source, "tau-package.toml"))
			require.NoFileExists(t, sidecarPath(source))
			if phase != "success" {
				return
			}
			require.NoError(t, os.WriteFile(filepath.Join(source, "ext.js"), []byte("changed extension"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(source, "prompts", "new.md"), []byte("new prompt"), 0o600))
			data, readErr = os.ReadFile(filepath.Join(destination, "ext.js"))
			require.NoError(t, readErr)
			require.Equal(t, "changed extension", string(data))
			require.FileExists(t, filepath.Join(destination, "prompts", "new.md"))
			require.FileExists(t, filepath.Join(destination, "node_modules", "dep"))
			sidecar, readErr := LoadSidecar(destination)
			require.NoError(t, readErr)
			require.True(t, sidecar.Pi)
			require.Equal(t, source, sidecar.Source)
			require.NoError(t, NewManager(base, t.TempDir()).Uninstall(t.Context(), "linked-pi", ScopeUser))
			require.FileExists(t, filepath.Join(source, "ext.js"))
		})
	}
}

func TestPiLinkedResourcesKeepInstallationMetadataPrivate(t *testing.T) {
	for _, project := range []bool{false, true} {
		t.Run(fmt.Sprint(project), func(t *testing.T) {
			source := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(source, "package.json"), []byte(`{"name":"@scope/linked","version":"1.0.0","pi":{"prompts":["prompts"]}}`), 0o600))
			require.NoError(t, os.Mkdir(filepath.Join(source, "prompts"), 0o700))
			outside := filepath.Join(t.TempDir(), "manifest")
			require.NoError(t, os.WriteFile(outside, []byte("source metadata"), 0o600))
			require.NoError(t, os.Symlink(outside, filepath.Join(source, "tau-package.toml")))
			require.NoError(t, os.WriteFile(sidecarPath(source), []byte("source sidecar"), 0o600))
			user, local := t.TempDir(), t.TempDir()
			manager := NewInstallManager(user, local)
			manager.ensureShim = func(context.Context, string) error { t.Fatal("resource package requested shim"); return nil }
			manager.lookPath = func(string) (string, error) { t.Fatal("resource package requested node"); return "", nil }
			manager.npmInstall = func(context.Context, string, []string) error {
				t.Fatal("resource package requested dependencies")
				return nil
			}
			require.NoError(t, manager.Install(t.Context(), Config{Source: source, Link: true, Pi: true, Yes: true, Project: project}))
			base := user
			scope := ScopeUser
			if project {
				base = local
				scope = ScopeProject
			}
			destination := filepath.Join(base, "@scope", "linked")
			installed, err := Load(filepath.Join(destination, "tau-package.toml"))
			require.NoError(t, err)
			require.Equal(t, "@scope/linked", installed.Package.Name)
			sidecar, err := LoadSidecar(destination)
			require.NoError(t, err)
			require.Equal(t, string(scope), sidecar.Scope)
			data, err := os.ReadFile(outside)
			require.NoError(t, err)
			require.Equal(t, "source metadata", string(data))
			data, err = os.ReadFile(sidecarPath(source))
			require.NoError(t, err)
			require.Equal(t, "source sidecar", string(data))
			updated := `{"name":"@scope/linked","version":"1.0.0","pi":{"prompts":[]}}`
			require.NoError(t, os.WriteFile(filepath.Join(source, "package.json"), []byte(updated), 0o600))
			data, err = os.ReadFile(filepath.Join(destination, "package.json"))
			require.NoError(t, err)
			require.Equal(t, updated, string(data))
			require.NoError(t, NewManager(user, local).Uninstall(t.Context(), "@scope/linked", scope))
			require.FileExists(t, outside)
			require.DirExists(t, filepath.Join(source, "prompts"))
		})
	}
}

func TestLinkPiSourceRejectsFailedPreparation(t *testing.T) {
	t.Run("missing source", func(t *testing.T) {
		require.ErrorContains(t, linkPiSource(t.Context(), filepath.Join(t.TempDir(), "missing"), t.TempDir()), "read linked pi source")
	})
	t.Run("cancelled", func(t *testing.T) {
		source := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(source, "entry"), []byte("source"), 0o600))
		ctx, cancel := context.WithCancelCause(t.Context())
		cause := errors.New("stop linking")
		cancel(cause)
		staged := t.TempDir()
		require.ErrorIs(t, linkPiSource(ctx, source, staged), cause)
		require.NoFileExists(t, filepath.Join(staged, "entry"))
	})
	t.Run("invalid destination", func(t *testing.T) {
		source := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(source, "entry"), []byte("source"), 0o600))
		staged := filepath.Join(t.TempDir(), "file")
		require.NoError(t, os.WriteFile(staged, []byte("keep"), 0o600))
		require.Error(t, linkPiSource(t.Context(), source, staged))
		data, err := os.ReadFile(staged)
		require.NoError(t, err)
		require.Equal(t, "keep", string(data))
	})
}
