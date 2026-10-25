package packages

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/losinggeneration/tau/piext"
)

func (m *InstallManager) installPiFromLocal(ctx context.Context, cfg Config, info os.FileInfo) error {
	sidecar := InstallSidecar{
		Source:      cfg.Source,
		Ref:         "",
		CommitSHA:   "",
		InstalledAt: info.ModTime().Format(time.RFC3339),
		Pi:          true,
	}
	return m.installPiFromDir(ctx, cfg, cfg.Source, sidecar)
}

func (m *InstallManager) installPiFromGitDir(ctx context.Context, cfg Config, dir, ref, commitSHA, installedAt string) error {
	sidecar := InstallSidecar{
		Source:      cfg.Source,
		Ref:         ref,
		CommitSHA:   commitSHA,
		InstalledAt: strings.TrimSuffix(installedAt, "\n"),
		Pi:          true,
	}
	return m.installPiFromDir(ctx, cfg, dir, sidecar)
}

func (m *InstallManager) installPiFromNPM(ctx context.Context, cfg Config) error {
	if IsOfflineMode() {
		return fmt.Errorf("tau install --pi needs network; re-run online")
	}

	tmpDir, err := os.MkdirTemp("", "tau-pi-npm-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	spec := parseNpmSpec(strings.TrimSpace(strings.TrimPrefix(cfg.Source, "npm:")))
	pkgSpec := spec.Name
	if spec.Version != "" {
		pkgSpec += "@" + spec.Version
	}
	tarball, err := m.npmPackPackage(ctx, pkgSpec, tmpDir)
	if err != nil {
		return fmt.Errorf("npm pack: %w", err)
	}
	extractDir := filepath.Join(tmpDir, "package")
	if err := extractNpmPackage(tarball, extractDir); err != nil {
		return fmt.Errorf("extract npm package: %w", err)
	}

	sidecar := InstallSidecar{
		Source:      cfg.Source,
		InstalledAt: time.Now().Format(time.RFC3339),
		Pinned:      spec.Pinned,
		Pi:          true,
	}
	return m.installPiFromDir(ctx, cfg, extractDir, sidecar)
}

func (m *InstallManager) installPiFromDir(ctx context.Context, cfg Config, sourceDir string, sidecar InstallSidecar) error {
	pkg, err := ParsePiPackage(sourceDir)
	if err != nil {
		return fmt.Errorf("load pi package metadata from %s: %w", sourceDir, err)
	}
	manifest := piManifestForPrompt(pkg)

	scope := ScopeUser
	if cfg.Project {
		scope = ScopeProject
	}
	installBase := m.userDir
	if scope == ScopeProject {
		installBase = m.projectDir
	}

	if !cfg.Force {
		mgr := NewManager(m.userDir, m.projectDir)
		if err := mgr.CheckDuplicate(pkg.Name, scope); err != nil {
			return err
		}
	}

	ok, err := m.PromptTrust(ctx, cfg, manifest, nil)
	if err != nil {
		return fmt.Errorf("trust prompt: %w", err)
	}
	if !ok {
		return fmt.Errorf("install declined")
	}

	installPath := filepath.Join(installBase, pkg.Name)
	if err := os.MkdirAll(filepath.Dir(installPath), 0o700); err != nil {
		return fmt.Errorf("create packages dir: %w", err)
	}
	if cfg.Force {
		if err := os.RemoveAll(installPath); err != nil {
			return fmt.Errorf("remove existing installation: %w", err)
		}
	}
	if cfg.Link && sourceDir == cfg.Source {
		if err := os.Remove(installPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove existing symlink: %w", err)
		}
		if err := os.Symlink(cfg.Source, installPath); err != nil {
			return fmt.Errorf("create symlink: %w", err)
		}
	} else if err := copyDir(sourceDir, installPath); err != nil {
		return fmt.Errorf("copy package: %w", err)
	}

	if pkg.HasDeps {
		if IsOfflineMode() {
			return fmt.Errorf("tau install --pi needs network; re-run online")
		}
		if err := m.installPiDeps(ctx, installPath, pkg.PeerDeps); err != nil {
			return fmt.Errorf("install pi package dependencies: %w", err)
		}
	}

	var shimDir, nodeBin string
	if len(pkg.Extensions) > 0 {
		shimDir = filepath.Join(filepath.Dir(m.userDir), "pi-ext-shim")
		if err := m.ensurePiShim(ctx, shimDir); err != nil {
			return fmt.Errorf("ensure pi-ext-shim: %w", err)
		}
		nodeBin, err = m.findNode()
		if err != nil {
			return fmt.Errorf("find node: %w", err)
		}
	}
	manifestBytes, err := GenerateManifest(pkg, shimDir, nodeBin)
	if err != nil {
		return fmt.Errorf("generate manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(installPath, "tau-package.toml"), manifestBytes, 0o600); err != nil {
		return fmt.Errorf("write generated manifest: %w", err)
	}

	if sidecar.InstalledAt == "" {
		sidecar.InstalledAt = time.Now().Format(time.RFC3339)
	}
	if sidecar.Ref == "" && strings.HasPrefix(sidecar.Source, "npm:") {
		sidecar.Ref = pkg.Version
	}
	sidecar.Scope = string(scope)
	sidecar.Pi = true
	if err := sidecar.Write(installPath); err != nil {
		return fmt.Errorf("write install sidecar: %w", err)
	}

	loaded, err := Load(filepath.Join(installPath, "tau-package.toml"))
	if err != nil {
		return fmt.Errorf("load generated manifest: %w", err)
	}
	m.notifyReloaders(ctx, pkg.Name, loaded)
	return nil
}

func piManifestForPrompt(pkg *PiPackage) *Manifest {
	m := &Manifest{
		Package: PackageSection{
			Name:        pkg.Name,
			Version:     pkg.Version,
			Description: pkg.Description,
		},
	}
	if len(pkg.Extensions) > 0 {
		m.Plugin = PluginSection{APIVersion: "1"}
	}
	for _, prompt := range cleanPiEntries(pkg.Prompts) {
		m.PromptsPlural = append(m.PromptsPlural, ArtifactPath{Path: prompt})
	}
	for _, theme := range cleanPiEntries(pkg.Themes) {
		m.ThemesPlural = append(m.ThemesPlural, ArtifactPath{Path: theme})
	}
	for _, skill := range cleanPiEntries(pkg.Skills) {
		m.SkillsPlural = append(m.SkillsPlural, ArtifactPath{Path: skill})
	}
	return m
}

func (m *InstallManager) installPiDeps(ctx context.Context, dir string, peerDeps []string) error {
	if m.npmInstall != nil {
		return m.npmInstall(ctx, dir, peerDeps)
	}
	args := []string{"install", "--omit=dev", "--prefix", dir}
	args = append(args, peerDeps...)
	cmd := npmCommand(ctx, m.NPMCommand, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("npm install: %w", err)
	}
	return nil
}

func (m *InstallManager) npmPackPackage(ctx context.Context, spec, dst string) (string, error) {
	if m.npmPack != nil {
		return m.npmPack(ctx, spec, dst)
	}
	cmd := npmCommand(ctx, m.NPMCommand, "pack", spec, "--pack-destination", dst)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("npm pack %s: %w", spec, err)
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return "", fmt.Errorf("npm pack did not report a tarball")
	}
	return filepath.Join(dst, filepath.Base(name)), nil
}

func extractNpmPackage(tarball, dst string) error {
	file, err := os.Open(tarball)
	if err != nil {
		return fmt.Errorf("open tarball: %w", err)
	}
	defer func() { _ = file.Close() }()

	gz, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("read gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		rel := strings.TrimPrefix(filepath.Clean(filepath.FromSlash(hdr.Name)), "package"+string(os.PathSeparator))
		if rel == "." || rel == "" || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			continue
		}
		path := filepath.Join(dst, rel)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o700); err != nil {
				return fmt.Errorf("create tar dir: %w", err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return fmt.Errorf("create tar file dir: %w", err)
			}
			out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return fmt.Errorf("create tar file: %w", err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				closeErr := out.Close()
				if closeErr != nil {
					return fmt.Errorf("copy tar file: %w; close: %v", err, closeErr)
				}
				return fmt.Errorf("copy tar file: %w", err)
			}
			if err := out.Close(); err != nil {
				return fmt.Errorf("close tar file: %w", err)
			}
		}
	}
	return nil
}

func (m *InstallManager) ensurePiShim(ctx context.Context, dir string) error {
	if m.ensureShim != nil {
		return m.ensureShim(ctx, dir)
	}
	_, err := piext.EnsureShim(ctx, dir, piext.TimedNPMInstall, piext.StderrStatus)
	return err
}

func (m *InstallManager) findNode() (string, error) {
	lookPath := exec.LookPath
	if m.lookPath != nil {
		lookPath = m.lookPath
	}
	path, err := lookPath("node")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) {
		path, err = filepath.Abs(path)
		if err != nil {
			return "", err
		}
	}
	return strings.TrimSpace(path), nil
}
