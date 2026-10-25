package artifacts

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/denormal/go-gitignore"
	"github.com/losinggeneration/tau/agent/packages"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/paths"
)

// ignoreFiles lists the supported ignore file names.
var ignoreFiles = []string{".gitignore", ".ignore", ".fdignore"}

// FilterConfig holds pattern-based filtering configuration for discovery.
type FilterConfig struct {
	// Include includes paths matching these patterns. If empty, all paths are included.
	Include []string
	// Exclude excludes paths matching these patterns (after includes).
	Exclude []string
	// ForceInclude forces inclusion of exact paths (overrides exclusions).
	ForceInclude []string
	// ForceExclude forces exclusion of exact paths (overrides force includes).
	ForceExclude []string
}

// ignoreMatcher wraps a gitignore-style matcher.
type ignoreMatcher struct {
	gitignore.GitIgnore
	root string
}

// toPosixPath converts a path to forward slashes for comparison.
func toPosixPath(p string) string {
	return strings.ReplaceAll(p, string(filepath.Separator), "/")
}

// newIgnoreMatcher creates a matcher for a directory, loading ignore files found there.
// It also loads ignore files from parent directories up to rootDir.
// It uses the go-gitignore library for proper gitignore-compatible pattern matching.
func newIgnoreMatcher(dir, rootDir string) *ignoreMatcher {
	// Collect all patterns from ignore files
	var allPatterns []string

	collectDir := dir
	for {
		for _, filename := range ignoreFiles {
			ignorePath := filepath.Join(collectDir, filename)
			data, err := os.ReadFile(ignorePath)
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(data), "\n") {
				trimmed := strings.TrimSpace(line)
				if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
					allPatterns = append(allPatterns, trimmed)
				}
			}
		}

		// Move to parent directory, stop at root or filesystem boundary
		parent := filepath.Dir(collectDir)
		if parent == collectDir {
			// Filesystem root reached
			break
		}
		if collectDir == rootDir {
			// We've processed rootDir, stop here (don't process parent directories)
			break
		}
		if !strings.HasPrefix(parent, rootDir) {
			// Parent is outside rootDir
			break
		}
		collectDir = parent
	}

	// Create combined matcher from all patterns
	matcher := &combinedPatternsMatcher{
		patterns: allPatterns,
		root:     rootDir,
	}

	return &ignoreMatcher{
		GitIgnore: matcher,
		root:      rootDir,
	}
}

// combinedPatternsMatcher creates a GitIgnore from a list of patterns
type combinedPatternsMatcher struct {
	patterns []string
	root     string
}

// Match implements GitIgnore for combinedPatternsMatcher
func (m *combinedPatternsMatcher) Match(path string) gitignore.Match {
	return matchPath(path, m.patterns)
}

// Ignore implements GitIgnore
func (m *combinedPatternsMatcher) Ignore(path string) bool {
	return m.Match(path) != nil && m.Match(path).Ignore()
}

// Include implements GitIgnore
func (m *combinedPatternsMatcher) Include(path string) bool {
	return m.Match(path) != nil && m.Match(path).Include()
}

// Base implements GitIgnore
func (m *combinedPatternsMatcher) Base() string {
	return m.root
}

// Absolute implements GitIgnore
func (m *combinedPatternsMatcher) Absolute(path string, isdir bool) gitignore.Match {
	return m.Match(path)
}

// Relative implements GitIgnore
func (m *combinedPatternsMatcher) Relative(path string, isdir bool) gitignore.Match {
	return m.Match(path)
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
		// Gitignore semantics: patterns starting with / only match at root
		matches := gitignoreMatch(posixPath, trimmed)
		if matches {
			return &patternMatch{pattern: pattern, ignored: !negated}
		}
	}
	return nil
}

// patternMatch implements gitignore.Match
type patternMatch struct {
	pattern string
	ignored bool
}

func (m *patternMatch) Ignore() bool {
	return m.ignored
}

func (m *patternMatch) Include() bool {
	return !m.ignored
}

