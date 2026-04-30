package packages

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/denormal/go-gitignore"
)

// ignoreFileNames lists the supported ignore file names.
// These match the TypeScript implementation's IGNORE_FILE_NAMES.
var ignoreFileNames = []string{".gitignore", ".ignore", ".fdignore"}

// ignoreMatcher wraps gitignore patterns for matching paths.
type ignoreMatcher struct {
	gitignore.GitIgnore
	root string
}

// newIgnoreMatcher creates a matcher for a directory tree, loading ignore files
// from the directory and parent directories up to rootDir. This matches the
// TypeScript implementation's addIgnoreRules() behavior.
func newIgnoreMatcher(dir, rootDir string) *ignoreMatcher {
	var allPatterns []string

	// Collect patterns from all directories from dir up to rootDir
	collectDir := dir
	for {
		for _, filename := range ignoreFileNames {
			ignorePath := filepath.Join(collectDir, filename)
			data, err := os.ReadFile(ignorePath)
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(data), "\n") {
				trimmed := strings.TrimSpace(line)
				// Skip empty lines and comments (lines starting with #)
				// But not escaped # (\#)
				if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
					// Handle escaped leading ! or #
					unescaped := unescapeIgnorePattern(trimmed)
					allPatterns = append(allPatterns, unescaped)
				}
			}
		}

		// Move to parent directory, stop at root or filesystem boundary
		parent := filepath.Dir(collectDir)
		if parent == collectDir {
			break // filesystem root
		}
		if collectDir == rootDir {
			break // reached the specified root
		}
		if !strings.HasPrefix(parent, rootDir) {
			break // parent is outside rootDir
		}
		collectDir = parent
	}

	matcher := &combinedIgnoreMatcher{
		patterns: allPatterns,
		root:     rootDir,
	}

	return &ignoreMatcher{
		GitIgnore: matcher,
		root:      rootDir,
	}
}

// unescapeIgnorePattern handles escaped leading characters in ignore patterns.
// In ignore files, \! means a literal !, and \# means a literal #.
func unescapeIgnorePattern(pattern string) string {
	// Handle escaped leading ! or #
	if strings.HasPrefix(pattern, "\\!") {
		return pattern[1:] // \" becomes "
	}
	if strings.HasPrefix(pattern, "\\#") {
		return pattern[1:] // \# becomes #
	}
	return pattern
}

// combinedIgnoreMatcher implements gitignore.GitIgnore from collected patterns.
type combinedIgnoreMatcher struct {
	patterns []string
	root     string
}

// Match implements gitignore.GitIgnore.
func (m *combinedIgnoreMatcher) Match(path string) gitignore.Match {
	return matchPath(path, m.patterns)
}

// Ignore implements gitignore.GitIgnore.
func (m *combinedIgnoreMatcher) Ignore(path string) bool {
	match := m.Match(path)
	return match != nil && match.Ignore()
}

// Include implements gitignore.GitIgnore.
func (m *combinedIgnoreMatcher) Include(path string) bool {
	match := m.Match(path)
	return match != nil && match.Include()
}

// Base implements gitignore.GitIgnore.
func (m *combinedIgnoreMatcher) Base() string {
	return m.root
}

// Absolute implements gitignore.GitIgnore.
func (m *combinedIgnoreMatcher) Absolute(path string, isdir bool) gitignore.Match {
	return m.Match(path)
}

// Relative implements gitignore.GitIgnore.
func (m *combinedIgnoreMatcher) Relative(path string, isdir bool) gitignore.Match {
	return m.Match(path)
}

// patternMatchResult implements gitignore.Match.
type patternMatchResult struct {
	pattern string
	ignored bool
}

func (m *patternMatchResult) Ignore() bool {
	return m.ignored
}

func (m *patternMatchResult) Include() bool {
	return !m.ignored
}

func (m *patternMatchResult) String() string {
	return m.pattern
}

func (m *patternMatchResult) Position() gitignore.Position {
	return gitignore.Position{}
}

// toPosixPath converts a path to forward slashes for gitignore compatibility.
func toPosixPath(p string) string {
	return filepath.ToSlash(p)
}

// matchPath checks if a path should be ignored
func matchPath(path string, patterns []string) gitignore.Match {
	posixPath := toPosixPath(path)
	for _, pattern := range patterns {
		negated := false
		trimmed := pattern

		if strings.HasPrefix(trimmed, "!") {
			negated = true
			trimmed = trimmed[1:]
		}

		// Check if path matches this pattern
		// Gitignore semantics: patterns starting with / only match at the root level
		matches := gitignoreMatch(posixPath, trimmed)
		if matches {
			return &patternMatchResult{pattern: pattern, ignored: !negated}
		}
	}
	return nil
}

