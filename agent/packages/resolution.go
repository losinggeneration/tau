package packages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/paths"
)

// ResolvedPackage selects one package root and its resource policy. Delta is
// an optional project autoload-disabled overlay over the user policy.
type ResolvedPackage struct {
	Root   string
	Source ai.PackageSource
	Delta  *ai.PackageSource
}

func packageIdentity(source, base string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", errors.New("empty package source")
	}
	if spec, ok := strings.CutPrefix(source, "npm:"); ok {
		name := parseNpmSpec(spec).Name
		if err := validatePackageName(name); err != nil {
			return "", err
		}

		return "npm:" + name, nil
	}
	gitSource, explicit := strings.CutPrefix(source, "git:")
	if !explicit {
		gitSource = source
	}
	if explicit || isGitURL(source) || strings.HasPrefix(source, "ssh://") {
		gitSource, _ = parseSourceRef(gitSource)
		if strings.HasPrefix(gitSource, "git@") && !strings.Contains(gitSource, "://") {
			hostPath := strings.TrimPrefix(gitSource, "git@")
			host, path, ok := strings.Cut(hostPath, ":")
			if !ok {
				return "", fmt.Errorf("invalid git source %q", source)
			}
			gitSource = "ssh://git@" + host + "/" + path
		} else {
			gitSource = normalizeGitURL(gitSource)
		}
		u, err := url.Parse(gitSource)
		if err != nil {
			return "", fmt.Errorf("parse git source: %w", err)
		}
		path := u.Path
		path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
		if (u.Host == "" && u.Scheme != "file") || path == "" || path == "/" {
			return "", fmt.Errorf("invalid git source %q", source)
		}
		for _, part := range strings.Split(path, "/") {
			if part == ".." || part == "." {
				return "", fmt.Errorf("invalid git repository path %q", path)
			}
		}
		return "git:" + strings.ToLower(u.Host) + "/" + strings.TrimPrefix(path, "/"), nil
	}
	resolved, err := filepath.Abs(paths.ResourcePath(source, base, ""))
	if err != nil {
		return "", fmt.Errorf("resolve local package: %w", err)
	}
	return "local:" + resolved, nil
}

// InstalledRoots lists direct packages, including scoped packages and links.
// It does not follow arbitrary nested directories.
func InstalledRoots(base string) ([]string, error) {
	if base == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read packages directory: %w", err)
	}
	entries, err = recoverInstalledEntries(base, entries)
	if err != nil {
		return nil, err
	}
	var roots []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tau-install-") {
			continue
		}
		path := filepath.Join(base, entry.Name())
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stat installed package: %w", err)
		}
		if !info.IsDir() {
			continue
		}
		if strings.HasPrefix(entry.Name(), "@") {
			scoped, err := os.ReadDir(path)
			if err != nil {
				return nil, fmt.Errorf("read scoped packages: %w", err)
			}
			scoped, err = recoverInstalledEntries(path, scoped)
			if err != nil {
				return nil, err
			}
			for _, child := range scoped {
				if strings.HasPrefix(child.Name(), ".tau-install-") {
					continue
				}
				childPath := filepath.Join(path, child.Name())
				info, err := os.Stat(childPath)
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					return nil, fmt.Errorf("stat installed package: %w", err)
				}
				if info.IsDir() {
					roots = append(roots, childPath)
				}
			}
		} else {
			roots = append(roots, path)
		}
	}

	return roots, nil
}

func recoverInstalledEntries(base string, entries []os.DirEntry) ([]os.DirEntry, error) {
	recovered := false
	for _, entry := range entries {
		name, ok := strings.CutPrefix(entry.Name(), ".tau-install-recovery-")
		if !ok {
			continue
		}
		if err := validatePackageName(name); err != nil {
			return nil, fmt.Errorf("invalid recovery identity: %w", err)
		}
		// Discovery is synchronous; bound its wait on an active package writer.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		unlock, err := lockPackageWriter(ctx, filepath.Join(base, name))
		cancel()
		if err != nil {
			return nil, err
		}
		if err := unlock(); err != nil {
			return nil, err
		}
		recovered = true
	}
	if recovered {
		entries, err := os.ReadDir(base)
		if err != nil {
			return nil, fmt.Errorf("read recovered packages: %w", err)
		}
		return entries, nil
	}
	return entries, nil
}

