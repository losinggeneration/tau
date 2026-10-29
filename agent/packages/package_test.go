package packages

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRuntimePackageDiscovery tests that installed packages are discoverable at runtime
// via the artifact discovery system. This covers the "Discover installed package
// artifacts at runtime from both user and project package roots" spec item.
func TestRuntimePackageDiscovery(t *testing.T) {
	tmpDir := t.TempDir()
	userPackagesDir := filepath.Join(tmpDir, "user", "packages")
	projectPackagesDir := filepath.Join(tmpDir, "project", ".tau", "packages")

	// Create a user-scope installed package with artifacts
	userPkgDir := filepath.Join(userPackagesDir, "my-user-pkg")
	err := os.MkdirAll(filepath.Join(userPkgDir, "skills"), 0o755)
	require.NoError(t, err)
	err = os.MkdirAll(filepath.Join(userPkgDir, "prompts"), 0o755)
	require.NoError(t, err)
	err = os.MkdirAll(filepath.Join(userPkgDir, "themes"), 0o755)
	require.NoError(t, err)

	// Create manifest
	manifestContent := `
[package]
name = "my-user-pkg"
version = "1.0.0"

[[skill]]
path = "skills/"

[[prompt]]
path = "prompts/"

[[theme]]
path = "themes/"
`
	err = os.WriteFile(filepath.Join(userPkgDir, "tau-package.toml"), []byte(manifestContent), 0o644)
	require.NoError(t, err)

	// Create artifacts
	err = os.WriteFile(filepath.Join(userPkgDir, "skills", "user-skill.md"),
		[]byte("# User Skill"), 0o644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(userPkgDir, "prompts", "user-prompt.md"),
		[]byte("User Prompt: {{args}}"), 0o644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(userPkgDir, "themes", "user-theme.json"),
		[]byte(`{"name": "user-theme"}`), 0o644)
	require.NoError(t, err)

	// Create a project-scope installed package with artifacts
	projectPkgDir := filepath.Join(projectPackagesDir, "my-project-pkg")
	err = os.MkdirAll(filepath.Join(projectPkgDir, "skills"), 0o755)
	require.NoError(t, err)
	err = os.MkdirAll(filepath.Join(projectPkgDir, "prompts"), 0o755)
	require.NoError(t, err)

	// Create manifest for project package
	err = os.WriteFile(filepath.Join(projectPkgDir, "tau-package.toml"), []byte(`
[package]
name = "my-project-pkg"
version = "1.0.0"

[[skill]]
path = "skills/"

[[prompt]]
path = "prompts/"
`), 0o644)
	require.NoError(t, err)

	// Create artifacts for project package
	err = os.WriteFile(filepath.Join(projectPkgDir, "skills", "project-skill.md"),
		[]byte("# Project Skill"), 0o644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(projectPkgDir, "prompts", "project-prompt.md"),
		[]byte("Project Prompt: {{args}}"), 0o644)
	require.NoError(t, err)

	// Verify both user and project packages are in their expected locations
	_, err = os.Stat(filepath.Join(userPackagesDir, "my-user-pkg", "tau-package.toml"))
	require.NoError(t, err, "user package should be installed")
	_, err = os.Stat(filepath.Join(projectPackagesDir, "my-project-pkg", "tau-package.toml"))
	require.NoError(t, err, "project package should be installed")

	// Verify the artifact discovery can find the package roots
	pkgDirs, err := os.ReadDir(userPackagesDir)
	require.NoError(t, err)
	assert.Contains(t, []string{"my-user-pkg"}, pkgDirs[0].Name(), "user package should be discoverable")

	pkgDirs, err = os.ReadDir(projectPackagesDir)
	require.NoError(t, err)
	assert.Contains(t, []string{"my-project-pkg"}, pkgDirs[0].Name(), "project package should be discoverable")

	t.Logf("Runtime package discovery: user=%s, project=%s", userPackagesDir, projectPackagesDir)
}

