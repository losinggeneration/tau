package packages

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// PiManifest declares package.json resources. Nil and empty lists remain distinct.
type PiManifest struct {
	Extensions []string `json:"extensions"`
	Skills     []string `json:"skills"`
	Prompts    []string `json:"prompts"`
	Themes     []string `json:"themes"`
}

// ReadPiManifest reads resource declarations without requiring install metadata.
// A missing package.json or pi object returns nil.
func ReadPiManifest(root string) (*PiManifest, error) {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Pi manifest: %w", err)
	}
	var pkg struct {
		Pi *PiManifest `json:"pi"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &pkg); err != nil {
		return nil, fmt.Errorf("parse Pi manifest: %w", err)
	}
	return pkg.Pi, nil
}

// PiResourcePaths expands declared sources and applies manifest overrides.
// Globs discover visible paths only; exact paths can address hidden or linked trees.
func PiResourcePaths(root string, entries []string, kind ResourceKind) ([]string, error) {
	var candidates, overrides []string
	for _, entry := range entries {
		if strings.HasPrefix(entry, "!") || strings.HasPrefix(entry, "+") || strings.HasPrefix(entry, "-") {
			overrides = append(overrides, entry)
			continue
		}
		var sources []string
		if strings.ContainsAny(entry, "*?") {
			pattern := filepath.ToSlash(filepath.Clean(entry))
			if err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if p == root {
					return nil
				}
				if strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				rel, err := filepath.Rel(root, p)
				if err != nil {
					return err
				}
				if piGlobMatch(filepath.ToSlash(rel), pattern) {
					sources = append(sources, p)
				}
				return nil
			}); err != nil {
				return nil, fmt.Errorf("expand Pi resource %q: %w", entry, err)
			}
		} else {
			source := entry
			if !filepath.IsAbs(source) {
				source = filepath.Join(root, source)
			}
			sources = append(sources, source)
		}
		for _, source := range sources {
			info, err := os.Stat(source)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("stat Pi resource: %w", err)
			}
			if !info.IsDir() {
				candidates = append(candidates, source)
				continue
			}
			var resources []Resource
			switch kind {
			case ResourceKindExtension:
				resources, err = collectExtensionResources(source, nil)
			case ResourceKindSkill:
				resources, err = collectSkillResources(source, root, "pi")
			case ResourceKindPrompt:
				resources, err = collectPromptResources(source)
			case ResourceKindTheme:
				resources, err = collectThemeResources(source)
			}
			if err != nil {
				return nil, fmt.Errorf("collect Pi resources: %w", err)
			}
			for _, resource := range resources {
				candidates = append(candidates, resource.Path)
			}
		}
	}
	var selected []string
	for _, candidate := range candidates {
		rel, err := filepath.Rel(root, candidate)
		if err != nil {
			return nil, fmt.Errorf("relative Pi resource: %w", err)
		}
		rel = filepath.ToSlash(rel)
		enabled := true
		for _, override := range overrides {
			pattern := filepath.ToSlash(filepath.Clean(override[1:]))
			match := rel == pattern || filepath.ToSlash(candidate) == pattern
			if override[0] == '!' {
				match = piGlobMatch(rel, pattern) || piGlobMatch(filepath.Base(candidate), pattern)
			}
			if kind == ResourceKindSkill && filepath.Base(candidate) == "SKILL.md" {
				parent := path.Dir(rel)
				match = match || parent == pattern || (override[0] == '!' && piGlobMatch(parent, pattern))
			}
			if match {
				enabled = override[0] == '+'
			}
		}
		if enabled && !slices.Contains(selected, candidate) {
			selected = append(selected, candidate)
		}
	}
	return selected, nil
}

func piGlobMatch(name, pattern string) bool {
	parts := strings.SplitN(pattern, "/", 2)
	if parts[0] == "**" {
		if len(parts) == 1 {
			return true
		}
		if piGlobMatch(name, parts[1]) {
			return true
		}
		if i := strings.IndexByte(name, '/'); i >= 0 {
			return piGlobMatch(name[i+1:], pattern)
		}
		return false
	}
	names := strings.SplitN(name, "/", 2)
	matched, err := path.Match(parts[0], names[0])
	if err != nil || !matched {
		return false
	}
	if len(parts) == 1 || len(names) == 1 {
		return len(parts) == len(names)
	}
	return piGlobMatch(names[1], parts[1])
}