// ResolvePackageSources resolves configured local sources and already installed
// npm/git sources. It never installs; see InstallManager.InstallMissingSources.
func ResolvePackageSources(settings *ai.Settings, userDir, projectDir string) ([]ResolvedPackage, error) {
	resolved, _, err := resolvePackages(settings, userDir, projectDir)
	return resolved, err
}

// MissingSourceAction is a caller's decision for a configured source that
// isn't installed.
type MissingSourceAction int

const (
	MissingSourceInstall MissingSourceAction = iota
	MissingSourceSkip
	MissingSourceError
)

// InstallMissingSources installs configured npm and git sources that aren't
// installed, each into the scope that configures it, and returns the installed
// sources. The settings entry is the approval, so installs don't prompt. decide
// picks an action per source and nil installs them all. Offline mode installs
// nothing.
func (m *InstallManager) InstallMissingSources(ctx context.Context, settings *ai.Settings, decide func(context.Context, string) (MissingSourceAction, error)) ([]string, error) {
	if IsOfflineMode() {
		return nil, nil
	}
	_, missing, err := resolvePackages(settings, m.userDir, m.projectDir)
	if err != nil {
		return nil, err
	}
	var installed []string
	for _, source := range missing {
		action := MissingSourceInstall
		if decide != nil {
			if action, err = decide(ctx, source.source); err != nil {
				return installed, err
			}
		}
		switch action {
		case MissingSourceSkip:
			continue
		case MissingSourceError:
			return installed, fmt.Errorf("missing source: %s", source.source)
		}
		if err := m.Install(ctx, Config{Source: source.source, Project: source.project, Yes: true, Force: source.stale}); err != nil {
			return installed, fmt.Errorf("install %s: %w", source.source, err)
		}
		installed = append(installed, source.source)
	}
	return installed, nil
}

// missingSource is a configured npm or git source with no installed root, or
// an installed npm package outside its configured version range (stale).
type missingSource struct {
	source  string
	project bool
	stale   bool
}

// npmVersionMatches reports whether the package installed at root satisfies
// the version in an npm source. A version that isn't a range, such as a
// dist-tag, always matches; an unreadable installed version never does.
func npmVersionMatches(source, root string) bool {
	version := parseNpmSpec(strings.TrimPrefix(strings.TrimSpace(source), "npm:")).Version
	if version == "" {
		return true
	}
	constraint, err := semver.NewConstraint(version)
	if err != nil {
		return true
	}
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return false
	}
	var manifest struct{ Version string }
	if json.Unmarshal(data, &manifest) != nil {
		return false
	}
	installed, err := semver.NewVersion(manifest.Version)
	return err == nil && constraint.Check(installed)
}

