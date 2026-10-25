package packages

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectExtensionResourcesReadsTypedPiManifest(t *testing.T) {
	for _, body := range []string{
		`{"pi":{"extensions":["comma,name.js","quote\"name.js"]}}`,
		"\xef\xbb\xbf" + `{"pi":{"extensions":["comma,name.js","quote\"name.js"]}}`,
	} {
		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, "package.json"), []byte(body), 0o600))
		for _, name := range []string{"comma,name.js", `quote"name.js`, "index.js"} {
			require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("extension"), 0o600))
		}
		got, err := collectExtensionResources(root, nil)
		require.NoError(t, err)
		assert.Equal(t, []Resource{{Path: filepath.Join(root, "comma,name.js"), Kind: ResourceKindExtension, Valid: true}, {Path: filepath.Join(root, `quote"name.js`), Kind: ResourceKindExtension, Valid: true}}, got)
	}
}

func TestCollectExtensionResourcesReportsMalformedPiManifest(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"pi":{"extensions":"wrong"}}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "index.js"), []byte("extension"), 0o600))
	got, err := collectExtensionResources(root, nil)
	require.ErrorContains(t, err, "parse Pi manifest")
	assert.Empty(t, got)
}

func TestCollectExtensionResourcesReportsNestedMalformedManifest(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	require.NoError(t, os.Mkdir(nested, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(nested, "package.json"), []byte(`{"pi":{"extensions":"wrong"}}`), 0o600))
	got, err := collectExtensionResources(root, nil)
	require.ErrorContains(t, err, "parse Pi manifest")
	assert.Empty(t, got)
}

func TestCollectPackageResourcesReportsCollectionFailures(t *testing.T) {
	for _, dir := range []string{"extensions", "skills", "prompts", "themes"} {
		t.Run(dir, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.Symlink(dir, filepath.Join(root, dir)))
			got, err := CollectPackageResources(root, nil)
			require.Error(t, err)
			assert.Empty(t, got)
		})
	}
	for _, kind := range []ResourceKind{ResourceKindSkill, ResourceKindPrompt, ResourceKindTheme} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.Symlink("broken", filepath.Join(root, "broken")))
			manifest := &Manifest{}
			switch kind {
			case ResourceKindSkill:
				manifest.Skills = []ArtifactPath{{Path: "broken"}}
			case ResourceKindPrompt:
				manifest.Prompts = []ArtifactPath{{Path: "broken"}}
			case ResourceKindTheme:
				manifest.Themes = []ArtifactPath{{Path: "broken"}}
			}
			got, err := CollectPackageResources(root, manifest)
			require.Error(t, err)
			assert.Empty(t, got)
		})
	}
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "extensions"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "extensions", "package.json"), []byte(`{"pi":{"extensions":"wrong"}}`), 0o600))
	got, err := CollectPackageResources(root, nil)
	require.ErrorContains(t, err, "parse Pi manifest")
	assert.Empty(t, got)
}

func TestCollectPackageResourcesSkipsMissingOptionalDirectories(t *testing.T) {
	got, err := CollectPackageResources(t.TempDir(), nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestCollectExtensionResourcesSkipsIgnoredDirectories(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored/\n"), 0o600))
	for _, dir := range []string{"visible", "ignored", ".hidden", "node_modules"} {
		require.NoError(t, os.Mkdir(filepath.Join(root, dir), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(root, dir, "index.js"), []byte("extension"), 0o600))
	}
	got, err := collectExtensionResources(root, nil)
	require.NoError(t, err)
	assert.Equal(t, []Resource{{Path: filepath.Join(root, "visible", "index.js"), Kind: ResourceKindExtension, Valid: true}}, got)
}

func TestResourceCollectorsReportEntryStatErrors(t *testing.T) {
	for _, name := range []string{"index.ts", "index.js", "SKILL.md"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.Symlink(name, filepath.Join(root, name)))
			var got []Resource
			var err error
			if name == "SKILL.md" {
				got, err = collectSkillResources(root, root, "pi")
			} else {
				got, err = collectExtensionResources(root, nil)
			}
			require.Error(t, err)
			assert.Empty(t, got)
		})
	}
}

func TestResourceCollectorsDoNotReturnDirectoryAsEntry(t *testing.T) {
	for _, name := range []string{"index.ts", "SKILL.md"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, name), 0o700))
			var got []Resource
			var err error
			if name == "SKILL.md" {
				got, err = collectSkillResources(root, root, "pi")
			} else {
				got, err = collectExtensionResources(root, nil)
			}
			require.NoError(t, err)
			assert.Empty(t, got)
		})
	}
}

func TestCollectExtensionResourcesFollowsLinkedEntries(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(external, "index.ts"), []byte("entry"), 0o600))
	require.NoError(t, os.Symlink(external, filepath.Join(root, "linked")))
	require.NoError(t, os.Symlink(filepath.Join(external, "index.ts"), filepath.Join(root, "file.js")))
	require.NoError(t, os.Symlink(filepath.Join(external, "missing"), filepath.Join(root, "broken.js")))
	got, err := collectExtensionResources(root, nil)
	require.NoError(t, err)
	require.Equal(t, []Resource{{Path: filepath.Join(root, "file.js"), Kind: ResourceKindExtension, Valid: true}, {Path: filepath.Join(root, "linked", "index.ts"), Kind: ResourceKindExtension, Valid: true}}, got)
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("linked/\n"), 0o600))
	got, err = collectExtensionResources(root, nil)
	require.NoError(t, err)
	require.Equal(t, []Resource{{Path: filepath.Join(root, "file.js"), Kind: ResourceKindExtension, Valid: true}}, got)
}

func TestCollectExtensionResourcesReportsLinkedEntryStatFailure(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Symlink("loop.js", filepath.Join(root, "loop.js")))
	got, err := collectExtensionResources(root, nil)
	require.ErrorContains(t, err, "stat extension entry")
	require.Empty(t, got)
}
