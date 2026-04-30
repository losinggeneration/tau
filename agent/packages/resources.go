package packages

import (
	"os"
	"path/filepath"
	"strings"
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
func collectExtensionResources(dir string, parentIgnore *ignoreMatcher) ([]Resource, error) {
	var resources []Resource

	// Check for package.json with pi.extensions field
	packageJSONPath := filepath.Join(dir, "package.json")
	if info, err := os.Stat(packageJSONPath); err == nil && !info.IsDir() {
		if extensions := readPiExtensions(packageJSONPath); len(extensions) > 0 {
			// Use explicit extension entries from package.json
			for _, extPath := range extensions {
				fullPath := filepath.Join(dir, extPath)
				if info, err := os.Stat(fullPath); err == nil && !info.IsDir() {
					resources = append(resources, Resource{
						Path:  fullPath,
						Kind:  ResourceKindExtension,
						Valid: true,
					})
				}
			}
			return resources, nil
		}
	}

	// Fall back to index.ts/index.js
	for _, name := range []string{"index.ts", "index.js"} {
		indexPath := filepath.Join(dir, name)
		if _, err := os.Stat(indexPath); err == nil {
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
	ignore := newIgnoreMatcher(dir, dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(dir, name)

		if entry.IsDir() {
			// Recurse into subdirectories with ignore context
			subResources, _ := collectExtensionResources(path, ignore)
			resources = append(resources, subResources...)
			continue
		}

		ext := filepath.Ext(name)
		if ext != ".ts" && ext != ".js" {
			continue
		}

		// Skip hidden files
		if strings.HasPrefix(name, ".") {
			continue
		}

		if isIgnored(ignore, path, false) {
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

// readPiExtensions reads the pi.extensions field from a package.json.
func readPiExtensions(packageJSONPath string) []string {
	data, err := os.ReadFile(packageJSONPath)
	if err != nil {
		return nil
	}

	// Simple JSON parsing to extract pi.extensions
	// This avoids importing encoding/json for this simple use case
	content := string(data)

	// Look for "pi": { ... "extensions": [...] }
	piIdx := strings.Index(content, `"pi"`)
	if piIdx == -1 {
		return nil
	}

	extensionsIdx := strings.Index(content[piIdx:], `"extensions"`)
	if extensionsIdx == -1 {
		return nil
	}

	// Find the array start [
	arrayStart := strings.Index(content[piIdx+extensionsIdx:], "[")
	if arrayStart == -1 {
		return nil
	}

	// Extract array content (simplified parsing)
	start := piIdx + extensionsIdx + arrayStart + 1
	depth := 1
	end := start
	for i := start; i < len(content) && depth > 0; i++ {
		switch content[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				end = i
			}
		}
	}

	arrayContent := strings.TrimSpace(content[start:end])
	if arrayContent == "" {
		return nil
	}

	// Parse array elements
	var extensions []string
	elements := strings.Split(arrayContent, ",")
	for _, elem := range elements {
		elem = strings.TrimSpace(elem)
		if strings.HasPrefix(elem, `"`) && strings.HasSuffix(elem, `"`) {
			ext := elem[1 : len(elem)-1]
			extensions = append(extensions, ext)
		}
	}

	return extensions
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

	ignore := newIgnoreMatcher(dir, rootDir)

	// Check for SKILL.md in this directory first (directory mode)
	skillMDPath := filepath.Join(dir, "SKILL.md")
	if _, err := os.Stat(skillMDPath); err == nil {
		if !isIgnored(ignore, skillMDPath, false) {
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
			if isIgnored(ignore, path, true) {
				continue
			}

			// Recurse into subdirectories
			subResources, _ := collectSkillResources(path, rootDir, skillMode)
			resources = append(resources, subResources...)
		} else {
			// Check for loose .md files (not SKILL.md)
			if strings.HasSuffix(name, ".md") && name != "SKILL.md" {
				if isIgnored(ignore, path, false) {
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

	ignore := newIgnoreMatcher(dir, dir)

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
			if isIgnored(ignore, path, false) {
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

	ignore := newIgnoreMatcher(dir, dir)

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
			if isIgnored(ignore, path, false) {
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

	// Collect extensions with smart discovery (package.json pi.extensions or index.ts/index.js fallback)
	extensionsDir := filepath.Join(pkgRoot, "extensions")
	if extResources, err := collectExtensionResources(extensionsDir, nil); err == nil {
		resources = append(resources, extResources...)
	} else {
		// If extensions directory doesn't exist, check for package.json in root for extension entries
		if extResources, err := collectExtensionResources(pkgRoot, nil); err == nil && len(extResources) > 0 {
			resources = append(resources, extResources...)
		}
	}

	// Collect skills
	if manifest != nil && len(manifest.AllSkills()) > 0 {
		for _, skill := range manifest.AllSkills() {
			skillPath := filepath.Join(pkgRoot, skill.Path)
			info, err := os.Stat(skillPath)
			if err == nil && info.IsDir() {
				if skillResources, err := collectSkillResources(skillPath, pkgRoot, "pi"); err == nil {
					resources = append(resources, skillResources...)
				}
			} else if err == nil {
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
		if skillResources, err := collectSkillResources(skillsDir, pkgRoot, "pi"); err == nil {
			resources = append(resources, skillResources...)
		}
	}

	// Collect prompts
	if manifest != nil && len(manifest.AllPrompts()) > 0 {
		for _, prompt := range manifest.AllPrompts() {
			promptPath := filepath.Join(pkgRoot, prompt.Path)
			info, err := os.Stat(promptPath)
			if err == nil && info.IsDir() {
				if promptResources, err := collectPromptResources(promptPath); err == nil {
					resources = append(resources, promptResources...)
				}
			} else if err == nil {
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
		if promptResources, err := collectPromptResources(promptsDir); err == nil {
			resources = append(resources, promptResources...)
		}
	}

	// Collect themes
	if manifest != nil && len(manifest.AllThemes()) > 0 {
		for _, theme := range manifest.AllThemes() {
			themePath := filepath.Join(pkgRoot, theme.Path)
			info, err := os.Stat(themePath)
			if err == nil && info.IsDir() {
				if themeResources, err := collectThemeResources(themePath); err == nil {
					resources = append(resources, themeResources...)
				}
			} else if err == nil {
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
		if themeResources, err := collectThemeResources(themesDir); err == nil {
			resources = append(resources, themeResources...)
		}
	}

	return resources, nil
}
