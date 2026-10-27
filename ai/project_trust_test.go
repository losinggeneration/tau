package ai

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProjectTrustStoreCanonicalInheritance(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	require.NoError(t, os.Mkdir(child, 0o700))
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(child, alias))
	store := ProjectTrustStore{Path: filepath.Join(t.TempDir(), "agent", "trust.json")}
	decision, err := store.Get(t.Context(), child)
	require.NoError(t, err)
	require.Nil(t, decision)
	require.NoError(t, store.Set(t.Context(), root, new(true)))
	decision, err = store.Get(t.Context(), alias)
	require.NoError(t, err)
	require.Equal(t, new(true), decision)
	require.NoError(t, store.Set(t.Context(), alias, new(false)))
	decision, err = store.Get(t.Context(), child)
	require.NoError(t, err)
	require.Equal(t, new(false), decision)
	require.NoError(t, store.Set(t.Context(), child, nil))
	decision, err = store.Get(t.Context(), child)
	require.NoError(t, err)
	require.Equal(t, new(true), decision)
	*decision = false
	decision, err = store.Get(t.Context(), child)
	require.NoError(t, err)
	require.True(t, *decision)
}

func TestProjectTrustStoreRejectsInvalidFile(t *testing.T) {
	for _, body := range []string{"null", "[]", "true", "", `{"/project":"secret decision"}`, `{"/project":1}`, `{} {}`} {
		t.Run(body, func(t *testing.T) {
			store := ProjectTrustStore{Path: filepath.Join(t.TempDir(), "trust.json")}
			require.NoError(t, os.WriteFile(store.Path, []byte(body), 0o600))
			_, err := store.Get(t.Context(), t.TempDir())
			require.Error(t, err)
			require.NotContains(t, err.Error(), "secret decision")
			require.Error(t, store.Set(t.Context(), t.TempDir(), new(true)))
			data, err := os.ReadFile(store.Path)
			require.NoError(t, err)
			require.Equal(t, body, string(data))
		})
	}
}

func TestProjectTrustStoreConcurrentDecisions(t *testing.T) {
	store := ProjectTrustStore{Path: filepath.Join(t.TempDir(), "trust.json")}
	dirs := make([]string, 12)
	for i := range dirs {
		dirs[i] = t.TempDir()
	}
	var wg sync.WaitGroup
	for _, dir := range dirs {
		wg.Go(func() { require.NoError(t, store.Set(t.Context(), dir, new(true))) })
	}
	wg.Wait()
	for _, dir := range dirs {
		decision, err := store.Get(t.Context(), dir)
		require.NoError(t, err)
		require.Equal(t, new(true), decision)
	}
}

func TestProjectTrustStoreCancellationAndReadFailure(t *testing.T) {
	store := ProjectTrustStore{Path: filepath.Join(t.TempDir(), "trust.json")}
	require.NoError(t, os.Symlink(store.Path, store.Path))
	_, err := store.Get(t.Context(), t.TempDir())
	require.Error(t, err)
	require.Error(t, store.Set(t.Context(), t.TempDir(), new(true)))
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("trust cancelled")
	cancel(cause)
	_, err = store.Get(ctx, t.TempDir())
	require.ErrorIs(t, err, cause)
	require.ErrorIs(t, store.Set(ctx, t.TempDir(), new(true)), cause)
}

func TestHasTrustRequiringProjectResources(t *testing.T) {
	for _, entry := range []string{"settings.json", "mcp.json", "extensions", "skills", "prompts", "themes", "packages", "SYSTEM.md", "APPEND_SYSTEM.md"} {
		t.Run(entry, func(t *testing.T) {
			cwd, home := t.TempDir(), t.TempDir()
			protected, err := HasTrustRequiringProjectResources(t.Context(), cwd, home)
			require.NoError(t, err)
			require.False(t, protected)
			require.NoError(t, os.Mkdir(filepath.Join(cwd, ".tau"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(cwd, ".tau", entry), nil, 0o600))
			protected, err = HasTrustRequiringProjectResources(t.Context(), cwd, home)
			require.NoError(t, err)
			require.True(t, protected)
		})
	}
	t.Run("ancestor skills and global exemption", func(t *testing.T) {
		home := t.TempDir()
		cwd := filepath.Join(home, "workspace", "project")
		require.NoError(t, os.MkdirAll(cwd, 0o700))
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".agents", "skills"), 0o700))
		protected, err := HasTrustRequiringProjectResources(t.Context(), cwd, home)
		require.NoError(t, err)
		require.False(t, protected)
		require.NoError(t, os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("context"), 0o600))
		protected, err = HasTrustRequiringProjectResources(t.Context(), cwd, home)
		require.NoError(t, err)
		require.False(t, protected)
		require.NoError(t, os.MkdirAll(filepath.Join(home, "workspace", ".agents", "skills"), 0o700))
		protected, err = HasTrustRequiringProjectResources(t.Context(), cwd, home)
		require.NoError(t, err)
		require.True(t, protected)
	})
	t.Run("resource errors", func(t *testing.T) {
		cwd := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(cwd, ".tau"), 0o700))
		path := filepath.Join(cwd, ".tau", "settings.json")
		require.NoError(t, os.Symlink(path, path))
		_, err := HasTrustRequiringProjectResources(t.Context(), cwd, t.TempDir())
		require.Error(t, err)
	})
}

