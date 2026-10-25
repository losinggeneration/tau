package artifacts

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/losinggeneration/tau/agent/packages"
	"github.com/losinggeneration/tau/ai"
)

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		path    string
		pattern string
		want    bool
	}{
		// Basic matching
		{"foo", "foo", true},
		{"foo", "bar", false},

		// Glob patterns
		{"foo.md", "*.md", true},
		{"bar.md", "*.md", true},
		{"foo.txt", "*.md", false},
		{"README.md", "README.md", true},

		// Directory patterns - * matches across /
		{"skills/foo", "skills/*", true},
		{"skills/bar/baz", "skills/*", true}, // * matches bar/baz

		// ? matching - exactly 1 char each
		{"foo.md", "foo.??", true},   // 4 literal + 2 ? = 6, "md" = 2
		{"foo.md", "foo.???", false}, // 4 literal + 3 ? = 7, "md" = 2

		// Exact patterns
		{"foo/bar.md", "foo/bar.md", true},
	}

	for _, tt := range tests {
		got := matchGlob(tt.path, tt.pattern)
		if got != tt.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.path, tt.pattern, got, tt.want)
		}
	}
}

func TestMatchesAnyPattern(t *testing.T) {
	baseDir := "/test"

	tests := []struct {
		filePath string
		patterns []string
		want     bool
	}{
		{"/test/skills/my-skill/SKILL.md", []string{"skills/*"}, true},
		{"/test/skills/my-skill/SKILL.md", []string{"!skills/*"}, false},
		{"/test/skills/my-skill/SKILL.md", []string{"*"}, true},
		{"/test/skills/test", []string{"skills/test"}, true},
	}

	for _, tt := range tests {
		got := matchesAnyPattern(tt.filePath, tt.patterns, baseDir)
		if got != tt.want {
			t.Errorf("matchesAnyPattern(%q, %v) = %v, want %v", tt.filePath, tt.patterns, got, tt.want)
		}
	}
}

func TestApplyFilter(t *testing.T) {
	baseDir := "/test"
	allPaths := []string{
		"/test/skills/skill1/SKILL.md",
		"/test/skills/skill2/SKILL.md",
		"/test/prompts/prompt1.md",
		"/test/skills/excluded/skill3.md",
	}

	tests := []struct {
		name     string
		patterns []string
		wantLen  int
	}{
		{"no patterns", nil, 4},
		{"exclude skills", []string{"!skills/*"}, 1}, // Only prompts/prompt1.md remains
		{"include skills only", []string{"skills/*"}, 3},
		{"force include excluded", []string{"-skills/excluded/*", "+skills/excluded/skill3.md"}, 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var patterns []string
			if tt.patterns != nil {
				patterns = tt.patterns
			}
			got := ApplyFilter(allPaths, patterns, baseDir)
			if len(got) != tt.wantLen {
				t.Errorf("ApplyFilter() got %d paths, want %d", len(got), tt.wantLen)
			}
		})
	}
}

