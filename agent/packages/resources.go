package packages

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/losinggeneration/tau/internal/ignore"
)

// ResourceKind represents the type of resource being collected.
type ResourceKind int

const (
	ResourceKindExtension ResourceKind = iota
	ResourceKindSkill
	ResourceKindPrompt
	ResourceKindTheme
)

// Resource represents a discovered resource.
type Resource struct {
	Path  string
	Kind  ResourceKind
	Valid bool
}

// collectExtensionResources collects extension resources from a directory.
// Extensions are .ts or .js files, with smart discovery from package.json
// or index.ts/index.js fallback.
// When isRoot is true (passed via parentIgnore != nil signal), the function
// will only return explicit entries (package.json pi.extensions or index.ts/index.js)
// and will NOT collect loose .ts/.js files from the root directory.
// This matches the TypeScript collectAutoExtensionEntries() behavior.
func collectExtensionResources(dir string, parentIgnore *ignore.Matcher) ([]Resource, error) {
	var resources []Resource

	pi, err := ReadPiManifest(dir)
	if err != nil {
		return nil, err
	}
	if pi != nil && len(pi.Extensions) > 0 {
		for _, extPath := range pi.Extensions {
			fullPath := extPath
			if !filepath.IsAbs(fullPath) {
				fullPath = filepath.Join(dir, fullPath)
			}
			info, err := os.Stat(fullPath)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("stat extension: %w", err)
			}
			if !info.IsDir() {
				resources = append(resources, Resource{Path: fullPath, Kind: ResourceKindExtension, Valid: true})
			}
		}
		return resources, nil
	}

	// Fall back to index.ts/index.js
	for _, name := range []string{"index.ts", "index.js"} {
		indexPath := filepath.Join(dir, name)
		info, err := os.Stat(indexPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stat extension index: %w", err)
		}
		if !info.IsDir() {
			resources = append(resources, Resource{
				Path:  indexPath,
				Kind:  ResourceKindExtension,
				Valid: true,
			})
			return resources, nil
		}
	}

	// If parentIgnore is non-nil, this was called on the package root.
	// In that case, don't collect loose .ts/.js files - only explicit entries.
	// This matches the TypeScript behavior where collectAutoExtensionEntries
	// returns early if resolveExtensionEntries finds explicit entries.
	if parentIgnore != nil {
		return resources, nil
	}

	// Smart discovery: collect .ts and .js files in directory
	ignore, err := ignore.New(dir, dir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(dir, name)
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			continue
		}
		isDir := entry.IsDir()
		isFile := entry.Type().IsRegular()
		if entry.Type()&os.ModeSymlink != 0 {
			info, err := os.Stat(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("stat extension entry: %w", err)
			}
			isDir = info.IsDir()
			isFile = info.Mode().IsRegular()
		}
		if ignore.Ignored(path, isDir) {
			continue
		}

		if isDir {
			// Recurse into subdirectories with ignore context
			subResources, err := collectExtensionResources(path, ignore)
			if err != nil {
				return nil, fmt.Errorf("collect extensions in %s: %w", path, err)
			}
			resources = append(resources, subResources...)
			continue
		}

		ext := filepath.Ext(name)
		if !isFile || (ext != ".ts" && ext != ".js") {
			continue
		}

		// Skip hidden files
		if strings.HasPrefix(name, ".") {
			continue
		}

		if ignore.Ignored(path, false) {
			continue
		}

		resources = append(resources, Resource{
			Path:  path,
			Kind:  ResourceKindExtension,
			Valid: true,
		})
	}

	return resources, nil
}

// collectSkillResources recursively collects SKILL.md files.
// This matches the TypeScript collectSkillEntries() behavior.
func collectSkillResources(dir string, rootDir string, skillMode string) ([]Resource, error) {
	var resources []Resource

	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}

	ignore, err := ignore.New(dir, rootDir)
	if err != nil {
		return nil, err
	}

	// Check for SKILL.md in this directory first (directory mode)
	skillMDPath := filepath.Join(dir, "SKILL.md")
	skillInfo, err := os.Stat(skillMDPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat skill entry: %w", err)
	}
	if err == nil && !skillInfo.IsDir() {
		if !ignore.Ignored(skillMDPath, false) {
			resources = append(resources, Resource{
				Path:  skillMDPath,
				Kind:  ResourceKindSkill,
				Valid: true,
			})
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		name := entry.Name()

		// Skip hidden files and node_modules
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			continue
		}

		path := filepath.Join(dir, name)

		if entry.IsDir() {
			if ignore.Ignored(path, true) {
				continue
			}

			// Recurse into subdirectories
			subResources, err := collectSkillResources(path, rootDir, skillMode)
			if err != nil {
				return nil, fmt.Errorf("collect skills in %s: %w", path, err)
			}
			resources = append(resources, subResources...)
		} else {
			// Check for loose .md files (not SKILL.md)
			if strings.HasSuffix(name, ".md") && name != "SKILL.md" {
				if ignore.Ignored(path, false) {
					continue
				}
				resources = append(resources, Resource{
					Path:  path,
					Kind:  ResourceKindSkill,
					Valid: true,
				})
			}
		}
	}

	return resources, nil
}

