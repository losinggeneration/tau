package packages

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

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
		parts := strings.Split(name, "/")
		if len(parts) > 2 || (len(parts) == 2 && !strings.HasPrefix(parts[0], "@")) {
			return "", fmt.Errorf("invalid npm package name %q", name)
		}
		for _, part := range parts {
			part = strings.TrimPrefix(part, "@")
			if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `\:@ `) {
				return "", fmt.Errorf("invalid npm package name %q", name)
			}
		}
		if strings.HasPrefix(name, "@") && len(parts) != 2 {
			return "", fmt.Errorf("invalid npm package name %q", name)
		}
		return "npm:" + name, nil
	}
	gitSource, explicit := strings.CutPrefix(source, "git:")
	if !explicit {
		gitSource = source
	}
	if explicit || isGitURL(source) || strings.HasPrefix(source, "ssh://") {
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
		if index := strings.LastIndex(path, "@"); index >= 0 {
			path = path[:index]
		}
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
	var roots []string
	for _, entry := range entries {
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
			for _, child := range scoped {
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

// ResolvePackageSources resolves configured local sources and already installed
// npm/git sources. Discovery never installs or executes an unapproved package.
func ResolvePackageSources(settings *ai.Settings, userDir, projectDir string) ([]ResolvedPackage, error) {
	for _, dir := range []*string{&userDir, &projectDir} {
		if *dir == "" {
			continue
		}
		absolute, err := filepath.Abs(paths.ResourcePath(*dir, "", ""))
		if err != nil {
			return nil, fmt.Errorf("resolve packages directory: %w", err)
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
			return nil, err
		}
		for _, root := range roots {
			sidecar, err := LoadSidecar(root)
			var identity string
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					return nil, err
				}
				name, relErr := filepath.Rel(base, root)
				if relErr != nil {
					return nil, fmt.Errorf("resolve installed name: %w", relErr)
				}
				identity, err = packageIdentity("npm:"+filepath.ToSlash(name), base)
				if err != nil {
					return nil, err
				}
			} else {
				identity, err = packageIdentity(sidecar.Source, base)
				if err != nil {
					return nil, fmt.Errorf("installed package %s: %w", root, err)
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
				return nil, err
			}
			entries = append(entries, configured{source, identity, scope})
		}
	}
	claimed := make(map[string]bool)
	seenRoots := make(map[string]bool)
	result := make([]ResolvedPackage, 0)
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
		if root == "" {
			continue
		}
		canonical, err := filepath.EvalSymlinks(root)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("resolve package root: %w", err)
		}
		if !seenRoots[canonical] {
			seenRoots[canonical] = true
			result = append(result, ResolvedPackage{canonical, source, delta})
		}
	}
	for _, install := range installs {
		if install.identity != "" && claimed[install.identity] {
			continue
		}
		canonical, err := filepath.EvalSymlinks(install.root)
		if err != nil {
			return nil, fmt.Errorf("resolve installed package: %w", err)
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
	return result, nil
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
