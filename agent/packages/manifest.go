// Package packages provides tau-package.toml parsing and package management.
package packages // import "github.com/losinggeneration/tau/agent/packages"

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// Manifest represents a parsed tau-package.toml manifest.
type Manifest struct {
	Package      PackageSection      `toml:"package"`
	Plugin       PluginSection       `toml:"plugin"`
	Build        BuildSection        `toml:"build"`
	Run          RunSection          `toml:"run"`
	Releases     []ReleaseSection    `toml:"release"`
	Reqs         RequirementsSection `toml:"requirements"`
	ConfigSchema string              `toml:"config_schema"`

	// Artifact sections accept both singular and plural TOML keys.
	// Singular: [[skill]], [[prompt]], [[theme]], [[extension]] (original format)
	// Plural:   [[skills]], [[prompts]], [[themes]], [[extensions]] (canonical format)
	Skills     []ArtifactPath `toml:"skill"`
	Prompts    []ArtifactPath `toml:"prompt"`
	Themes     []ArtifactPath `toml:"theme"`
	Extensions []ArtifactPath `toml:"extension"`

	// Canonical plural sections (for [[skills]], [[prompts]], [[themes]], [[extensions]])
	SkillsPlural     []ArtifactPath `toml:"skills"`
	PromptsPlural    []ArtifactPath `toml:"prompts"`
	ThemesPlural     []ArtifactPath `toml:"themes"`
	ExtensionsPlural []ArtifactPath `toml:"extensions"`
}

// PackageSection is required.
type PackageSection struct {
	Name        string   `toml:"name"`
	Version     string   `toml:"version"`
	Description string   `toml:"description"`
	Authors     []string `toml:"authors"`
	License     string   `toml:"license"`
	Homepage    string   `toml:"homepage"`
}

// PluginSection is optional.
type PluginSection struct {
	Name       string `toml:"name"`
	APIVersion string `toml:"api_version"`
	SHA256     string `toml:"sha256"`
}

// BuildSection is optional.
type BuildSection struct {
	Command string `toml:"command"`
}

// RunSection is optional.
type RunSection struct {
	Command string `toml:"command"`
}

// ReleaseSection is optional (zero or more).
type ReleaseSection struct {
	Platform string `toml:"platform"`
	URL      string `toml:"url"`
	SHA256   string `toml:"sha256"`
}

// RequirementsSection is optional.
type RequirementsSection struct {
	Runtime string `toml:"runtime"`
}

// ArtifactPath represents a [[skills]], [[prompts]], or [[themes]] entry.
type ArtifactPath struct {
	Path string `toml:"path"`
}

// Load loads a tau-package.toml from the given path.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	return Parse(data)
}

// Parse parses a tau-package.toml from raw bytes.
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("validate manifest: %w", err)
	}
	return &m, nil
}

// ParseManifest is a synonym for Parse, used by tests for API compatibility.
func ParseManifest(data []byte) (*Manifest, error) {
	return Parse(data)
}

// LoadManifest loads a tau-package.toml from a directory path.
// It returns an error if the manifest file doesn't exist or can't be parsed.
func LoadManifest(dir string) (*Manifest, error) {
	return Load(filepath.Join(dir, "tau-package.toml"))
}

// Validate checks that the manifest has required fields and is well-formed.
func (m *Manifest) Validate() error {
	if m.Package.Name == "" {
		return fmt.Errorf("missing required [package].name")
	}
	if m.Package.Version == "" {
		return fmt.Errorf("missing required [package].version")
	}
	hasPlugin := m.Plugin.APIVersion != ""
	hasSkills := len(m.AllSkills()) > 0
	hasPrompts := len(m.AllPrompts()) > 0
	hasThemes := len(m.AllThemes()) > 0
	hasExtensions := len(m.AllExtensions()) > 0
	if !hasPlugin && !hasSkills && !hasPrompts && !hasThemes && !hasExtensions {
		return fmt.Errorf("package has no [plugin] and no artifact sections (skills/prompts/themes/extensions)")
	}
	return nil
}

// HasPlugin returns true if the manifest declares a plugin.
func (m *Manifest) HasPlugin() bool {
	return m.Plugin.APIVersion != ""
}

// ArtifactCounts returns the number of skills, prompts, themes, and extensions in the manifest.
func (m *Manifest) ArtifactCounts() (skills, prompts, themes, extensions int) {
	return len(m.AllSkills()), len(m.AllPrompts()), len(m.AllThemes()), len(m.AllExtensions())
}

// AllSkills returns the combined list of skills from both [[skill]] and [[skills]] sections.
func (m *Manifest) AllSkills() []ArtifactPath {
	return append(append([]ArtifactPath{}, m.Skills...), m.SkillsPlural...)
}

// AllPrompts returns the combined list of prompts from both [[prompt]] and [[prompts]] sections.
func (m *Manifest) AllPrompts() []ArtifactPath {
	return append(append([]ArtifactPath{}, m.Prompts...), m.PromptsPlural...)
}

// AllThemes returns the combined list of themes from both [[theme]] and [[themes]] sections.
func (m *Manifest) AllThemes() []ArtifactPath {
	return append(append([]ArtifactPath{}, m.Themes...), m.ThemesPlural...)
}

// AllExtensions returns the combined list of extensions from both [[extension]] and [[extensions]] sections.
func (m *Manifest) AllExtensions() []ArtifactPath {
	return append(append([]ArtifactPath{}, m.Extensions...), m.ExtensionsPlural...)
}

// HasExtensions returns true if the manifest declares any extensions.
func (m *Manifest) HasExtensions() bool {
	return len(m.AllExtensions()) > 0
}

// IsEmpty returns true if the manifest has no content.
func (m *Manifest) IsEmpty() bool {
	return m.Package.Name == "" && m.Package.Version == ""
}
