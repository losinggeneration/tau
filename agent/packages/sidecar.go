package packages

import (
	"encoding/json"
	"errors"
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
	// Pinned indicates whether the package was installed with an exact version.
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
func (s InstallSidecar) Write(pkgDir string) (err error) {
	path := sidecarPath(pkgDir)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal sidecar: %w", err)
	}
	file, err := os.CreateTemp(pkgDir, ".tau-sidecar-*")
	if err != nil {
		return fmt.Errorf("create sidecar: %w", err)
	}
	temporary := file.Name()
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, file.Close())
		}
		if removeErr := os.Remove(temporary); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write sidecar: %w", err)
	}
	if err := file.Close(); err != nil {
		closed = true
		return fmt.Errorf("close sidecar: %w", err)
	}
	closed = true
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("replace sidecar: %w", err)
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
	var s *InstallSidecar
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse sidecar: %w", err)
	}
	if s == nil {
		return nil, fmt.Errorf("sidecar must be an object")
	}
	return s, nil
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
	sidecar, err = LoadSidecar(pkgDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("load package sidecar: %w", err)
	}

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