func TestNewIgnoreMatcher(t *testing.T) {
	// Create a temp directory with a .gitignore file
	tmpDir, err := os.MkdirTemp("", "tau-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Create .gitignore
	gitignorePath := filepath.Join(tmpDir, ".gitignore")
	err = os.WriteFile(gitignorePath, []byte("*.log\nnode_modules/\n"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	// Create a subdir with its own .gitignore
	subDir := filepath.Join(tmpDir, "subdir")
	err = os.MkdirAll(subDir, 0o755)
	if err != nil {
		t.Fatal(err)
	}

	subGitignore := filepath.Join(subDir, ".gitignore")
	err = os.WriteFile(subGitignore, []byte("*.tmp\n"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	// Test matcher
	m := newIgnoreMatcher(tmpDir, tmpDir)

	if !m.ignores("debug.log") {
		t.Error("expected .log file to be ignored")
	}
	if !m.ignores("node_modules/") {
		t.Error("expected node_modules/ to be ignored")
	}
	if m.ignores("readme.md") {
		t.Error("expected readme.md to NOT be ignored")
	}
}

func TestNewIgnoreMatcherFromSubdir(t *testing.T) {
	// Test that we can create a matcher from a subdirectory
	// and it correctly loads patterns from the root
	tmpDir, err := os.MkdirTemp("", "tau-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Create .gitignore in root
	gitignorePath := filepath.Join(tmpDir, ".gitignore")
	err = os.WriteFile(gitignorePath, []byte("*.log\n*.tmp\n"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	// Create a subdir
	subDir := filepath.Join(tmpDir, "subdir")
	err = os.MkdirAll(subDir, 0o755)
	if err != nil {
		t.Fatal(err)
	}

	// Create matcher from subdir perspective
	m := newIgnoreMatcher(subDir, tmpDir)

	// Pattern *.log from root should match log files in subdir
	if !m.ignores("subdir/debug.log") {
		t.Error("expected subdir/debug.log to be ignored by root *.log pattern")
	}
	// Pattern *.tmp from root should match tmp files in subdir
	if !m.ignores("subdir/test.tmp") {
		t.Error("expected subdir/test.tmp to be ignored by root *.tmp pattern")
	}
	// Non-matching pattern
	if m.ignores("subdir/readme.md") {
		t.Error("expected subdir/readme.md to NOT be ignored")
	}
}

func TestNewIgnoreMatcherFromSubdirLoadsSubdirIgnoreFiles(t *testing.T) {
	tmpDir := t.TempDir()
	subDir := filepath.Join(tmpDir, "subdir")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, ".gitignore"), []byte("*.tmp\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newIgnoreMatcher(subDir, tmpDir)

	if !m.ignores("subdir/debug.log") {
		t.Error("expected root .gitignore pattern to apply")
	}
	if !m.ignores("subdir/scratch.tmp") {
		t.Error("expected subdir .gitignore pattern to apply")
	}
	if m.ignores("subdir/readme.md") {
		t.Error("expected unrelated file not to be ignored")
	}
}

func TestIgnoreFileSupport(t *testing.T) {
	// Test that all ignore file names are recognized
	for _, name := range ignoreFiles {
		if name != ".gitignore" && name != ".ignore" && name != ".fdignore" {
			t.Errorf("unexpected ignore file name: %s", name)
		}
	}
}

func TestIsPattern(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"foo", false},
		{"!foo", true},
		{"+foo", true},
		{"-foo", true},
		{"*.md", true},
		{"foo?", true},
	}

	for _, tt := range tests {
		got := isPattern(tt.s)
		if got != tt.want {
			t.Errorf("isPattern(%q) = %v, want %v", tt.s, got, tt.want)
		}
	}
}

func TestSplitPatterns(t *testing.T) {
	entries := []string{"foo", "!bar", "+baz", "*.md", "test"}
	plain, patterns := splitPatterns(entries)

	if len(plain) != 2 {
		t.Errorf("expected 2 plain entries, got %d", len(plain))
	}
	if len(patterns) != 3 {
		t.Errorf("expected 3 pattern entries, got %d", len(patterns))
	}
}

func TestApplyFilterExpandsHomeAfterModifiers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := filepath.Join(home, "skills", "a", "SKILL.md")
	b := filepath.Join(home, "skills", "b", "SKILL.md")
	other := filepath.Join(t.TempDir(), "other.md")
	for _, tc := range []struct {
		patterns []string
		want     []string
	}{
		{[]string{"~/skills/*"}, []string{a, b}},
		{[]string{"!~/skills/*"}, []string{other}},
		{[]string{"!~/skills/*", "+~/skills/a"}, []string{other, a}},
		{[]string{"!~/skills/*", "+~/skills/a", "-~/skills/a"}, []string{other}},
	} {
		if got := ApplyFilter([]string{a, b, other}, tc.patterns, t.TempDir()); !slices.Equal(got, tc.want) {
			t.Errorf("ApplyFilter(%v) = %v, want %v", tc.patterns, got, tc.want)
		}
	}
}

func TestApplyPackageFilterPreservesPresenceAndOrderedDelta(t *testing.T) {
	root := t.TempDir()
	all := []string{filepath.Join(root, "prompts", "keep.md"), filepath.Join(root, "prompts", "drop.md"), filepath.Join(root, "prompts", "last.md")}
	for _, tc := range []struct {
		name     string
		patterns []string
		autoload *bool
		delta    *ai.PackageSource
		want     []string
	}{
		{name: "absent", want: all},
		{name: "empty", patterns: []string{}, want: nil},
		{name: "include exclude force", patterns: []string{"prompts/*", "!prompts/*", "+prompts/keep.md", "+prompts/last.md", "-prompts/last.md"}, want: all[:1]},
		{name: "disabled starts empty", autoload: new(false), want: nil},
		{name: "disabled ordered", autoload: new(false), patterns: []string{"+prompts/keep.md", "-prompts/keep.md", "prompts/drop.md"}, want: all[1:2]},
		{name: "project delta", delta: &ai.PackageSource{Autoload: new(false), Prompts: []string{"-prompts/drop.md"}}, want: []string{all[0], all[2]}},
		{name: "empty project delta inherits", delta: &ai.PackageSource{Autoload: new(false), Prompts: []string{}}, want: all},
		{name: "delta enables excluded", patterns: []string{}, delta: &ai.PackageSource{Autoload: new(false), Prompts: []string{"+prompts/keep.md"}}, want: all[:1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ApplyPackageFilter(all, packages.ResolvedPackage{Root: root, Source: ai.PackageSource{Autoload: tc.autoload, Prompts: tc.patterns}, Delta: tc.delta}, "prompts")
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
