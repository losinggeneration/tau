package packages

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsHidden(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected bool
	}{
		{"hidden file", ".hidden", true},
		{"hidden dir file", ".hidden/file.txt", true},
		{"normal file", "file.txt", false},
		{"normal dir file", "dir/file.txt", false},
		{"hidden in normal dir", "dir/.hidden", true},
		{"double hidden", "a/.hidden/b", true},
		{"deep hidden", "a/b/c/.hidden", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isHidden(tt.path)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestUnescapeIgnorePattern(t *testing.T) {
	tests := []struct {
		name     string
		pattern  string
		expected string
	}{
		{"normal", "*.log", "*.log"},
		{"escaped bang", "\\!important.txt", "!important.txt"},
		{"escaped hash", "\\#note.txt", "#note.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := unescapeIgnorePattern(tt.pattern)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestCollectPackageResources_RespectsGitignore(t *testing.T) {
	tmpDir := t.TempDir()

	// Create tau.toml
	tauToml := `[package]
name = "test-pkg"
version = "1.0.0"

[plugin]
api_version = "1"
`
	err := os.WriteFile(filepath.Join(tmpDir, "tau.toml"), []byte(tauToml), 0o644)
	require.NoError(t, err)

	// Create .gitignore
	err = os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(`
# Ignore log files
*.log
build
node_modules
`), 0o644)
	require.NoError(t, err)

	// Create extensions directory with files
	extDir := filepath.Join(tmpDir, "extensions")
	err = os.MkdirAll(extDir, 0o755)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(extDir, "index.ts"), []byte("export {}"), 0o644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(extDir, "debug.log"), []byte("log"), 0o644)
	require.NoError(t, err)

	// Create skills directory with files
	skillsDir := filepath.Join(tmpDir, "skills")
	err = os.MkdirAll(skillsDir, 0o755)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(skillsDir, "SKILL.md"), []byte("# Skill"), 0o644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(skillsDir, "readme.md"), []byte("# Readme"), 0o644)
	require.NoError(t, err)

	// Create prompts directory with files
	promptsDir := filepath.Join(tmpDir, "prompts")
	err = os.MkdirAll(promptsDir, 0o755)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(promptsDir, "prompt.md"), []byte("# Prompt"), 0o644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(promptsDir, "debug.log"), []byte("log"), 0o644)
	require.NoError(t, err)

	// Create themes directory with files
	themesDir := filepath.Join(tmpDir, "themes")
	err = os.MkdirAll(themesDir, 0o755)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(themesDir, "theme.json"), []byte("{}"), 0o644)
	require.NoError(t, err)

	resources, err := CollectPackageResources(tmpDir, nil)
	require.NoError(t, err)

	// Build a map of collected resource paths
	resourcePaths := make(map[string]bool)
	for _, r := range resources {
		rel, _ := filepath.Rel(tmpDir, r.Path)
		resourcePaths[rel] = true
	}

	// Extensions should be found (but not the .log file)
	assert.True(t, resourcePaths["extensions/index.ts"], "extensions/index.ts should be included")

	// Skills should be found
	assert.True(t, resourcePaths["skills/SKILL.md"], "skills/SKILL.md should be included")
	assert.True(t, resourcePaths["skills/readme.md"], "skills/readme.md should be included")

	// Prompts should be found (but not the .log file)
	assert.True(t, resourcePaths["prompts/prompt.md"], "prompts/prompt.md should be included")

	// Themes should be found
	assert.True(t, resourcePaths["themes/theme.json"], "themes/theme.json should be included")

	// Log files should be ignored
	assert.False(t, resourcePaths["extensions/debug.log"], "extensions/debug.log should be ignored")
	assert.False(t, resourcePaths["prompts/debug.log"], "prompts/debug.log should be ignored")
}

func TestCollectPackageResources_RespectsNestedIgnoreFiles(t *testing.T) {
	tmpDir := t.TempDir()

	// Create tau.toml
	tauToml := `[package]
name = "test-pkg"
version = "1.0.0"
`
	err := os.WriteFile(filepath.Join(tmpDir, "tau.toml"), []byte(tauToml), 0o644)
	require.NoError(t, err)

	// Create extensions with subdir with its own .gitignore
	extDir := filepath.Join(tmpDir, "extensions")
	err = os.MkdirAll(extDir, 0o755)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(extDir, "index.ts"), []byte("export {}"), 0o644)
	require.NoError(t, err)

	// Create vendor dir with .gitignore that ignores everything
	vendorDir := filepath.Join(extDir, "vendor")
	err = os.MkdirAll(vendorDir, 0o755)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(vendorDir, ".gitignore"), []byte(`
*
`), 0o644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(vendorDir, "library.js"), []byte("module.exports"), 0o644)
	require.NoError(t, err)

	resources, err := CollectPackageResources(tmpDir, nil)
	require.NoError(t, err)

	// The vendor directory should be ignored by vendor/.gitignore
	found := false
	for _, r := range resources {
		rel, _ := filepath.Rel(tmpDir, r.Path)
		if rel == "extensions/vendor/library.js" {
			found = true
			break
		}
	}
	assert.False(t, found, "extensions/vendor/library.js should be ignored by vendor/.gitignore")
}