// collectPromptResources collects .md files in a directory.
// This matches the TypeScript collectAutoPromptEntries() behavior.
func collectPromptResources(dir string) ([]Resource, error) {
	var resources []Resource

	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}

	ignore, err := ignore.New(dir, dir)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		name := entry.Name()

		// Skip hidden files and node_modules
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			continue
		}

		path := filepath.Join(dir, name)

		if entry.IsDir() {
			// Don't recurse into subdirectories for prompts
			continue
		}

		// Only collect .md files
		if strings.HasSuffix(name, ".md") {
			if ignore.Ignored(path, false) {
				continue
			}
			resources = append(resources, Resource{
				Path:  path,
				Kind:  ResourceKindPrompt,
				Valid: true,
			})
		}
	}

	return resources, nil
}

// collectThemeResources collects .json files in a directory.
// This matches the TypeScript collectAutoThemeEntries() behavior.
func collectThemeResources(dir string) ([]Resource, error) {
	var resources []Resource

	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}

	ignore, err := ignore.New(dir, dir)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		name := entry.Name()

		// Skip hidden files and node_modules
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			continue
		}

		path := filepath.Join(dir, name)

		if entry.IsDir() {
			// Don't recurse into subdirectories for themes
			continue
		}

		// Only collect .json files
		if strings.HasSuffix(name, ".json") {
			if ignore.Ignored(path, false) {
				continue
			}
			resources = append(resources, Resource{
				Path:  path,
				Kind:  ResourceKindTheme,
				Valid: true,
			})
		}
	}

	return resources, nil
}

// CollectPackageResources collects all resources from a package directory.
// It respects ignore files and uses manifest entries if available.
func CollectPackageResources(pkgRoot string, manifest *Manifest) ([]Resource, error) {
	var resources []Resource

	extResources, err := collectExtensionResources(filepath.Join(pkgRoot, "extensions"), nil)
	if os.IsNotExist(err) {
		extResources, err = collectExtensionResources(pkgRoot, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("collect package extensions: %w", err)
	}
	resources = append(resources, extResources...)

	// Collect skills
	if manifest != nil && len(manifest.AllSkills()) > 0 {
		for _, skill := range manifest.AllSkills() {
			skillPath := filepath.Join(pkgRoot, skill.Path)
			info, err := os.Stat(skillPath)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("stat package skill: %w", err)
			}
			if info.IsDir() {
				skillResources, err := collectSkillResources(skillPath, pkgRoot, "pi")
				if err != nil {
					return nil, fmt.Errorf("collect package skills: %w", err)
				}
				resources = append(resources, skillResources...)
			} else {
				// Single file
				resources = append(resources, Resource{
					Path:  skillPath,
					Kind:  ResourceKindSkill,
					Valid: true,
				})
			}
		}
	} else {
		// Convention: skills/ directory
		skillsDir := filepath.Join(pkgRoot, "skills")
		skillResources, err := collectSkillResources(skillsDir, pkgRoot, "pi")
		if err != nil {
			return nil, fmt.Errorf("collect package skills: %w", err)
		}
		resources = append(resources, skillResources...)
	}

	// Collect prompts
	if manifest != nil && len(manifest.AllPrompts()) > 0 {
		for _, prompt := range manifest.AllPrompts() {
			promptPath := filepath.Join(pkgRoot, prompt.Path)
			info, err := os.Stat(promptPath)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("stat package prompt: %w", err)
			}
			if info.IsDir() {
				promptResources, err := collectPromptResources(promptPath)
				if err != nil {
					return nil, fmt.Errorf("collect package prompts: %w", err)
				}
				resources = append(resources, promptResources...)
			} else {
				// Single file
				resources = append(resources, Resource{
					Path:  promptPath,
					Kind:  ResourceKindPrompt,
					Valid: true,
				})
			}
		}
	} else {
		// Convention: prompts/ directory
		promptsDir := filepath.Join(pkgRoot, "prompts")
		promptResources, err := collectPromptResources(promptsDir)
		if err != nil {
			return nil, fmt.Errorf("collect package prompts: %w", err)
		}
		resources = append(resources, promptResources...)
	}

	// Collect themes
	if manifest != nil && len(manifest.AllThemes()) > 0 {
		for _, theme := range manifest.AllThemes() {
			themePath := filepath.Join(pkgRoot, theme.Path)
			info, err := os.Stat(themePath)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("stat package theme: %w", err)
			}
			if info.IsDir() {
				themeResources, err := collectThemeResources(themePath)
				if err != nil {
					return nil, fmt.Errorf("collect package themes: %w", err)
				}
				resources = append(resources, themeResources...)
			} else {
				// Single file
				resources = append(resources, Resource{
					Path:  themePath,
					Kind:  ResourceKindTheme,
					Valid: true,
				})
			}
		}
	} else {
		// Convention: themes/ directory
		themesDir := filepath.Join(pkgRoot, "themes")
		themeResources, err := collectThemeResources(themesDir)
		if err != nil {
			return nil, fmt.Errorf("collect package themes: %w", err)
		}
		resources = append(resources, themeResources...)
	}

	return resources, nil
}