func TestResolveProjectTrustPrecedence(t *testing.T) {
	cwd, home := t.TempDir(), t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(cwd, ".tau"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(cwd, ".tau", "SYSTEM.md"), nil, 0o600))
	store := ProjectTrustStore{Path: filepath.Join(t.TempDir(), "trust.json")}
	for _, policy := range []string{"ask", "never", "unknown", "always"} {
		trusted, err := ResolveProjectTrust(t.Context(), store, cwd, home, policy, ProjectTrustOptions{})
		require.NoError(t, err)
		require.Equal(t, policy == "always", trusted)
	}
	require.NoError(t, store.Set(t.Context(), cwd, new(false)))
	trusted, err := ResolveProjectTrust(t.Context(), store, cwd, home, "always", ProjectTrustOptions{})
	require.NoError(t, err)
	require.False(t, trusted)
	called := false
	opts := ProjectTrustOptions{Decide: func(context.Context, string) (*bool, error) { called = true; return new(true), nil }}
	trusted, err = ResolveProjectTrust(t.Context(), store, cwd, home, "never", opts)
	require.NoError(t, err)
	require.True(t, trusted)
	require.True(t, called)
	called = false
	opts.Override = new(false)
	trusted, err = ResolveProjectTrust(t.Context(), store, cwd, home, "always", opts)
	require.NoError(t, err)
	require.False(t, trusted)
	require.False(t, called)
	opts.Override = nil
	opts.Decide = func(context.Context, string) (*bool, error) { return nil, nil }
	trusted, err = ResolveProjectTrust(t.Context(), store, cwd, home, "always", opts)
	require.NoError(t, err)
	require.False(t, trusted)
	cause := errors.New("trust decision failed")
	opts.Decide = func(context.Context, string) (*bool, error) { return nil, cause }
	_, err = ResolveProjectTrust(t.Context(), store, cwd, home, "always", opts)
	require.ErrorIs(t, err, cause)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	opts.Decide = func(context.Context, string) (*bool, error) { cancel(cause); return new(true), nil }
	_, err = ResolveProjectTrust(ctx, store, cwd, home, "always", opts)
	require.ErrorIs(t, err, cause)
	trusted, err = ResolveProjectTrust(t.Context(), store, t.TempDir(), home, "never", ProjectTrustOptions{})
	require.NoError(t, err)
	require.True(t, trusted)
}

func TestLoadTrustedSettingsNeverOpensDeniedProjectSettings(t *testing.T) {
	global, cwd, home := t.TempDir(), t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(global, "settings.json"), []byte(`{"defaultProjectTrust":"never","theme":"global","skills":["global-skills"]}`), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(cwd, ".tau"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(cwd, ".tau", "settings.json"), []byte(`{"invalid`), 0o600))
	settings, err := LoadTrustedSettings(t.Context(), global, cwd, home, ProjectTrustOptions{})
	require.NoError(t, err)
	require.False(t, settings.ProjectResourcesTrusted())
	require.Equal(t, "global", settings.Theme)
	_, err = LoadTrustedSettings(t.Context(), global, cwd, home, ProjectTrustOptions{Override: new(true)})
	require.ErrorContains(t, err, "load project settings")
	require.NoError(t, os.WriteFile(filepath.Join(cwd, ".tau", "settings.json"), []byte(`{"defaultProjectTrust":"always","theme":"project","skills":["project-skills"]}`), 0o600))
	settings, err = LoadTrustedSettings(t.Context(), global, cwd, home, ProjectTrustOptions{Override: new(true)})
	require.NoError(t, err)
	require.True(t, settings.ProjectResourcesTrusted())
	require.Equal(t, "project", settings.Theme)
	require.Equal(t, "never", settings.DefaultProjectTrust)
	data, err := settings.MarshalJSON()
	require.NoError(t, err)
	require.Contains(t, string(data), `"defaultProjectTrust":"never"`)
	settings, err = LoadTrustedSettings(t.Context(), global, cwd, home, ProjectTrustOptions{})
	require.NoError(t, err)
	require.False(t, settings.ProjectResourcesTrusted())
	require.Equal(t, "global", settings.Theme)
	require.NoError(t, ProjectTrustStore{Path: filepath.Join(global, "trust.json")}.Set(t.Context(), cwd, new(true)))
	settings, err = LoadTrustedSettings(t.Context(), global, cwd, home, ProjectTrustOptions{})
	require.NoError(t, err)
	require.True(t, settings.ProjectResourcesTrusted())
	require.Equal(t, "project", settings.Theme)
	clone := MergeSettings(settings, nil)
	*settings.projectTrusted = false
	require.True(t, clone.ProjectResourcesTrusted())
}

