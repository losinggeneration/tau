package packages

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    *Manifest
		wantErr string
	}{
		{
			name: "minimal plugin package",
			input: `
[package]
name = "test-pkg"
version = "1.0.0"

[plugin]
api_version = "1"
`,
			want: &Manifest{
				Package: PackageSection{Name: "test-pkg", Version: "1.0.0"},
				Plugin:  PluginSection{APIVersion: "1"},
			},
		},
		{
			name: "package with all sections",
			input: `
[package]
name = "full-pkg"
version = "0.3.1"
description = "A test package"
authors = ["Alice", "Bob"]
license = "MIT"
homepage = "https://example.com"

[plugin]
api_version = "1"

[build]
command = "go build -o helper ./cmd/plugin"

[run]
command = "./helper"

[[release]]
platform = "linux/amd64"
url = "https://example.com/helper-linux-amd64.tar.gz"
sha256 = "abc123..."

[[release]]
platform = "darwin/arm64"
url = "https://example.com/helper-darwin-arm64.tar.gz"
sha256 = "def456..."

[requirements]
runtime = "python>=3.11"

[[skill]]
path = "skills/"

[[prompt]]
path = "prompts/"

[[theme]]
path = "themes/"
`,
			want: &Manifest{
				Package: PackageSection{
					Name:        "full-pkg",
					Version:     "0.3.1",
					Description: "A test package",
					Authors:     []string{"Alice", "Bob"},
					License:     "MIT",
					Homepage:    "https://example.com",
				},
				Plugin: PluginSection{APIVersion: "1"},
				Build:  BuildSection{Command: "go build -o helper ./cmd/plugin"},
				Run:    RunSection{Command: "./helper"},
				Releases: []ReleaseSection{
					{Platform: "linux/amd64", URL: "https://example.com/helper-linux-amd64.tar.gz", SHA256: "abc123..."},
					{Platform: "darwin/arm64", URL: "https://example.com/helper-darwin-arm64.tar.gz", SHA256: "def456..."},
				},
				Reqs:    RequirementsSection{Runtime: "python>=3.11"},
				Skills:  []ArtifactPath{{Path: "skills/"}},
				Prompts: []ArtifactPath{{Path: "prompts/"}},
				Themes:  []ArtifactPath{{Path: "themes/"}},
			},
		},
		{
			name:    "missing name",
			input:   "[package]\nversion = \"1.0.0\"\n[plugin]\napi_version = \"1\"",
			wantErr: "missing required [package].name",
		},
		{
			name:    "missing version",
			input:   "[package]\nname = \"test\"\n[plugin]\napi_version = \"1\"",
			wantErr: "missing required [package].version",
		},
		{
			name:    "no plugin and no artifacts",
			input:   "[package]\nname = \"test\"\nversion = \"1.0.0\"",
			wantErr: "package has no [plugin] and no artifact sections",
		},
		{
			name: "only skills is valid",
			input: `
[package]
name = "skill-pkg"
version = "1.0.0"

[[skill]]
path = "skills/"
`,
			want: &Manifest{
				Package: PackageSection{Name: "skill-pkg", Version: "1.0.0"},
				Skills:  []ArtifactPath{{Path: "skills/"}},
			},
		},
		{
			name: "only prompts is valid",
			input: `
[package]
name = "prompt-pkg"
version = "1.0.0"

[[prompt]]
path = "prompts/"
`,
			want: &Manifest{
				Package: PackageSection{Name: "prompt-pkg", Version: "1.0.0"},
				Prompts: []ArtifactPath{{Path: "prompts/"}},
			},
		},
		{
			name: "only themes is valid",
			input: `
[package]
name = "theme-pkg"
version = "1.0.0"

[[theme]]
path = "themes/"
`,
			want: &Manifest{
				Package: PackageSection{Name: "theme-pkg", Version: "1.0.0"},
				Themes:  []ArtifactPath{{Path: "themes/"}},
			},
		},
		{
			name: "canonical plural sections",
			input: `
[package]
name = "canonical-pkg"
version = "1.0.0"

[[skills]]
path = "skills/"

[[prompts]]
path = "prompts/"

[[themes]]
path = "themes/"
`,
			want: &Manifest{
				Package:       PackageSection{Name: "canonical-pkg", Version: "1.0.0"},
				SkillsPlural:  []ArtifactPath{{Path: "skills/"}},
				PromptsPlural: []ArtifactPath{{Path: "prompts/"}},
				ThemesPlural:  []ArtifactPath{{Path: "themes/"}},
			},
		},
		{
			name: "mixed singular and plural sections",
			input: `
[package]
name = "mixed-pkg"
version = "1.0.0"

[[skill]]
path = "skills1/"

[[skills]]
path = "skills2/"

[[prompt]]
path = "prompts1/"

[[prompts]]
path = "prompts2/"

[[theme]]
path = "themes1/"

[[themes]]
path = "themes2/"
`,
			want: &Manifest{
				Package:       PackageSection{Name: "mixed-pkg", Version: "1.0.0"},
				Skills:        []ArtifactPath{{Path: "skills1/"}},
				SkillsPlural:  []ArtifactPath{{Path: "skills2/"}},
				Prompts:       []ArtifactPath{{Path: "prompts1/"}},
				PromptsPlural: []ArtifactPath{{Path: "prompts2/"}},
				Themes:        []ArtifactPath{{Path: "themes1/"}},
				ThemesPlural:  []ArtifactPath{{Path: "themes2/"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse([]byte(tt.input))
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want.Package, got.Package)
			assert.Equal(t, tt.want.Plugin, got.Plugin)
			assert.Equal(t, tt.want.Build, got.Build)
			assert.Equal(t, tt.want.Run, got.Run)
			assert.Equal(t, tt.want.Releases, got.Releases)
			assert.Equal(t, tt.want.Reqs, got.Reqs)
			assert.Equal(t, tt.want.Skills, got.Skills)
			assert.Equal(t, tt.want.Prompts, got.Prompts)
			assert.Equal(t, tt.want.Themes, got.Themes)
		})
	}
}

