package ignore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIgnoreRulesUseGitignoreSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, rules, path  string
		directory, ignored bool
	}{
		{name: "last rule wins", rules: "*.md\n!keep.md\n", path: "keep.md"},
		{name: "escaped bang is literal", rules: "\\!note.md\n", path: "!note.md", ignored: true},
		{name: "escaped hash is literal", rules: "\\#note.md\n", path: "#note.md", ignored: true},
		{name: "root directory anchor", rules: "/build/\n", path: "build", directory: true, ignored: true},
		{name: "root anchor excludes descendants", rules: "/build/\n", path: "nested/build", directory: true},
		{name: "star cannot cross slash", rules: "/a*b.md\n", path: "a/keep/b.md"},
		{name: "question cannot cross slash", rules: "/a?b.md\n", path: "a/b.md"},
		{name: "double star matches zero directories", rules: "**/keep.md\n", path: "keep.md", ignored: true},
		{name: "character class", rules: "[ab].md\n", path: "a.md", ignored: true},
		{name: "directory pattern does not match file", rules: "cache/\n", path: "cache"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte(tc.rules), 0o600))
			matcher, err := New(root, root)
			require.NoError(t, err)
			assert.Equal(t, tc.ignored, matcher.Ignored(filepath.Join(root, filepath.FromSlash(tc.path)), tc.directory))
		})
	}
}

func TestIgnoreRulesKeepNestedBasesAndPrecedence(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	require.NoError(t, os.Mkdir(nested, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.md\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(nested, ".gitignore"), []byte("!keep.md\n/local.js\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(nested, ".ignore"), []byte("!local.js\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(nested, ".fdignore"), []byte("/local.js\n"), 0o600))
	matcher, err := New(nested, root)
	require.NoError(t, err)
	assert.False(t, matcher.Ignored(filepath.Join(nested, "keep.md"), false))
	assert.True(t, matcher.Ignored(filepath.Join(nested, "drop.md"), false))
	assert.True(t, matcher.Ignored(filepath.Join(nested, "local.js"), false))
	assert.False(t, matcher.Ignored(filepath.Join(root, "local.js"), false))
	assert.False(t, matcher.Ignored(filepath.Join(nested, "deeper", "local.js"), false))
	assert.False(t, matcher.Ignored(filepath.Join(root+"-outside", "drop.md"), false))
}
