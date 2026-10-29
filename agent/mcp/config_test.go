package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeJSON(t *testing.T, path, data string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
}

func names(cfg Config) []string {
	var out []string
	for _, s := range cfg.Servers {
		out = append(out, s.Name)
	}
	return out
}

func TestLoadConfigMergesProjectOnlyWhenTrusted(t *testing.T) {
	agentDir, cwd := t.TempDir(), t.TempDir()
	writeJSON(t, filepath.Join(agentDir, "mcp.json"), `{
		"autoEnableCodemode": false,
		"mcpServers": {
			"docs": {"url": "https://example.com/mcp", "headers": {"Authorization": "Bearer x"}, "auth": {"provider": "github"}},
			"fs": {"command": "npx", "args": ["-y", "server"]},
			"tools": {"command": "tools"}
		}
	}`)
	project := filepath.Join(cwd, ".tau", "mcp.json")
	writeJSON(t, project, `{
		"autoEnableCodemode": true,
		"mcpServers": {
			"docs": {"enabled": false, "exposure": "direct"},
			"fs": {"command": "other"},
			"local": {"url": "http://127.0.0.1:9/mcp"}
		}
	}`)

	untrusted := LoadConfig(agentDir, cwd, false)
	assert.Empty(t, untrusted.Errors)
	assert.Equal(t, []string{"docs", "fs", "tools"}, names(untrusted))
	assert.Empty(t, untrusted.ProjectConfig)
	require.NotNil(t, untrusted.AutoEnableCodemode)
	assert.False(t, *untrusted.AutoEnableCodemode)

	cfg := LoadConfig(agentDir, cwd, true)
	assert.Empty(t, cfg.Errors)
	assert.Equal(t, project, cfg.ProjectConfig)
	assert.Equal(t, []string{"docs", "fs", "tools", "local"}, names(cfg))
	docs := cfg.Servers[0]
	assert.False(t, docs.Config.IsEnabled())
	assert.Equal(t, ExposureDirect, docs.Config.Exposure)
	assert.Equal(t, "Bearer x", docs.Config.Headers["Authorization"], "an override keeps global credentials")
	assert.Equal(t, "github", docs.Config.Auth.Provider)
	assert.Equal(t, ScopeGlobal, docs.Scope)
	assert.Equal(t, project, docs.Override)
	assert.Equal(t, "other", cfg.Servers[1].Config.Command)
	assert.Empty(t, cfg.Servers[1].Config.Args, "a full project entry replaces the global one")
	assert.Equal(t, ScopeProject, cfg.Servers[1].Scope)
	assert.True(t, *cfg.AutoEnableCodemode)
}

func TestLoadConfigDiagnosesAndSkipsBadEntries(t *testing.T) {
	agentDir, cwd := t.TempDir(), t.TempDir()
	writeJSON(t, filepath.Join(agentDir, "mcp.json"), `{"mcpServers": {
		"bad name": {"command": "x"},
		"null": null,
		"legacy": {"type": "sse", "url": "https://x/sse"},
		"ftp": {"url": "ftp://x"},
		"neither": {"exposure": "direct"},
		"badexp": {"command": "x", "exposure": "sometimes"},
		"badtool": {"command": "x", "toolExposure": {"a": "nope"}},
		"timeout": {"command": "x", "timeout": 0},
		"args": {"command": "x", "args": "one"},
		"plainauth": {"url": "http://example.com/mcp", "auth": {"provider": "p"}},
		"my-server": {"command": "ok"},
		"my_server": {"command": "clash"}
	}}`)
	writeJSON(t, filepath.Join(cwd, ".tau", "mcp.json"), `{"mcpServers": {
		"ghost": {"enabled": false},
		"my-server": {"enabled": false, "command2": 1},
		"remote": {"url": "https://example.com/mcp", "auth": {"provider": "p"}}
	}}`)
	cfg := LoadConfig(agentDir, cwd, true)
	assert.Equal(t, []string{"my-server"}, names(cfg))
	assert.Len(t, cfg.Errors, 14)
	for _, want := range []string{
		`invalid server name "bad name"`,
		`server "null" must be an object`,
		"legacy SSE transport is not supported",
		`server "ftp": url must be an http or https URL`,
		`server "neither" needs either "command" (stdio) or "url"`,
		`server "badexp": exposure must be one of`,
		`server "badtool": toolExposure "a" must be one of`,
		`server "timeout": timeout must be a positive number`,
		`server "args": args must be an array of strings`,
		`server "plainauth": auth requires an https URL`,
		`server "my_server" conflicts with "my-server"`,
		`server "ghost" needs "command" or "url", or a global server to override`,
		`server "my-server": an override can only set enabled, exposure, toolExposure`,
		`server "remote": auth is only allowed in the global mcp.json`,
	} {
		assert.Condition(t, func() bool {
			for _, e := range cfg.Errors {
				if strings.Contains(e, want) {
					return true
				}
			}
			return false
		}, "missing error %q in %q", want, cfg.Errors)
	}
}