func TestLoad(t *testing.T) {
	tmpDir := t.TempDir()
	manifestPath := filepath.Join(tmpDir, "tau-package.toml")
	err := os.WriteFile(manifestPath, []byte(`
[package]
name = "load-test"
version = "2.0.0"

[plugin]
api_version = "1"
`), 0o644)
	require.NoError(t, err)

	m, err := Load(manifestPath)
	require.NoError(t, err)
	assert.Equal(t, "load-test", m.Package.Name)
	assert.Equal(t, "2.0.0", m.Package.Version)
	assert.Equal(t, "1", m.Plugin.APIVersion)
}

func TestHasPlugin(t *testing.T) {
	assert.True(t, (&Manifest{Plugin: PluginSection{APIVersion: "1"}}).HasPlugin())
	assert.False(t, (&Manifest{Skills: []ArtifactPath{{Path: "skills/"}}}).HasPlugin())
}

func TestIsEmpty(t *testing.T) {
	assert.True(t, (&Manifest{}).IsEmpty())
	assert.False(t, (&Manifest{Package: PackageSection{Name: "test"}}).IsEmpty())
	assert.False(t, (&Manifest{Package: PackageSection{Version: "1.0.0"}}).IsEmpty())
}

func TestAllArtifacts(t *testing.T) {
	m := &Manifest{
		Skills:        []ArtifactPath{{Path: "skill1"}, {Path: "skill2"}},
		SkillsPlural:  []ArtifactPath{{Path: "skill3"}},
		Prompts:       []ArtifactPath{{Path: "prompt1"}},
		PromptsPlural: []ArtifactPath{{Path: "prompt2"}, {Path: "prompt3"}},
		Themes:        []ArtifactPath{{Path: "theme1"}},
		ThemesPlural:  []ArtifactPath{{Path: "theme2"}},
	}

	assert.Equal(t, []ArtifactPath{{Path: "skill1"}, {Path: "skill2"}, {Path: "skill3"}}, m.AllSkills())
	assert.Equal(t, []ArtifactPath{{Path: "prompt1"}, {Path: "prompt2"}, {Path: "prompt3"}}, m.AllPrompts())
	assert.Equal(t, []ArtifactPath{{Path: "theme1"}, {Path: "theme2"}}, m.AllThemes())
}

func TestAllArtifactsEmpty(t *testing.T) {
	m := &Manifest{}
	assert.Empty(t, m.AllSkills())
	assert.Empty(t, m.AllPrompts())
	assert.Empty(t, m.AllThemes())
}

func TestArtifactCounts(t *testing.T) {
	m := &Manifest{
		Skills:       []ArtifactPath{{Path: "skill1"}},
		SkillsPlural: []ArtifactPath{{Path: "skill2"}},
		Prompts:      []ArtifactPath{{Path: "prompt1"}, {Path: "prompt2"}},
		ThemesPlural: []ArtifactPath{{Path: "theme1"}},
	}

	// Should count combined singular + plural
	skills, prompts, themes, extensions := m.ArtifactCounts()
	assert.Equal(t, 2, skills)
	assert.Equal(t, 2, prompts)
	assert.Equal(t, 1, themes)
	assert.Equal(t, 0, extensions)
}

func TestManifestRejectsUnsafePackageNames(t *testing.T) {
	for _, name := range []string{".", "..", "../outside", "nested/name", "/absolute", `..\outside`, "@scope/../outside", "@scope/..", "@/name", "-option", ".tau-install-plugin", "@scope/.tau-install-plugin"} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(fmt.Sprintf("[package]\nname = %q\nversion = \"1\"\n[plugin]\napi_version = \"1\"\n", name)))
			require.Error(t, err)
		})
	}
}

func TestManifestAcceptsSafeInstallationNames(t *testing.T) {
	for _, name := range []string{"plugin", "my.plugin-1_2", "@scope/plugin", "@scope/my.plugin-1_2"} {
		t.Run(name, func(t *testing.T) {
			manifest, err := Parse([]byte(fmt.Sprintf("[package]\nname = %q\nversion = \"1\"\n[plugin]\napi_version = \"1\"\n", name)))
			require.NoError(t, err)
			require.Equal(t, name, manifest.Package.Name)
		})
	}
}