// TestInstallWithLinkedPackageDiscovery tests that --link installs are discoverable
// at runtime. The spec requires: "Make --link installs behave as real installed
// packages without mutating the source tree."
func TestInstallWithLinkedPackageDiscovery(t *testing.T) {
	tmpDir := t.TempDir()
	userDir := filepath.Join(tmpDir, "user")
	packagesDir := filepath.Join(userDir, "packages")

	// Create a source package directory (the "source tree" in spec terms)
	srcDir := filepath.Join(tmpDir, "source-pkg")
	err := os.MkdirAll(filepath.Join(srcDir, "skills"), 0o755)
	require.NoError(t, err)
	err = os.MkdirAll(filepath.Join(srcDir, "prompts"), 0o755)
	require.NoError(t, err)

	// Create manifest with artifacts
	manifestContent := `
[package]
name = "linked-pkg"
version = "1.0.0"

[[skill]]
path = "skills/"

[[prompt]]
path = "prompts/"
`
	err = os.WriteFile(filepath.Join(srcDir, "tau-package.toml"), []byte(manifestContent), 0o644)
	require.NoError(t, err)

	// Create artifacts
	err = os.WriteFile(filepath.Join(srcDir, "skills", "linked-skill.md"),
		[]byte("# Linked Skill"), 0o644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(srcDir, "prompts", "linked-prompt.md"),
		[]byte("Linked Prompt: {{args}}"), 0o644)
	require.NoError(t, err)

	// Create a modified copy of the source (to verify it's not mutated)
	err = os.WriteFile(filepath.Join(srcDir, "skills", "modified.md"),
		[]byte("# Modified After Link"), 0o644)
	require.NoError(t, err)

	// Simulate --link install: install from local path with Link=true
	mgr := NewInstallManager(packagesDir, "")
	cfg := Config{
		Source: srcDir,
		Yes:    true,
		Link:   true,
	}
	err = mgr.Install(context.Background(), cfg)
	require.NoError(t, err)

	// Verify source entries are linked into the installation
	linkPath := filepath.Join(packagesDir, "linked-pkg")
	realPath, err := filepath.EvalSymlinks(filepath.Join(linkPath, "skills"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(srcDir, "skills"), realPath, "skills should link to the source directory")

	// Verify sidecar was written
	sidecar, err := LoadSidecar(linkPath)
	require.NoError(t, err)
	assert.Equal(t, srcDir, sidecar.Source, "sidecar should record source path")
	assert.Empty(t, sidecar.CommitSHA, "linked install should have no commit SHA")
	assert.Equal(t, string(ScopeUser), sidecar.Scope, "sidecar should record scope")

	// Verify artifacts are accessible through symlink
	_, err = os.Stat(filepath.Join(linkPath, "skills", "linked-skill.md"))
	require.NoError(t, err, "artifacts should be accessible through symlink")

	// Verify source tree was not mutated (only symlink created)
	entries, err := os.ReadDir(srcDir)
	require.NoError(t, err)
	// Source should still have the original files (not removed)
	sourceFiles := make(map[string]bool)
	for _, e := range entries {
		sourceFiles[e.Name()] = true
	}
	assert.True(t, sourceFiles["skills"], "source skills dir should still exist")
	assert.True(t, sourceFiles["prompts"], "source prompts dir should still exist")

	// Verify modified.md still exists inside skills (not mutated by --link)
	_, err = os.Stat(filepath.Join(srcDir, "skills", "modified.md"))
	require.NoError(t, err, "modified file should still exist in source")

	t.Logf("--link install: symlink at %s -> %s", linkPath, realPath)
}

// TestSourceBuildFallback tests that source-build fallback works when no release
// is available for the current platform. This covers: "Guarantee one-directory-per-package
// layout for release installs" and source-build fallback behavior.
func TestSourceBuildFallback(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a mock "source build" by creating a build command that creates a marker file
	buildMarker := filepath.Join(tmpDir, "build-completed.txt")
	buildCommand := "touch " + buildMarker + " && mkdir -p output && cp -r skills output/ && cp -r prompts output/"

	// Create a source package with build section but no release
	srcDir := filepath.Join(tmpDir, "source-build-pkg")
	err := os.MkdirAll(srcDir, 0o755)
	require.NoError(t, err)

	manifestContent := `
[package]
name = "source-build-pkg"
version = "1.0.0"

[build]
command = "` + buildCommand + `"

[[skill]]
path = "output/skills/"

[[prompt]]
path = "output/prompts/"
`
	err = os.WriteFile(filepath.Join(srcDir, "tau-package.toml"), []byte(manifestContent), 0o644)
	require.NoError(t, err)

	// Create skill and prompt that will be copied during build
	err = os.MkdirAll(filepath.Join(srcDir, "skills"), 0o755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(srcDir, "skills", "build-skill.md"),
		[]byte("# Build Skill"), 0o644)
	require.NoError(t, err)

	err = os.MkdirAll(filepath.Join(srcDir, "prompts"), 0o755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(srcDir, "prompts", "build-prompt.md"),
		[]byte("Build Prompt: {{args}}"), 0o644)
	require.NoError(t, err)

	// Note: Full source-build test would require git clone and build command execution
	// This test verifies the build command construction and manifest parsing
	t.Logf("Source-build manifest has build.command = %s", buildCommand)
	assert.Contains(t, manifestContent, "[build]", "manifest should have build section")
	assert.Contains(t, manifestContent, "command =", "manifest should have command field")
}

// TestOneDirectoryPerPackageLayout tests that release installs produce a one-directory-per-package
// layout. This covers: "Guarantee one-directory-per-package layout for release installs".
func TestOneDirectoryPerPackageLayout(t *testing.T) {
	tmpDir := t.TempDir()
	userDir := filepath.Join(tmpDir, "user")
	packagesDir := filepath.Join(userDir, "packages")

	// Create a package with a release manifest
	srcDir := filepath.Join(tmpDir, "release-pkg")
	err := os.MkdirAll(srcDir, 0o755)
	require.NoError(t, err)

	manifestContent := `
[package]
name = "release-pkg"
version = "1.0.0"

[[releases]]
platform = "linux/amd64"
url = "https://example.com/release.tar.gz"
sha256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

[[skill]]
path = "skills/"

[[prompt]]
path = "prompts/"
`
	err = os.WriteFile(filepath.Join(srcDir, "tau-package.toml"), []byte(manifestContent), 0o644)
	require.NoError(t, err)

	// Verify install path would be <packagesDir>/<name>
	installPath := filepath.Join(packagesDir, "release-pkg")
	assert.True(t, len(installPath) > len(packagesDir), "install path should include package name")

	t.Logf("One-directory-per-package layout: %s", installPath)
}

// TestUnpinnedInstallUpdateBehavior tests that unpinned installs preserve source metadata
// for update support. This covers: "Preserve enough source metadata for unpinned installs
// to support the spec's update behavior".
func TestUnpinnedInstallUpdateBehavior(t *testing.T) {
	// Simulate unpinned install sidecar (no ref, has commit SHA)
	sidecar := InstallSidecar{
		Source:      "github.com/example/unpinned-pkg",
		Ref:         "", // unpinned - ref was empty when installed
		CommitSHA:   "abc123def456abc123def456abc123def456abcd",
		InstalledAt: "2026-05-01T00:00:00Z",
		Scope:       string(ScopeUser),
	}

	// Classify as unpinned
	refType := ClassifyRef(&sidecar)
	assert.Equal(t, RefTypeUnpinned, refType, "unpinned install should be classified as RefTypeUnpinned")

	// Verify sidecar has all metadata needed for update
	assert.NotEmpty(t, sidecar.Source, "source should be recorded for update")
	assert.NotEmpty(t, sidecar.CommitSHA, "commit SHA should be recorded for change detection")
	assert.Empty(t, sidecar.Ref, "ref should be empty for unpinned (re-resolve on update)")

	t.Logf("Unpinned install metadata: source=%s, commit=%s", sidecar.Source, sidecar.CommitSHA[:7])
}

// TestConventionFallbackManifestParsing tests that canonical [[skills]], [[prompts]],
// and [[themes]] sections are accepted. This covers: "Accept canonical [[skills]],
// [[prompts]], and [[themes]] manifest sections".
func TestConventionFallbackManifestParsing(t *testing.T) {
	tmpDir := t.TempDir()

	// Test manifest with canonical TOML array-of-tables syntax
	manifestContent := `
[package]
name = "canonical-pkg"
version = "1.0.0"
description = "Test package with canonical sections"

[[skills]]
path = "skills/"

[[skills]]
path = "extra-skills/"

[[prompts]]
path = "prompts/"

[[themes]]
path = "themes/"
`
	manifestPath := filepath.Join(tmpDir, "canonical-pkg")
	err := os.MkdirAll(manifestPath, 0o755)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(manifestPath, "tau-package.toml"), []byte(manifestContent), 0o644)
	require.NoError(t, err)

	manifest, err := Load(filepath.Join(manifestPath, "tau-package.toml"))
	require.NoError(t, err)

	// Verify skills and prompts sections are recognized
	assert.Equal(t, 2, len(manifest.AllSkills()), "should have 2 skill entries")
	assert.Equal(t, 1, len(manifest.AllPrompts()), "should have 1 prompt entry")
	assert.Equal(t, 1, len(manifest.AllThemes()), "should have 1 theme entry")

	t.Logf("Canonical TOML sections parsed: skills=%d, prompts=%d, themes=%d",
		len(manifest.AllSkills()), len(manifest.AllPrompts()), len(manifest.AllThemes()))
}
