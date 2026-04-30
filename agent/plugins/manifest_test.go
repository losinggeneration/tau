package plugins

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/commands"
	"github.com/losinggeneration/tau/ui"
)

func TestParseManifest(t *testing.T) {
	manifestContent := `
[package]
name = "test-plugin"
version = "0.1.0"
description = "A test plugin"

[plugin]
api_version = "1"
tool_prefix = "test-"

[failure]
max_retries = 5
backoff_base = 2.0
backoff_factor = 1.5
init_timeout = 15.0
exec_timeout = 60.0
on_event_timeout = 10.0
on_cmd_timeout = 8.0
`

	// Create a temp file with the manifest content
	tmpDir := t.TempDir()
	manifestPath := tmpDir + "/tau-package.toml"
	if err := os.WriteFile(manifestPath, []byte(manifestContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Parse the manifest
	m, err := parseManifest(manifestPath)
	if err != nil {
		t.Fatalf("parseManifest failed: %v", err)
	}

	// Verify package info
	if m.Package.Name != "test-plugin" {
		t.Errorf("expected name 'test-plugin', got %q", m.Package.Name)
	}
	if m.Package.Version != "0.1.0" {
		t.Errorf("expected version '0.1.0', got %q", m.Package.Version)
	}

	// Verify plugin info
	if m.Plugin == nil {
		t.Fatal("expected plugin section to be present")
	}
	if m.Plugin.APIVersion != "1" {
		t.Errorf("expected api_version '1', got %q", m.Plugin.APIVersion)
	}
	if m.Plugin.ToolPrefix != "test-" {
		t.Errorf("expected tool_prefix 'test-', got %q", m.Plugin.ToolPrefix)
	}

	// Verify failure config
	if m.Failure == nil {
		t.Fatal("expected failure section to be present")
	}
	if m.Failure.MaxRetries != 5 {
		t.Errorf("expected max_retries 5, got %d", m.Failure.MaxRetries)
	}
	if m.Failure.BackoffBase != 2.0 {
		t.Errorf("expected backoff_base 2.0, got %f", m.Failure.BackoffBase)
	}

	// Verify getTimeout
	if got := m.getTimeout("init", 10); got != 15*1e9 {
		t.Errorf("expected init timeout 15s, got %v", got)
	}
	if got := m.getTimeout("exec", 30); got != 60*1e9 {
		t.Errorf("expected exec timeout 60s, got %v", got)
	}
	if got := m.getTimeout("on_event", 5); got != 10*1e9 {
		t.Errorf("expected on_event timeout 10s, got %v", got)
	}
	if got := m.getTimeout("on_cmd", 5); got != 8*1e9 {
		t.Errorf("expected on_cmd timeout 8s, got %v", got)
	}
	if got := m.getTimeout("unknown", 42); got != 42 {
		t.Errorf("expected default timeout 42, got %v", got)
	}

	// Verify getFailureConfig
	cfg := m.getFailureConfig()
	if cfg.MaxAttempts != 5 {
		t.Errorf("expected MaxAttempts 5, got %d", cfg.MaxAttempts)
	}
	if cfg.BackoffBase != 2*1e9 {
		t.Errorf("expected BackoffBase 2s, got %v", cfg.BackoffBase)
	}
	if cfg.BackoffFactor != 1.5 {
		t.Errorf("expected BackoffFactor 1.5, got %f", cfg.BackoffFactor)
	}
}

func TestGetTimeoutsFromManifest(t *testing.T) {
	// Test that PluginInfo.getTimeouts merges manifest with defaults
	info := &PluginInfo{
		Manifest: &Manifest{
			Failure: &struct {
				MaxRetries     int     `toml:"max_retries"`
				BackoffBase    float64 `toml:"backoff_base"`
				BackoffFactor  float64 `toml:"backoff_factor"`
				InitTimeout    float64 `toml:"init_timeout"`
				ExecTimeout    float64 `toml:"exec_timeout"`
				OnEventTimeout float64 `toml:"on_event_timeout"`
				OnCmdTimeout   float64 `toml:"on_cmd_timeout"`
			}{
				InitTimeout: 15,
			},
		},
	}

	timeouts := info.getTimeouts()
	if timeouts.Initialize != 15*1e9 {
		t.Errorf("expected Initialize 15s, got %v", timeouts.Initialize)
	}
	// Other timeouts should be defaults
	if timeouts.ExecuteTool != DefaultRPCTimeouts.ExecuteTool {
		t.Errorf("expected ExecuteTool default, got %v", timeouts.ExecuteTool)
	}
}

func TestGetTimeoutsNoManifest(t *testing.T) {
	// Test that PluginInfo.getTimeouts returns defaults when no manifest
	info := &PluginInfo{}
	timeouts := info.getTimeouts()
	if timeouts != DefaultRPCTimeouts {
		t.Errorf("expected default timeouts, got %v", timeouts)
	}
}

func TestRunCommand(t *testing.T) {
	manifestContent := `
[package]
name = "test-plugin"
version = "0.1.0"

[plugin]
api_version = "1"

[run]
command = "my-wrapper.sh --plugin"
`

	tmpDir := t.TempDir()
	manifestPath := tmpDir + "/tau-package.toml"
	if err := os.WriteFile(manifestPath, []byte(manifestContent), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := parseManifest(manifestPath)
	if err != nil {
		t.Fatalf("parseManifest failed: %v", err)
	}

	if !m.HasRunCommand() {
		t.Error("expected HasRunCommand() to be true")
	}
	if m.Run == nil || m.Run.Command != "my-wrapper.sh --plugin" {
		t.Errorf("expected run command 'my-wrapper.sh --plugin', got %q", m.Run.Command)
	}
}

func TestHasRunCommandFalse(t *testing.T) {
	manifestContent := `
[package]
name = "test-plugin"
version = "0.1.0"

[plugin]
api_version = "1"
`

	tmpDir := t.TempDir()
	manifestPath := tmpDir + "/tau-package.toml"
	if err := os.WriteFile(manifestPath, []byte(manifestContent), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := parseManifest(manifestPath)
	if err != nil {
		t.Fatalf("parseManifest failed: %v", err)
	}

	if m.HasRunCommand() {
		t.Error("expected HasRunCommand() to be false")
	}
}

func TestParseManifestConfigSchema(t *testing.T) {
	manifestContent := `
[package]
name = "test-plugin"
version = "0.1.0"

[plugin]
api_version = "1"

[config_schema]
schema = """{
  "type": "object",
  "properties": {
    "enabled": { "type": "boolean" },
    "paths": {
      "type": "array",
      "items": { "type": "string" }
    }
  },
  "required": ["enabled"]
}"""
`

	tmpDir := t.TempDir()
	manifestPath := tmpDir + "/tau-package.toml"
	if err := os.WriteFile(manifestPath, []byte(manifestContent), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := parseManifest(manifestPath)
	if err != nil {
		t.Fatalf("parseManifest failed: %v", err)
	}

	if m.ConfigSchema == nil {
		t.Fatal("expected config_schema section to be present")
	}
	if m.ConfigSchema.Schema == "" {
		t.Fatal("expected non-empty config_schema.schema")
	}

	var schemaDoc map[string]any
	if err := json.Unmarshal([]byte(m.ConfigSchema.Schema), &schemaDoc); err != nil {
		t.Fatalf("config schema is not valid JSON: %v", err)
	}

	if schemaDoc["type"] != "object" {
		t.Fatalf("schema type = %v, want object", schemaDoc["type"])
	}

	required, ok := schemaDoc["required"].([]any)
	if !ok || len(required) != 1 || required[0] != "enabled" {
		t.Fatalf("required = %#v, want [enabled]", schemaDoc["required"])
	}
}

func TestValidatePluginConfig(t *testing.T) {
	reg := commands.NewInMemoryRegistry()
	mgr := NewManager(t.TempDir(), ui.NewNoOpUI(), reg)

	schema := `{
  "type": "object",
  "properties": {
    "enabled": { "type": "boolean" },
    "root": { "type": "string" }
  },
  "required": ["enabled"]
}`

	t.Run("valid", func(t *testing.T) {
		mgr.SetSettings(&ai.Settings{
			Plugins: map[string]json.RawMessage{
				"test-plugin": json.RawMessage(`{"enabled":true,"root":"/tmp"}`),
			},
		})

		if err := mgr.validatePluginConfig("test-plugin", schema); err != nil {
			t.Fatalf("validatePluginConfig(valid): %v", err)
		}
	})

	t.Run("missing_required_field", func(t *testing.T) {
		mgr.SetSettings(&ai.Settings{
			Plugins: map[string]json.RawMessage{
				"test-plugin": json.RawMessage(`{"root":"/tmp"}`),
			},
		})

		err := mgr.validatePluginConfig("test-plugin", schema)
		if err == nil {
			t.Fatal("expected validation error for missing required field")
		}
		if !strings.Contains(err.Error(), "enabled") {
			t.Fatalf("expected error to mention enabled, got %v", err)
		}
	})

	t.Run("wrong_type", func(t *testing.T) {
		mgr.SetSettings(&ai.Settings{
			Plugins: map[string]json.RawMessage{
				"test-plugin": json.RawMessage(`{"enabled":"yes","root":"/tmp"}`),
			},
		})

		err := mgr.validatePluginConfig("test-plugin", schema)
		if err == nil {
			t.Fatal("expected validation error for wrong type")
		}
		if !strings.Contains(err.Error(), "enabled") {
			t.Fatalf("expected error to mention enabled, got %v", err)
		}
	})
}