func TestLoadConfigRejectsMalformedFiles(t *testing.T) {
	agentDir, cwd := t.TempDir(), t.TempDir()
	writeJSON(t, filepath.Join(agentDir, "mcp.json"), `{"mcpServers": []}`)
	writeJSON(t, filepath.Join(cwd, ".tau", "mcp.json"), `{oops`)
	cfg := LoadConfig(agentDir, cwd, true)
	require.Len(t, cfg.Errors, 2)
	assert.Contains(t, cfg.Errors[0], `expected an object with an "mcpServers" object`)
	assert.Empty(t, cfg.Servers)
	assert.Empty(t, LoadConfig(t.TempDir(), cwd, false).Errors, "missing files are fine")
}

func TestToolExposureForPrefersExactThenFirstPattern(t *testing.T) {
	agentDir := t.TempDir()
	writeJSON(t, filepath.Join(agentDir, "mcp.json"), `{"mcpServers": {"s": {"command": "x", "exposure": "hidden",
		"toolExposure": {"get_*": "direct", "get_*_x": "deferred", "get_secret": "codemode-deferred"}}, "d": {"command": "x"}}}`)
	cfg := LoadConfig(agentDir, t.TempDir(), false)
	require.Empty(t, cfg.Errors)
	s := cfg.Servers[0].Config
	assert.Equal(t, ExposureCodemode, s.ToolExposureFor("get_secret"), "exact key wins and the alias resolves")
	assert.Equal(t, ExposureDirect, s.ToolExposureFor("get_a_x"), "first matching pattern wins")
	assert.Equal(t, ExposureHidden, s.ToolExposureFor("put"))
	assert.Equal(t, ExposureCodemode, cfg.Servers[1].Config.ToolExposureFor("any"), "default exposure")
	assert.Equal(t, "mcp__my_server", Namespace("my-server"))
}

func TestUpdateServerConfigKeepsContentAndDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	require.NoError(t, os.WriteFile(path, []byte("{\n\t\"z\": 1,\n\t\"mcpServers\": {\n\t\t\"s\": {\"url\": \"http://x\", \"enabled\": false, \"exposure\": \"direct\"},\n\t\t\"o\": {\"exposure\": \"direct\"}\n\t}\n}\n"), 0o600))

	require.NoError(t, updateServerConfig(path, "s", ConfigPatch{Enabled: new(true), Exposure: ExposureCodemode}, false))
	require.NoError(t, updateServerConfig(path, "o", ConfigPatch{Exposure: ExposureCodemode}, false))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "{\n\t\"z\": 1,\n\t\"mcpServers\": {\n\t\t\"s\": {\n\t\t\t\"url\": \"http://x\"\n\t\t},\n\t\t\"o\": {\n\t\t\t\"exposure\": \"codemode\"\n\t\t}\n\t}\n}\n", string(data),
		"defaults drop their keys except in overrides; order and indentation stay")

	assert.ErrorContains(t, updateServerConfig(path, "missing", ConfigPatch{Enabled: new(false)}, false), `does not define MCP server "missing"`)
	require.NoError(t, os.WriteFile(path, []byte(`[]`), 0o600))
	assert.ErrorContains(t, updateServerConfig(path, "s", ConfigPatch{Enabled: new(false)}, true), "expected an object")
}
