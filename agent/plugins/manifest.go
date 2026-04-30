package plugins

import (
	"os"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Manifest represents the tau-package.toml manifest file.
type Manifest struct {
	Package struct {
		Name        string `toml:"name"`
		Version     string `toml:"version"`
		Description string `toml:"description"`
	} `toml:"package"`
	Plugin *struct {
		APIVersion string `toml:"api_version"`
		ToolPrefix string `toml:"tool_prefix"`
	} `toml:"plugin"`
	Failure *struct {
		MaxRetries     int     `toml:"max_retries"`
		BackoffBase    float64 `toml:"backoff_base"`
		BackoffFactor  float64 `toml:"backoff_factor"`
		InitTimeout    float64 `toml:"init_timeout"`
		ExecTimeout    float64 `toml:"exec_timeout"`
		OnEventTimeout float64 `toml:"on_event_timeout"`
		OnCmdTimeout   float64 `toml:"on_cmd_timeout"`
	} `toml:"failure"`
	Run          *RunSection   `toml:"run"`
	ConfigSchema *ConfigSchema `toml:"config_schema"`
}

// RunSection defines how to launch the plugin.
type RunSection struct {
	Command string `toml:"command"`
}

// HasRunCommand returns true if the manifest specifies a run command.
func (m *Manifest) HasRunCommand() bool {
	return m.Run != nil && m.Run.Command != ""
}

// ConfigSchema holds an optional JSON Schema for plugin configuration validation.
type ConfigSchema struct {
	// Schema is the JSON Schema document as a string.
	Schema string `toml:"schema"`
}

// parseManifest reads and parses a tau-package.toml file.
func parseManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var m Manifest
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, err
	}

	return &m, nil
}

// getTimeout gets a timeout value from the manifest's failure config.
func (m *Manifest) getTimeout(field string, defaultValue time.Duration) time.Duration {
	if m.Failure == nil {
		return defaultValue
	}
	switch field {
	case "init":
		if m.Failure.InitTimeout > 0 {
			return time.Duration(m.Failure.InitTimeout * float64(time.Second))
		}
	case "exec":
		if m.Failure.ExecTimeout > 0 {
			return time.Duration(m.Failure.ExecTimeout * float64(time.Second))
		}
	case "on_event":
		if m.Failure.OnEventTimeout > 0 {
			return time.Duration(m.Failure.OnEventTimeout * float64(time.Second))
		}
	case "on_cmd":
		if m.Failure.OnCmdTimeout > 0 {
			return time.Duration(m.Failure.OnCmdTimeout * float64(time.Second))
		}
	}
	return defaultValue
}

// getFailureConfig returns a FailureConfig from the manifest.
func (m *Manifest) getFailureConfig() FailureConfig {
	cfg := DefaultFailureConfig
	if m.Failure == nil {
		return cfg
	}
	if m.Failure.MaxRetries > 0 {
		cfg.MaxAttempts = m.Failure.MaxRetries
	}
	if m.Failure.BackoffBase > 0 {
		cfg.BackoffBase = time.Duration(m.Failure.BackoffBase * float64(time.Second))
	}
	if m.Failure.BackoffFactor > 0 {
		cfg.BackoffFactor = m.Failure.BackoffFactor
	}
	return cfg
}