func TestProjectTrustPromptFollowsSavedAndGlobalPolicy(t *testing.T) {
	cwd, home := t.TempDir(), t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(cwd, ".tau"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(cwd, ".tau", "settings.json"), nil, 0o600))
	store := ProjectTrustStore{Path: filepath.Join(t.TempDir(), "trust.json")}
	calls := 0
	opts := ProjectTrustOptions{Prompt: func(context.Context, string) (*bool, error) { calls++; return new(true), nil }}
	trusted, err := ResolveProjectTrust(t.Context(), store, cwd, home, "ask", opts)
	require.NoError(t, err)
	require.True(t, trusted)
	require.Equal(t, 1, calls)
	trusted, err = ResolveProjectTrust(t.Context(), store, cwd, home, "never", opts)
	require.NoError(t, err)
	require.False(t, trusted)
	require.Equal(t, 1, calls)
	require.NoError(t, store.Set(t.Context(), cwd, new(false)))
	trusted, err = ResolveProjectTrust(t.Context(), store, cwd, home, "ask", opts)
	require.NoError(t, err)
	require.False(t, trusted)
	require.Equal(t, 1, calls)
}

func TestProjectTrustDoesNotFollowSessionDirectoryChanges(t *testing.T) {
	cwd, global, home := t.TempDir(), t.TempDir(), t.TempDir()
	settings, err := LoadTrustedSettings(t.Context(), global, cwd, home, ProjectTrustOptions{Override: new(true)})
	require.NoError(t, err)
	require.True(t, settings.ProjectResourcesTrustedAt(cwd))
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(cwd, alias))
	require.True(t, settings.ProjectResourcesTrustedAt(alias))
	require.False(t, settings.ProjectResourcesTrustedAt(t.TempDir()))
	require.False(t, settings.ProjectResourcesTrustedAt(filepath.Join(cwd, "missing")))
	require.True(t, (*Settings)(nil).ProjectResourcesTrustedAt(t.TempDir()))
}

func TestPromptProjectTrustChoices(t *testing.T) {
	for _, selection := range []int{-1, 0, 1, 2, 3, 4, 99} {
		t.Run(fmt.Sprint(selection), func(t *testing.T) {
			cwd := t.TempDir()
			store := ProjectTrustStore{Path: filepath.Join(t.TempDir(), "trust.json")}
			trusted, err := PromptProjectTrust(t.Context(), store, cwd, func(_ context.Context, title string, choices []string) (int, error) {
				require.Contains(t, title, cwd)
				require.Len(t, choices, 5)
				return selection, nil
			})
			require.NoError(t, err)
			require.Equal(t, selection == 0 || selection == 1 || selection == 4, *trusted)
			saved, err := store.Get(t.Context(), cwd)
			require.NoError(t, err)
			if selection == 0 || selection == 2 || selection == 4 {
				require.Equal(t, trusted, saved)
			} else {
				require.Nil(t, saved)
			}
			if selection == 4 {
				saved, err = store.Get(t.Context(), filepath.Dir(cwd))
				require.NoError(t, err)
				require.Equal(t, new(true), saved)
			}
		})
	}
}

func TestPromptProjectTrustFailureAndCancellationDoNotSave(t *testing.T) {
	cwd := t.TempDir()
	store := ProjectTrustStore{Path: filepath.Join(t.TempDir(), "trust.json")}
	cause := errors.New("dialog failed")
	_, err := PromptProjectTrust(t.Context(), store, cwd, func(context.Context, string, []string) (int, error) { return 0, cause })
	require.ErrorIs(t, err, cause)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	_, err = PromptProjectTrust(ctx, store, cwd, func(context.Context, string, []string) (int, error) { cancel(cause); return 0, nil })
	require.ErrorIs(t, err, cause)
	require.NoFileExists(t, store.Path)
	require.NoError(t, store.Set(t.Context(), cwd, new(false)))
	trusted, err := PromptProjectTrust(t.Context(), store, cwd, func(context.Context, string, []string) (int, error) { return 4, nil })
	require.NoError(t, err)
	require.True(t, *trusted)
	saved, err := store.Get(t.Context(), cwd)
	require.NoError(t, err)
	require.True(t, *saved)
}
