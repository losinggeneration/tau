package packages

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPiResourcePathsExpandFourFamiliesAndOverrides(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"ext/keep.js", "ext/nested/keep.ts", "ext/drop.js", "ext/.hidden.js", "ext/node_modules/vendor.js", "skills/keep/SKILL.md", "prompts/keep.md", "themes/keep.json"} {
		p := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		require.NoError(t, os.WriteFile(p, []byte("body"), 0o600))
	}
	for _, tc := range []struct {
		name          string
		kind          ResourceKind
		entries, want []string
	}{
		{"extensions", ResourceKindExtension, []string{"ext/**/*.?s", "!ext/drop.js"}, []string{"ext/keep.js", "ext/nested/keep.ts"}},
		{"ordered override", ResourceKindExtension, []string{"ext/**/*.?s", "!ext/**", "+ext/drop.js"}, []string{"ext/drop.js"}},
		{"exact hidden", ResourceKindExtension, []string{"ext/.hidden.js"}, []string{"ext/.hidden.js"}},
		{"skills", ResourceKindSkill, []string{"skills/*", "!skills/keep", "+skills/keep"}, []string{"skills/keep/SKILL.md"}},
		{"prompts", ResourceKindPrompt, []string{"prompts"}, []string{"prompts/keep.md"}},
		{"themes", ResourceKindTheme, []string{"themes"}, []string{"themes/keep.json"}},
		{"empty", ResourceKindPrompt, []string{}, nil},
		{"missing", ResourceKindPrompt, []string{"absent"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PiResourcePaths(root, tc.entries, tc.kind)
			require.NoError(t, err)
			var rel []string
			for _, p := range got {
				r, err := filepath.Rel(root, p)
				require.NoError(t, err)
				rel = append(rel, filepath.ToSlash(r))
			}
			assert.Equal(t, tc.want, rel)
		})
	}
}

func TestReadPiManifestPreservesPresenceAndReportsMalformedInput(t *testing.T) {
	root := t.TempDir()
	got, err := ReadPiManifest(root)
	require.NoError(t, err)
	assert.Nil(t, got)
	for _, tc := range []struct {
		body string
		want *PiManifest
		bad  bool
	}{
		{`{"name":"plain"}`, nil, false},
		{"\xef\xbb\xbf" + `{"pi":{"extensions":[],"skills":["skills"],"prompts":["prompts"],"themes":["themes"]}}`, &PiManifest{Extensions: []string{}, Skills: []string{"skills"}, Prompts: []string{"prompts"}, Themes: []string{"themes"}}, false},
		{`{"pi":{"prompts":"invalid"}}`, nil, true},
		{`{`, nil, true},
	} {
		require.NoError(t, os.WriteFile(filepath.Join(root, "package.json"), []byte(tc.body), 0o600))
		got, err := ReadPiManifest(root)
		if tc.bad {
			require.Error(t, err)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
	}
}

func TestPiResourcePathsReportsMalformedNestedExtensionManifest(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "extensions", "nested")
	require.NoError(t, os.MkdirAll(nested, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(nested, "package.json"), []byte(`{"pi":{"extensions":"wrong"}}`), 0o600))
	got, err := PiResourcePaths(root, []string{"extensions"}, ResourceKindExtension)
	require.ErrorContains(t, err, "parse Pi manifest")
	assert.Empty(t, got)
}