func resolvePackages(settings *ai.Settings, userDir, projectDir string) ([]ResolvedPackage, []missingSource, error) {
	if !settings.ProjectResourcesTrusted() {
		projectDir = ""
	}
	for _, dir := range []*string{&userDir, &projectDir} {
		if *dir == "" {
			continue
		}
		absolute, err := filepath.Abs(paths.ResourcePath(*dir, "", ""))
		if err != nil {
			return nil, nil, fmt.Errorf("resolve packages directory: %w", err)
		}
		*dir = absolute
	}

	type installed struct {
		root, identity string
		scope          int
	}
	var installs []installed
	for scope, base := range []string{projectDir, userDir} {
		roots, err := InstalledRoots(base)
		if err != nil {
			return nil, nil, err
		}
		for _, root := range roots {
			sidecar, err := LoadSidecar(root)
			var identity string
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					return nil, nil, err
				}
				name, relErr := filepath.Rel(base, root)
				if relErr != nil {
					return nil, nil, fmt.Errorf("resolve installed name: %w", relErr)
				}
				identity, err = packageIdentity("npm:"+filepath.ToSlash(name), base)
				if err != nil {
					return nil, nil, err
				}
			} else {
				identity, err = packageIdentity(sidecar.Source, base)
				if err != nil {
					return nil, nil, fmt.Errorf("installed package %s: %w", root, err)
				}
			}
			installs = append(installs, installed{root, identity, scope})
		}
	}
	user, project := settings.PackageSourcesByScope()
	if user.BaseDir == "" {
		user.BaseDir = filepath.Dir(userDir)
	}
	if project.BaseDir == "" {
		project.BaseDir = filepath.Dir(projectDir)
	}
	type configured struct {
		source   ai.PackageSource
		identity string
		scope    int
	}
	var entries []configured
	for scope, group := range []ai.PackageScope{project, user} {
		for _, source := range group.Sources {
			identity, err := packageIdentity(source.Source, group.BaseDir)
			if err != nil {
				return nil, nil, err
			}
			entries = append(entries, configured{source, identity, scope})
		}
	}
	claimed := make(map[string]bool)
	seenRoots := make(map[string]bool)
	result := make([]ResolvedPackage, 0)
	var missing []missingSource
	for _, entry := range entries {
		if claimed[entry.identity] {
			continue
		}
		claimed[entry.identity] = true
		source, scope := entry.source, entry.scope
		var delta *ai.PackageSource
		if scope == 0 && source.Autoload != nil && !*source.Autoload {
			for _, other := range entries {
				if other.scope == 1 && other.identity == entry.identity {
					delta = new(source)
					source, scope = other.source, other.scope
					break
				}
			}
		}
		var root string
		if local, ok := strings.CutPrefix(entry.identity, "local:"); ok {
			root = local
		} else {
			// A project entry may name a package that is only installed in the
			// user scope: the project's policy applies to the package it
			// configures. A user entry never falls back to a project install.
			scopes := []int{scope}
			if scope == 0 {
				scopes = append(scopes, 1)
			}
			for _, candidate := range scopes {
				for _, install := range installs {
					if install.scope == candidate && install.identity == entry.identity {
						root = install.root
						break
					}
				}
				if root != "" {
					break
				}
			}
			if root == "" && strings.HasPrefix(entry.identity, "npm:") {
				base := projectDir
				if scope == 1 {
					base = userDir
				}
				if base != "" {
					root = filepath.Join(base, strings.TrimPrefix(entry.identity, "npm:"))
				}
			}
		}
		if root != "" {
			canonical, err := filepath.EvalSymlinks(root)
			if err == nil {
				if strings.HasPrefix(entry.identity, "npm:") && !npmVersionMatches(source.Source, canonical) {
					missing = append(missing, missingSource{source.Source, scope == 0, true})
				}
				if !seenRoots[canonical] {
					seenRoots[canonical] = true
					result = append(result, ResolvedPackage{canonical, source, delta})
				}
				continue
			}
			if !errors.Is(err, os.ErrNotExist) {
				return nil, nil, fmt.Errorf("resolve package root: %w", err)
			}
		}
		if !strings.HasPrefix(entry.identity, "local:") && (scope == 1 || projectDir != "") {
			missing = append(missing, missingSource{source: source.Source, project: scope == 0})
		}
	}
	for _, install := range installs {
		if install.identity != "" && claimed[install.identity] {
			continue
		}
		canonical, err := filepath.EvalSymlinks(install.root)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve installed package: %w", err)
		}
		if seenRoots[canonical] {
			continue
		}
		seenRoots[canonical] = true
		if install.identity != "" {
			claimed[install.identity] = true
		}
		result = append(result, ResolvedPackage{Root: canonical})
	}
	return result, missing, nil
}

// CloneResolvedPackages isolates resource policies staged for a reload.
func CloneResolvedPackages(input []ResolvedPackage) []ResolvedPackage {
	result := slices.Clone(input)
	for i := range result {
		result[i].Source = result[i].Source.Clone()
		if result[i].Delta != nil {
			result[i].Delta = new(result[i].Delta.Clone())
		}
	}
	return result
}
