package ignore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/denormal/go-gitignore"
)

var ignoreFileNames = []string{".gitignore", ".ignore", ".fdignore"}

// Matcher applies ordered ignore files within a root directory.
type Matcher struct {
	root   string
	layers []ignoreLayer
}

type ignoreLayer struct {
	base     string
	patterns []gitignore.Pattern
}

// New loads the closest rules first, with later ignore files
// taking precedence within each directory.
func New(dir, rootDir string) (*Matcher, error) {
	matcher := &Matcher{root: filepath.Clean(rootDir)}
	for current := filepath.Clean(dir); ; current = filepath.Dir(current) {
		if !pathWithin(matcher.root, current) {
			return nil, fmt.Errorf("ignore directory %s is outside root %s", dir, rootDir)
		}
		layers, err := loadLayers(current)
		if err != nil {
			return nil, err
		}
		matcher.layers = append(matcher.layers, layers...)
		if current == matcher.root {
			return matcher, nil
		}
	}
}

// Child returns the rules for dir, a subdirectory of the directory m was
// built for, reading only dir's own ignore files. m is left unchanged, so a
// walk can build each directory's rules from its parent's.
func (m *Matcher) Child(dir string) (*Matcher, error) {
	layers, err := loadLayers(filepath.Clean(dir))
	if err != nil {
		return nil, err
	}
	if m == nil {
		return &Matcher{root: filepath.Clean(dir), layers: layers}, nil
	}
	return &Matcher{root: m.root, layers: slices.Concat(layers, m.layers)}, nil
}

// loadLayers reads one directory's ignore files in precedence order. A
// repository's .git/info/exclude comes last, below its ignore files, as in git.
func loadLayers(dir string) ([]ignoreLayer, error) {
	var layers []ignoreLayer
	add := func(path string) error {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read ignore file %s: %w", path, err)
		}
		var parseErr error
		patterns := gitignore.NewParser(strings.NewReader(string(data)), func(err gitignore.Error) bool {
			parseErr = errors.Join(parseErr, err)
			return true
		}).Parse()
		if parseErr != nil {
			return fmt.Errorf("parse ignore file %s: %w", path, parseErr)
		}
		layers = append(layers, ignoreLayer{base: dir, patterns: patterns})
		return nil
	}
	for i := len(ignoreFileNames) - 1; i >= 0; i-- {
		if err := add(filepath.Join(dir, ignoreFileNames[i])); err != nil {
			return nil, err
		}
	}
	// A .git file (worktree or submodule) points elsewhere; only a .git
	// directory holds info/exclude.
	if info, err := os.Lstat(filepath.Join(dir, ".git")); err == nil && info.IsDir() {
		if err := add(filepath.Join(dir, ".git", "info", "exclude")); err != nil {
			return nil, err
		}
	}
	return layers, nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Ignored keeps each ignore file's patterns relative to its own directory.
func (m *Matcher) Ignored(fullPath string, isDir bool) bool {
	if m == nil || !pathWithin(m.root, fullPath) {
		return false
	}
	for _, layer := range m.layers {
		if !pathWithin(layer.base, fullPath) {
			continue
		}
		rel, err := filepath.Rel(layer.base, fullPath)
		if err != nil {
			return false
		}
		for i := len(layer.patterns) - 1; i >= 0; i-- {
			pattern := layer.patterns[i]
			text := strings.TrimPrefix(pattern.String(), "!")
			// The dependency's anchored name matcher lets wildcards cross slashes.
			if strings.HasPrefix(text, "/") && !strings.Contains(strings.Trim(text, "/"), "/") && strings.Contains(filepath.ToSlash(rel), "/") {
				continue
			}
			if pattern.Match(filepath.ToSlash(rel), isDir) {
				return pattern.Ignore()
			}
		}
	}
	return false
}
