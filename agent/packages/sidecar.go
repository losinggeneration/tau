package packages

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// InstallSidecar represents the .tau-install.json sidecar file.
type InstallSidecar struct {
	Source      string `json:"source"`
	Ref         string `json:"ref"`
	CommitSHA   string `json:"commit_sha"`
	InstalledAt string `json:"installed_at"`
	Scope       string `json:"scope"`
	// Pinned indicates whether the package was installed with an explicit version.
	// This only applies to npm packages installed via npm:<name>@<version>.
	Pinned bool `json:"pinned,omitempty"`
	// Pi marks packages installed from package.json's pi extension metadata.
	Pi bool `json:"pi,omitempty"`
}

// sidecarPath returns the path to the install sidecar file.
func sidecarPath(pkgDir string) string {
	return filepath.Join(pkgDir, ".tau-install.json")
}

// Write writes the install sidecar to the package directory.
func (s InstallSidecar) Write(pkgDir string) error {
	path := sidecarPath(pkgDir)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal sidecar: %w", err)
	}
	// Default to reasonable permissions
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write sidecar: %w", err)
	}
	return nil
}

// LoadSidecar loads the install sidecar from a package directory.
func LoadSidecar(pkgDir string) (*InstallSidecar, error) {
	path := sidecarPath(pkgDir)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read sidecar: %w", err)
	}
	var s InstallSidecar
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse sidecar: %w", err)
	}
	return &s, nil
}

// Info returns information about an installed package.
type Info struct {
	Name        string
	Version     string
	Description string
	Source      string
	Ref         string
	CommitSHA   string
	InstalledAt string
	Scope       Scope
	Path        string
}

// GetInfo returns detailed information about an installed package.
func GetInfo(pkgDir string) (*Info, error) {
	// Load manifest
	manifestPath := filepath.Join(pkgDir, "tau-package.toml")
	manifest, err := Load(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("load manifest: %w", err)
	}

	// Load sidecar (optional)
	var sidecar *InstallSidecar
	sidecar, _ = LoadSidecar(pkgDir)

	info := &Info{
		Name:        manifest.Package.Name,
		Version:     manifest.Package.Version,
		Description: manifest.Package.Description,
		Path:        pkgDir,
	}

	if sidecar != nil {
		info.Source = sidecar.Source
		info.Ref = sidecar.Ref
		info.CommitSHA = sidecar.CommitSHA
		info.InstalledAt = sidecar.InstalledAt
		info.Scope = Scope(sidecar.Scope)
	}

	return info, nil
}
