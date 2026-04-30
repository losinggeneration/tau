package packages

import (
	"context"
	"errors"
	"testing"
)

// The post-install reload hooks used to be an inline block copied into all six
// install paths, so the only way to reach them was to drive a whole install:
// four tests each built a git repo, two of them also an HTTP server and a
// tarball, and every one of them asserted a single bool.
//
// With the block extracted these are ordinary unit tests. They also cover cases
// the integration tests could not reach at all — nil hooks, a nil manifest, a
// hook that returns an error — because arranging those through a real install
// was more trouble than it was worth.

func manifestWithPlugin(t *testing.T) *Manifest {
	t.Helper()
	m := &Manifest{}
	m.Package.Name = "pkg"
	// HasPlugin keys off APIVersion, not a path.
	m.Plugin.APIVersion = "1"
	if !m.HasPlugin() {
		t.Fatalf("fixture does not report a plugin; HasPlugin's shape changed: %#v", m)
	}
	return m
}

func manifestWithoutPlugin(t *testing.T) *Manifest {
	t.Helper()
	m := &Manifest{}
	m.Package.Name = "pkg"
	if m.HasPlugin() {
		t.Fatalf("fixture unexpectedly reports a plugin: %#v", m)
	}
	return m
}

func TestNotifyReloaders_CallsBothHooksForAPluginPackage(t *testing.T) {
	var artifactCalls int
	var pluginName string
	var pluginCalls int

	m := &InstallManager{
		ArtifactReloader: func(context.Context) error { artifactCalls++; return nil },
		PluginReloader: func(_ context.Context, name string) error {
			pluginCalls++
			pluginName = name
			return nil
		},
	}

	m.notifyReloaders(context.Background(), "my-package", manifestWithPlugin(t))

	if artifactCalls != 1 {
		t.Errorf("ArtifactReloader called %d times, want 1", artifactCalls)
	}
	if pluginCalls != 1 {
		t.Errorf("PluginReloader called %d times, want 1", pluginCalls)
	}
	if pluginName != "my-package" {
		t.Errorf("PluginReloader got package name %q, want %q", pluginName, "my-package")
	}
}

// A package with no plugin must not trigger a plugin reload — reloading a
// plugin that does not exist is at best wasted work.
func TestNotifyReloaders_SkipsPluginHookWithoutAPlugin(t *testing.T) {
	var pluginCalls int
	m := &InstallManager{
		ArtifactReloader: func(context.Context) error { return nil },
		PluginReloader:   func(context.Context, string) error { pluginCalls++; return nil },
	}

	m.notifyReloaders(context.Background(), "pkg", manifestWithoutPlugin(t))

	if pluginCalls != 0 {
		t.Errorf("PluginReloader called %d times for a package with no plugin", pluginCalls)
	}
}

// The npm path loads the manifest opportunistically and tolerates it being
// absent, so nil must not panic.
func TestNotifyReloaders_NilManifestSkipsThePluginHook(t *testing.T) {
	var artifactCalls, pluginCalls int
	m := &InstallManager{
		ArtifactReloader: func(context.Context) error { artifactCalls++; return nil },
		PluginReloader:   func(context.Context, string) error { pluginCalls++; return nil },
	}

	m.notifyReloaders(context.Background(), "pkg", nil)

	if artifactCalls != 1 {
		t.Errorf("ArtifactReloader called %d times, want 1", artifactCalls)
	}
	if pluginCalls != 0 {
		t.Errorf("PluginReloader called %d times with a nil manifest", pluginCalls)
	}
}

// Hooks are optional; a manager with neither set must be a no-op, not a panic.
func TestNotifyReloaders_NilHooksAreSafe(t *testing.T) {
	m := &InstallManager{}
	m.notifyReloaders(context.Background(), "pkg", manifestWithPlugin(t))
}

// A failing artifact reload must not stop the plugin reload. The package is
// already installed; skipping the second hook would leave it half-visible.
func TestNotifyReloaders_ArtifactFailureDoesNotSkipThePluginHook(t *testing.T) {
	var pluginCalls int
	m := &InstallManager{
		ArtifactReloader: func(context.Context) error { return errors.New("reload boom") },
		PluginReloader:   func(context.Context, string) error { pluginCalls++; return nil },
	}

	m.notifyReloaders(context.Background(), "pkg", manifestWithPlugin(t))

	if pluginCalls != 1 {
		t.Errorf("PluginReloader called %d times after an artifact-reload failure, want 1", pluginCalls)
	}
}

// Hook errors are warnings, not failures: notifyReloaders returns nothing, so
// a failing hook must not be able to abort an install that already succeeded
// on disk. This pins that contract.
func TestNotifyReloaders_HookErrorsDoNotPropagate(t *testing.T) {
	m := &InstallManager{
		ArtifactReloader: func(context.Context) error { return errors.New("artifact boom") },
		PluginReloader:   func(context.Context, string) error { return errors.New("plugin boom") },
	}

	// Compiles only while notifyReloaders returns no error, and must not panic.
	m.notifyReloaders(context.Background(), "pkg", manifestWithPlugin(t))
}

// The context reaches both hooks, so a cancelled install can stop a reload.
func TestNotifyReloaders_PassesTheContextThrough(t *testing.T) {
	type ctxKey string
	const key ctxKey = "marker"
	ctx := context.WithValue(context.Background(), key, "present")

	var artifactSaw, pluginSaw any
	m := &InstallManager{
		ArtifactReloader: func(c context.Context) error { artifactSaw = c.Value(key); return nil },
		PluginReloader:   func(c context.Context, _ string) error { pluginSaw = c.Value(key); return nil },
	}

	m.notifyReloaders(ctx, "pkg", manifestWithPlugin(t))

	if artifactSaw != "present" {
		t.Errorf("ArtifactReloader got a context without the caller's values: %v", artifactSaw)
	}
	if pluginSaw != "present" {
		t.Errorf("PluginReloader got a context without the caller's values: %v", pluginSaw)
	}
}
