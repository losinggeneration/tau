package packages

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

func (m *InstallManager) installPiFromNPM(ctx context.Context, cfg Config) (err error) {
	if IsOfflineMode() {
		return fmt.Errorf("tau install --pi needs network; re-run online")
	}

	tmpDir, err := os.MkdirTemp("", "tau-pi-npm-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(tmpDir)) }()

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
	if err := extractNpmPackageContext(ctx, tarball, extractDir); err != nil {
		return fmt.Errorf("extract npm package: %w", err)
	}

	pkg, err := ParsePiPackage(extractDir)
	if err != nil {
		return err
	}
	if pkg.Name != spec.Name {
		return fmt.Errorf("npm archive identity does not match requested package %q", spec.Name)
	}
	if err := validateNPMVersion(spec, pkg.Version); err != nil {
		return err
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
	if pkg.HasDeps && IsOfflineMode() {
		return fmt.Errorf("tau install --pi needs network; re-run online")
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

	if err := validatePackageParent(installBase, pkg.Name); err != nil {
		return err
	}
	installPath := filepath.Join(installBase, pkg.Name)
	if err := validateLocalReplacement(sourceDir, installPath); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(installPath), 0o700); err != nil {
		return fmt.Errorf("create packages dir: %w", err)
	}
	var loaded *Manifest
	err = stagePackageExpected(ctx, installPath, cfg.expected, func(staged string) error {
		if err := validatePackageParent(installBase, pkg.Name); err != nil {
			return err
		}
		if err := validateLocalReplacement(sourceDir, installPath); err != nil {
			return err
		}
		if !cfg.Force {
			if err := NewManager(m.userDir, m.projectDir).CheckDuplicate(pkg.Name, scope); err != nil {
				return err
			}
		}
		if err := copyDirContext(ctx, sourceDir, staged); err != nil {
			return fmt.Errorf("copy package: %w", err)
		}

		if pkg.HasDeps {
			if err := m.installPiDeps(ctx, staged, pkg.PeerDeps); err != nil {
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
		if err := os.WriteFile(filepath.Join(staged, "tau-package.toml"), manifestBytes, 0o600); err != nil {
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
		if err := sidecar.Write(staged); err != nil {
			return fmt.Errorf("write install sidecar: %w", err)
		}

		var loadErr error
		loaded, loadErr = Load(filepath.Join(staged, "tau-package.toml"))
		if loadErr != nil {
			return fmt.Errorf("load generated manifest: %w", loadErr)
		}
		if cfg.Link {
			return linkSourceEntries(ctx, sourceDir, staged, "package-lock.json", "npm-shrinkwrap.json", "node_modules", "tau-package.toml", ".tau-install.json")
		}
		return nil
	})
	if err != nil {
		return err
	}

	m.notifyReloaders(ctx, pkg.Name, loaded)
	return nil
}

// linkSourceEntries replaces each top-level entry of staged with a link into
// source, except the private entries, so install metadata and prepared
// dependencies never land in the source directory.
// ponytail: top-level entries are captured at install; reinstall to link newly added roots.
func linkSourceEntries(ctx context.Context, source, staged string, private ...string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return fmt.Errorf("read linked source: %w", err)
	}
	for _, entry := range entries {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		if slices.Contains(private, entry.Name()) {
			continue
		}
		path := filepath.Join(staged, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove staged source entry: %w", err)
		}
		if err := os.Symlink(filepath.Join(source, entry.Name()), path); err != nil {
			return fmt.Errorf("link source entry: %w", err)
		}
	}
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
	return extractNpmPackageContext(context.Background(), tarball, dst)
}

func extractNpmPackageContext(ctx context.Context, tarball, dst string) (err error) {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return fmt.Errorf("create npm package root: %w", err)
	}
	root, err := os.OpenRoot(dst)
	if err != nil {
		return fmt.Errorf("open npm package root: %w", err)
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	file, err := os.Open(tarball)
	if err != nil {
		return fmt.Errorf("open tarball: %w", err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat npm archive: %w", err)
	}
	if info.Size() > maxArchiveDownload {
		return errors.New("archive download size limit exceeded")
	}
	gz, err := gzip.NewReader(&archiveLimitReader{reader: file, remaining: maxArchiveDownload})
	if err != nil {
		return fmt.Errorf("read gzip: %w", err)
	}
	defer func() { err = errors.Join(err, gz.Close()) }()

	expanded := archiveContextReader{ctx: ctx, reader: &archiveLimitReader{reader: gz, remaining: maxArchiveExpanded + maxArchiveMetadata}}
	tr := tar.NewReader(expanded)
	var budget archiveBudget
	for {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		if err := budget.admit(hdr); err != nil {
			return err
		}
		rel := strings.TrimPrefix(filepath.Clean(filepath.FromSlash(hdr.Name)), "package"+string(os.PathSeparator))
		if !filepath.IsLocal(rel) {
			return fmt.Errorf("npm archive path escapes package: %q", hdr.Name)
		}
		if rel == "." {
			continue
		}
		path := rel
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(path, 0o700); err != nil {
				return fmt.Errorf("create tar dir: %w", err)
			}
		case tar.TypeReg:
			if err := root.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return fmt.Errorf("create tar file dir: %w", err)
			}
			out, err := root.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return fmt.Errorf("create tar file: %w", err)
			}
			if _, err := io.Copy(out, archiveContextReader{ctx: ctx, reader: tr}); err != nil {
				closeErr := out.Close()
				if closeErr != nil {
					return fmt.Errorf("copy tar file: %w", errors.Join(err, closeErr))
				}
				return fmt.Errorf("copy tar file: %w", err)
			}
			if err := out.Close(); err != nil {
				return fmt.Errorf("close tar file: %w", err)
			}
		}
	}
	if _, err := io.Copy(io.Discard, expanded); err != nil {
		return fmt.Errorf("validate archive trailer: %w", err)
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
