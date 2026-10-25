package plugins

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/losinggeneration/tau/agent/packages"
)

func TestGeneratedPiCommandPreservesArguments(t *testing.T) {
	for _, entries := range [][]string{{"space name.js", "apostrophe's.js", `double"quote.js`}, {"$HOME.js", "$(touch injected).js", "semi;colon.js", "line\nbreak.js", "日本語.js", "back\\slash.js"}} {
		t.Run(entries[0], func(t *testing.T) {
			root := t.TempDir()
			node := filepath.Join(root, "node bin's", "node")
			require.NoError(t, os.MkdirAll(filepath.Dir(node), 0o700))
			require.NoError(t, os.WriteFile(node, []byte("node fixture"), 0o700))
			shim := filepath.Join(root, "shim path", "pi-ext-shim")
			data, err := packages.GenerateManifest(&packages.PiPackage{Name: "quoted", Version: "1", Extensions: entries}, shim, node)
			require.NoError(t, err)
			manifestPath := filepath.Join(root, "tau-package.toml")
			require.NoError(t, os.WriteFile(manifestPath, data, 0o600))
			manifest, err := parseManifest(manifestPath)
			require.NoError(t, err)
			want := append([]string{node, filepath.Join(shim, "index.js")}, entries...)
			require.Equal(t, want, splitCommand(manifest.Run.Command))
			assert.Equal(t, node, resolveRunCommandPath(root, manifest.Run.Command))
			assert.True(t, manifestNeedsManagedShim(manifest))
			cmd, err := buildPluginCmd(root, manifest, manifestPath, "")
			require.NoError(t, err)
			assert.Equal(t, root, cmd.Dir)
			if entries[0] == "space name.js" {
				assert.Equal(t, append([]string{"sh", "-c", pluginDirectScript}, want...), cmd.Args)
			} else {
				assert.Equal(t, []string{"sh", "-c", pluginShellScript, "sh", manifest.Run.Command, root}, cmd.Args)
			}
			manifest.Run.args = splitCommand(manifest.Run.Command)
			cmd, err = buildPluginCmd(root, manifest, manifestPath, "")
			require.NoError(t, err)
			assert.Equal(t, append([]string{"sh", "-c", pluginDirectScript}, want...), cmd.Args)
		})
	}
}

func TestResolveRunCommandPathQuotedRelativeInterpreter(t *testing.T) {
	root := t.TempDir()
	node := filepath.Join(root, "node bin")
	require.NoError(t, os.WriteFile(node, []byte("fixture"), 0o700))
	assert.Equal(t, node, resolveRunCommandPath(root, `'./node bin' entry.js`))
	assert.Empty(t, resolveRunCommandPath(root, `'./node bin entry.js`))
}
