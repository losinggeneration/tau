package ignore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
		for i := len(ignoreFileNames) - 1; i >= 0; i-- {
			path := filepath.Join(current, ignoreFileNames[i])
			data, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("read ignore file %s: %w", path, err)
			}
			var parseErr error
			patterns := gitignore.NewParser(strings.NewReader(string(data)), func(err gitignore.Error) bool {
				parseErr = errors.Join(parseErr, err)
				return true
			}).Parse()
			if parseErr != nil {
				return nil, fmt.Errorf("parse ignore file %s: %w", path, parseErr)
			}
			matcher.layers = append(matcher.layers, ignoreLayer{base: current, patterns: patterns})
		}
		if current == matcher.root {
			return matcher, nil
		}
	}
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