func TestCollectPackageResources_RespectsAllIgnoreFileTypes(t *testing.T) {
	tmpDir := t.TempDir()

	// Create tau.toml
	tauToml := `[package]
name = "test-pkg"
version = "1.0.0"
`
	err := os.WriteFile(filepath.Join(tmpDir, "tau.toml"), []byte(tauToml), 0o644)
	require.NoError(t, err)

	// Create .gitignore (for *.log files)
	err = os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(`
*.log
`), 0o644)
	require.NoError(t, err)

	// Create .ignore (for *.swp files)
	err = os.WriteFile(filepath.Join(tmpDir, ".ignore"), []byte(`
*.swp
`), 0o644)
	require.NoError(t, err)

	// Create extensions directory
	extDir := filepath.Join(tmpDir, "extensions")
	err = os.MkdirAll(extDir, 0o755)
	require.NoError(t, err)

	// Create .fdignore in subdir (for *.bak files)
	err = os.WriteFile(filepath.Join(extDir, ".fdignore"), []byte(`
*.bak
`), 0o644)
	require.NoError(t, err)

	// Create files in extensions
	files := []string{
		"index.ts",
		"debug.log",
		"debug.swp",
		"backup.bak",
	}
	for _, f := range files {
		path := filepath.Join(extDir, f)
		err = os.WriteFile(path, []byte("content"), 0o644)
		require.NoError(t, err)
	}

	resources, err := CollectPackageResources(tmpDir, nil)
	require.NoError(t, err)

	// Build a map of collected resource paths
	resourcePaths := make(map[string]bool)
	for _, r := range resources {
		rel, _ := filepath.Rel(tmpDir, r.Path)
		resourcePaths[rel] = true
	}

	// index.ts should be included
	assert.True(t, resourcePaths["extensions/index.ts"], "extensions/index.ts should be included")

	// All ignored files should NOT be included
	assert.False(t, resourcePaths["extensions/debug.log"], "extensions/debug.log should be ignored by .gitignore")
	assert.False(t, resourcePaths["extensions/debug.swp"], "extensions/debug.swp should be ignored by .ignore")
	assert.False(t, resourcePaths["extensions/backup.bak"], "extensions/backup.bak should be ignored by .fdignore")
}

func TestCollectPackageResources_RespectsHiddenFiles(t *testing.T) {
	tmpDir := t.TempDir()

	// Create tau.toml
	tauToml := `[package]
name = "test-pkg"
version = "1.0.0"
`
	err := os.WriteFile(filepath.Join(tmpDir, "tau.toml"), []byte(tauToml), 0o644)
	require.NoError(t, err)

	// Create extensions directory with hidden files
	extDir := filepath.Join(tmpDir, "extensions")
	err = os.MkdirAll(extDir, 0o755)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(extDir, "index.ts"), []byte("export {}"), 0o644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(extDir, ".hidden.ts"), []byte("secret"), 0o644)
	require.NoError(t, err)

	resources, err := CollectPackageResources(tmpDir, nil)
	require.NoError(t, err)

	// Build a map of collected resource paths
	resourcePaths := make(map[string]bool)
	for _, r := range resources {
		rel, _ := filepath.Rel(tmpDir, r.Path)
		resourcePaths[rel] = true
	}

	// index.ts should be included
	assert.True(t, resourcePaths["extensions/index.ts"], "extensions/index.ts should be included")

	// Hidden files should NOT be included
	assert.False(t, resourcePaths["extensions/.hidden.ts"], "extensions/.hidden.ts should be ignored (hidden file)")
}

func TestCollectPackageResources_RespectsNodeModules(t *testing.T) {
	tmpDir := t.TempDir()

	// Create tau.toml
	tauToml := `[package]
name = "test-pkg"
version = "1.0.0"
`
	err := os.WriteFile(filepath.Join(tmpDir, "tau.toml"), []byte(tauToml), 0o644)
	require.NoError(t, err)

	// Create extensions directory
	extDir := filepath.Join(tmpDir, "extensions")
	err = os.MkdirAll(extDir, 0o755)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(extDir, "index.ts"), []byte("export {}"), 0o644)
	require.NoError(t, err)

	// Create node_modules with a .ts file that should be ignored
	nodeModulesDir := filepath.Join(extDir, "node_modules", "lodash")
	err = os.MkdirAll(nodeModulesDir, 0o755)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(nodeModulesDir, "index.ts"), []byte("export default {}"), 0o644)
	require.NoError(t, err)

	resources, err := CollectPackageResources(tmpDir, nil)
	require.NoError(t, err)

	// Build a map of collected resource paths
	resourcePaths := make(map[string]bool)
	for _, r := range resources {
		rel, _ := filepath.Rel(tmpDir, r.Path)
		resourcePaths[rel] = true
	}

	// index.ts should be included
	assert.True(t, resourcePaths["extensions/index.ts"], "extensions/index.ts should be included")

	// Files in node_modules should NOT be included
	assert.False(t, resourcePaths["extensions/node_modules/lodash/index.ts"], "node_modules should be ignored")
}