func (m *patternMatch) String() string {
	return m.pattern
}

func (m *patternMatch) Position() gitignore.Position {
	return gitignore.Position{}
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

// combinedPatternsMatcher implements gitignore.GitIgnore interface
var _ gitignore.GitIgnore = (*combinedPatternsMatcher)(nil)

// combinedGitIgnore creates a combined GitIgnore that matches against all provided GitIgnore objects
type combinedGitIgnore struct {
	matchers []gitignore.GitIgnore
	root     string
}

// Match implements GitIgnore
func (c *combinedGitIgnore) Match(path string) gitignore.Match {
	for _, m := range c.matchers {
		if match := m.Match(path); match != nil {
			return match
		}
	}
	return nil
}

// Ignore implements GitIgnore
func (c *combinedGitIgnore) Ignore(path string) bool {
	for _, m := range c.matchers {
		if m.Ignore(path) {
			return true
		}
	}
	return false
}

// Include implements GitIgnore
func (c *combinedGitIgnore) Include(path string) bool {
	for _, m := range c.matchers {
		if m.Include(path) {
			return true
		}
	}
	return false
}

// Base implements GitIgnore
func (c *combinedGitIgnore) Base() string {
	return c.root
}

// Absolute implements GitIgnore
func (c *combinedGitIgnore) Absolute(path string, isdir bool) gitignore.Match {
	for _, m := range c.matchers {
		if match := m.Absolute(path, isdir); match != nil {
			return match
		}
	}
	return nil
}

// Relative implements GitIgnore
func (c *combinedGitIgnore) Relative(path string, isdir bool) gitignore.Match {
	for _, m := range c.matchers {
		if match := m.Relative(path, isdir); match != nil {
			return match
		}
	}
	return nil
}

// combinedGitIgnore implements gitignore.GitIgnore interface
var _ gitignore.GitIgnore = (*combinedGitIgnore)(nil)

// ignores returns true if the path should be ignored.
// The path should be relative to the rootDir used when creating the matcher.
// It uses Posix path format internally for gitignore compatibility.
func (m *ignoreMatcher) ignores(path string) bool {
	// Convert to forward slashes for gitignore compatibility
	posixPath := toPosixPath(path)
	// Use Ignore() method which returns true if the path should be ignored
	return m.Ignore(posixPath)
}

// matchGlob does glob matching with support for * and ?.
func matchGlob(path, pattern string) bool {
	// Handle negated patterns
	negated := false
	if strings.HasPrefix(pattern, "!") {
		negated = true
		pattern = pattern[1:]
	}

	matches := globMatch(path, pattern)
	if negated {
		return !matches
	}
	return matches
}

// globMatch does glob matching with standard semantics:
// - * matches any sequence of characters (including /)
// - ? matches exactly one character
// - Pattern and string must both be consumed for a match
func globMatch(s, p string) bool {
	// Handle ** patterns
	if strings.Contains(p, "**") {
		return matchDoubleStar(s, p)
	}

	return globMatchIter(s, 0, p, 0)
}

// globMatchIter is the iterative glob matching implementation.
func globMatchIter(s string, sIdx int, p string, pIdx int) bool {
	for pIdx < len(p) {
		c := p[pIdx]
		if c == '*' {
			// Try matching * with 0, 1, 2... chars (greedy matching)
			for i := sIdx; i <= len(s); i++ {
				if globMatchIter(s, i, p, pIdx+1) {
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

// globMatchFrom removed - using unified globMatchIter instead

// matchDoubleStar handles ** glob patterns.
func matchDoubleStar(s, p string) bool {
	parts := strings.Split(p, "**")
	if len(parts) == 1 {
		return true
	}

	idx := 0
	for i, part := range parts {
		if part == "" {
			continue
		}

		if i == 0 {
			// First part must match from start
			if !strings.HasPrefix(s[idx:], part) {
				return false
			}
			idx += len(part)
		} else if i == len(parts)-1 {
			// Last part must match at end
			if !strings.HasSuffix(s, part) {
				return false
			}
		} else {
			// Middle parts can match anywhere
			pos := strings.Index(s[idx:], part)
			if pos == -1 {
				return false
			}
			idx += pos + len(part)
		}
	}
	return true
}

// isPattern returns true if the string contains glob patterns.
func isPattern(s string) bool {
	return strings.HasPrefix(s, "!") ||
		strings.HasPrefix(s, "+") ||
		strings.HasPrefix(s, "-") ||
		strings.Contains(s, "*") ||
		strings.Contains(s, "?")
}

// splitPatterns separates plain entries from pattern entries.
func splitPatterns(entries []string) (plain []string, patterns []string) {
	for _, entry := range entries {
		if isPattern(entry) {
			patterns = append(patterns, entry)
		} else {
			plain = append(plain, entry)
		}
	}
	return
}

// matchesAnyPattern checks if a file path matches any of the patterns.
// For skill files, also checks parent directory name.
func matchesAnyPattern(filePath string, patterns []string, baseDir string) bool {
	rel := toPosixPath(relativePath(filePath, baseDir))
	name := filepath.Base(filePath)
	filePathPosix := toPosixPath(filePath)

	isSkillFile := name == "SKILL.md"
	var parentDir, parentRel, parentDirPosix string
	if isSkillFile {
		parentDir = filepath.Dir(filePath)
		parentRel = toPosixPath(relativePath(parentDir, baseDir))
		parentDirPosix = toPosixPath(parentDir)
	}

	for _, pattern := range patterns {
		normalized := toPosixPath(paths.ResourcePath(pattern, "", ""))
		if globMatch(rel, normalized) || globMatch(name, normalized) || globMatch(filePathPosix, normalized) {
			return true
		}
		if isSkillFile {
			parentName := filepath.Base(parentDir)
			if globMatch(parentRel, normalized) || globMatch(parentName, normalized) || globMatch(parentDirPosix, normalized) {
				return true
			}
		}
	}
	return false
}

// matchesAnyExactPattern checks if a file path matches any exact pattern.
// For skill files, also checks parent directory exact matches.
func matchesAnyExactPattern(filePath string, patterns []string, baseDir string) bool {
	if len(patterns) == 0 {
		return false
	}

	rel := toPosixPath(relativePath(filePath, baseDir))
	filePathPosix := toPosixPath(filePath)

	isSkillFile := filepath.Base(filePath) == "SKILL.md"
	var parentDir, parentRel, parentDirPosix string
	if isSkillFile {
		parentDir = filepath.Dir(filePath)
		parentRel = toPosixPath(relativePath(parentDir, baseDir))
		parentDirPosix = toPosixPath(parentDir)
	}

	for _, pattern := range patterns {
		normalized := toPosixPath(normalizeExactPattern(paths.ResourcePath(pattern, "", "")))
		if normalized == rel || normalized == filePathPosix {
			return true
		}
		if isSkillFile {
			if normalized == parentRel || normalized == parentDirPosix {
				return true
			}
		}
	}
	return false
}

// normalizeExactPattern removes leading ./ or .\
func normalizeExactPattern(pattern string) string {
	if strings.HasPrefix(pattern, "./") || strings.HasPrefix(pattern, ".\\") {
		return pattern[2:]
	}
	return pattern
}

// relativePath returns the relative path from base to file.
func relativePath(filePath, baseDir string) string {
	rel, err := filepath.Rel(baseDir, filePath)
	if err != nil {
		return filePath
	}
	return rel
}

// isIgnored checks if a file path should be ignored based on the accumulated
// ignore rules. This is used during directory traversal.
func isIgnored(m *ignoreMatcher, fullPath string, isDir bool) bool {
	if m == nil {
		return false
	}

	relPath := toPosixPath(relativePath(fullPath, m.root))
	// For directories, add trailing slash for gitignore compatibility
	if isDir {
		relPath += "/"
	}

	return m.ignores(relPath)
}

// ApplyFilter applies pattern-based filtering to a list of paths.
// Pattern types:
//   - Plain patterns: include matching paths
//   - !pattern: exclude matching paths
//   - +path: force-include exact path (overrides exclusions)
//   - -path: force-exclude exact path (overrides force-includes)
func ApplyFilter(allPaths []string, patterns []string, baseDir string) []string {
	var includes, excludes, forceIncludes, forceExcludes []string

	for _, p := range patterns {
		if strings.HasPrefix(p, "+") {
			forceIncludes = append(forceIncludes, p[1:])
		} else if strings.HasPrefix(p, "-") {
			forceExcludes = append(forceExcludes, p[1:])
		} else if strings.HasPrefix(p, "!") {
			excludes = append(excludes, p[1:])
		} else {
			includes = append(includes, p)
		}
	}

	// Step 1: Apply includes (or all if no includes)
	var result []string
	if len(includes) == 0 {
		result = make([]string, len(allPaths))
		copy(result, allPaths)
	} else {
		for _, path := range allPaths {
			if matchesAnyPattern(path, includes, baseDir) {
				result = append(result, path)
			}
		}
	}

	// Step 2: Apply excludes
	if len(excludes) > 0 {
		filtered := result[:0]
		for _, path := range result {
			if !matchesAnyPattern(path, excludes, baseDir) {
				filtered = append(filtered, path)
			}
		}
		result = filtered
	}

	// Step 3: Force-include (add back from allPaths, overriding exclusions)
	if len(forceIncludes) > 0 {
		resultSet := make(map[string]bool)
		for _, p := range result {
			resultSet[p] = true
		}
		for _, path := range allPaths {
			if !resultSet[path] && matchesAnyExactPattern(path, forceIncludes, baseDir) {
				result = append(result, path)
				resultSet[path] = true
			}
		}
	}

	// Step 4: Force-exclude (remove even if included or force-included)
	if len(forceExcludes) > 0 {
		filtered := result[:0]
		for _, path := range result {
			if !matchesAnyExactPattern(path, forceExcludes, baseDir) {
				filtered = append(filtered, path)
			}
		}
		result = filtered
	}

	return result
}

// ApplyPackageFilter applies a package policy, then any ordered project delta.
// Nil lists inherit defaults; explicit empty lists disable that resource family.
func ApplyPackageFilter(all []string, pkg packages.ResolvedPackage, kind string) []string {
	patternsFor := func(source ai.PackageSource) []string {
		switch kind {
		case "extensions":
			return source.Extensions
		case "skills":
			return source.Skills
		case "prompts":
			return source.Prompts
		case "themes":
			return source.Themes
		default:
			return nil
		}
	}
	applyDelta := func(enabled map[string]bool, patterns []string) {
		for _, pattern := range patterns {
			target := pattern
			if strings.HasPrefix(pattern, "+") || strings.HasPrefix(pattern, "-") || strings.HasPrefix(pattern, "!") {
				target = pattern[1:]
			}
			exact := strings.HasPrefix(pattern, "+") || strings.HasPrefix(pattern, "-")
			for _, path := range all {
				matches := matchesAnyPattern(path, []string{target}, pkg.Root)
				if exact {
					matches = matchesAnyExactPattern(path, []string{target}, pkg.Root)
				}
				if matches {
					enabled[path] = !strings.HasPrefix(pattern, "-") && !strings.HasPrefix(pattern, "!")
				}
			}
		}
	}
	patterns := patternsFor(pkg.Source)
	enabled := make(map[string]bool, len(all))
	if pkg.Source.Autoload != nil && !*pkg.Source.Autoload {
		applyDelta(enabled, patterns)
	} else if patterns == nil || len(patterns) > 0 {
		for _, path := range ApplyFilter(all, patterns, pkg.Root) {
			enabled[path] = true
		}
	}
	if pkg.Delta != nil {
		applyDelta(enabled, patternsFor(*pkg.Delta))
	}
	var result []string
	for _, path := range all {
		if enabled[path] {
			result = append(result, path)
		}
	}
	return result
}