// gitignoreMatch performs gitignore-style glob matching
// Returns true if path matches the pattern
// Note: patterns starting with / should only match at the root level
func gitignoreMatch(path, pattern string) bool {
	// Handle directory patterns (with trailing /)
	isDirPattern := strings.HasSuffix(pattern, "/")
	pattern = strings.TrimSuffix(pattern, "/")

	// Check for leading slash - only matches at root
	hasLeadingSlash := strings.HasPrefix(pattern, "/")
	if hasLeadingSlash {
		// Only match if path doesn't contain a slash (i.e., at root level)
		if strings.Contains(path, "/") {
			return false
		}
		// Remove the leading slash for matching
		pattern = pattern[1:]
	}

	// For directory patterns or paths ending in /, also check with trailing /
	// This allows matching "ignored-dir" against "ignored-dir/" and vice versa
	pathHasSlash := strings.HasSuffix(path, "/")
	pathBase := strings.TrimSuffix(path, "/")

	// Handle ** patterns
	if strings.Contains(pattern, "**") {
		// For ** patterns, we need to handle both cases
		if gitignoreMatchIter(pathBase, 0, pattern, 0) {
			return true
		}
		if isDirPattern || pathHasSlash {
			// Also check with trailing slash for directory matching
			return gitignoreMatchIter(pathBase+"/", 0, pattern, 0)
		}
		return false
	}

	// For directory patterns or paths ending in /, also check with trailing /
	if isDirPattern || pathHasSlash {
		if gitignoreMatchIter(pathBase+"/", 0, pattern, 0) {
			return true
		}
		if !isDirPattern && pathHasSlash {
			// Path ends in / but pattern doesn't - try without trailing /
			return gitignoreMatchIter(pathBase, 0, pattern, 0)
		}
	}

	return gitignoreMatchIter(pathBase, 0, pattern, 0)
}

// gitignoreMatchIter is the iterative glob matching implementation
// s is the string to match, p is the pattern
// sIdx and pIdx are the current indices in s and p respectively
func gitignoreMatchIter(s string, sIdx int, p string, pIdx int) bool {
	for pIdx < len(p) {
		c := p[pIdx]
		if c == '*' {
			// Check if this is a ** pattern
			if pIdx+1 < len(p) && p[pIdx+1] == '*' {
				// ** matches anything including /
				// Try matching 0 chars, then 1 char, etc. with **
				for i := sIdx; i <= len(s); i++ {
					if gitignoreMatchIter(s, i, p, pIdx+2) {
						return true
					}
				}
				return false
			}
			// Single * matches anything except /
			// Try matching * with 0, 1, 2... chars (greedy matching)
			// But * cannot match / in gitignore
			for i := sIdx; i <= len(s); i++ {
				// Skip if current position is /
				if i < len(s) && s[i] == '/' {
					continue
				}
				if gitignoreMatchIter(s, i, p, pIdx+1) {
					return true
				}
			}
			return false
		}

		if sIdx >= len(s) {
			return false
		}

		if c == '?' {
			sIdx++
			pIdx++
		} else {
			if s[sIdx] != c {
				return false
			}
			sIdx++
			pIdx++
		}
	}

	return sIdx >= len(s)
}

// ignores returns true if the path should be ignored.
// Path should be relative to the rootDir used when creating the matcher.
func (m *ignoreMatcher) ignores(path string) bool {
	posixPath := toPosixPath(path)
	return m.Ignore(posixPath)
}

// isIgnored checks if a full path should be ignored.
func isIgnored(m *ignoreMatcher, fullPath string, isDir bool) bool {
	if m == nil {
		return false
	}

	relPath, err := filepath.Rel(m.root, fullPath)
	if err != nil {
		return false
	}

	posixPath := toPosixPath(relPath)
	if isDir {
		posixPath += "/"
	}

	return m.ignores(posixPath)
}

// isHidden checks if a path is hidden (starts with . or contains . components).
func isHidden(path string) bool {
	// Normalize path separators
	path = filepath.ToSlash(path)
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

// Verify combinedIgnoreMatcher implements gitignore.GitIgnore
var _ gitignore.GitIgnore = (*combinedIgnoreMatcher)(nil)
