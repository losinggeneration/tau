// Package mcp connects the Model Context Protocol servers configured in
// mcp.json and adapts their tools to agent tools.
package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Exposure says how a server's tools reach the model.
type Exposure string

const (
	// ExposureCodemode tools are callable from codemode scripts only.
	ExposureCodemode Exposure = "codemode"
	// ExposureDeferred tools are declared once tool search loads them.
	ExposureDeferred Exposure = "deferred"
	// ExposureDirect tools are declared to the model like built-in tools.
	ExposureDirect Exposure = "direct"
	// ExposureHidden tools are registered but unreachable.
	ExposureHidden Exposure = "hidden"
)

var exposures = []Exposure{ExposureCodemode, ExposureDeferred, ExposureDirect, ExposureHidden}

// ToolExposure overrides the exposure of one tool name or `*` pattern.
type ToolExposure struct {
	Pattern  string
	Exposure Exposure
}

// ServerConfig is one entry of mcpServers.
type ServerConfig struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	CWD     string            `json:"cwd,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	OAuth   json.RawMessage   `json:"oauth,omitempty"`
	Auth    *struct {
		Provider string `json:"provider"`
	} `json:"auth,omitempty"`
	Exposure    Exposure `json:"exposure,omitempty"`
	Description string   `json:"description,omitempty"`
	Enabled     *bool    `json:"enabled,omitempty"`
	// Timeout is the per-request timeout in seconds; progress renews it.
	Timeout float64 `json:"timeout,omitempty"`
	// ToolExposure keeps the file's key order, since the first matching
	// pattern wins.
	ToolExposure []ToolExposure `json:"-"`
}

// IsEnabled reports whether the server should connect.
func (c ServerConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// Exposures returns the exposures the server's tools can have, known from
// the config before the server connects.
func (c ServerConfig) Exposures() []Exposure {
	exposures := []Exposure{c.Exposure}
	if c.Exposure == "" {
		exposures[0] = ExposureCodemode
	}
	for _, o := range c.ToolExposure {
		if !slices.Contains(exposures, o.Exposure) {
			exposures = append(exposures, o.Exposure)
		}
	}
	return exposures
}

// ToolExposureFor returns a tool's exposure: an exact toolExposure key, then
// the first matching pattern, then the server exposure (default codemode).
func (c ServerConfig) ToolExposureFor(tool string) Exposure {
	for _, o := range c.ToolExposure {
		if o.Pattern == tool {
			return o.Exposure
		}
	}
	for _, o := range c.ToolExposure {
		if strings.Contains(o.Pattern, "*") && patternRegexp(o.Pattern).MatchString(tool) {
			return o.Exposure
		}
	}
	if c.Exposure == "" {
		return ExposureCodemode
	}
	return c.Exposure
}

func patternRegexp(pattern string) *regexp.Regexp {
	parts := strings.Split(pattern, "*")
	for i, part := range parts {
		parts[i] = regexp.QuoteMeta(part)
	}
	return regexp.MustCompile("^" + strings.Join(parts, ".*") + "$")
}

// Scope says which mcp.json defined a server.
type Scope string

const (
	ScopeGlobal  Scope = "global"
	ScopeProject Scope = "project"
)

// ServerEntry is a configured server with where it came from.
type ServerEntry struct {
	Name   string
	Config ServerConfig
	Source string
	Scope  Scope
	// Override is the project mcp.json that overrides this global server's
	// enabled, exposure or toolExposure.
	Override string
	raw      map[string]json.RawMessage
}

// Config is the merged MCP configuration.
type Config struct {
	Servers []ServerEntry
	// AutoEnableCodemode activates codemode when codemode servers connect.
	// Nil means the default, true.
	AutoEnableCodemode *bool
	// Errors lists problems with files and entries; bad entries are skipped.
	Errors []string
	// ProjectConfig is the trusted project's mcp.json, where /mcp saves
	// project overrides.
	ProjectConfig string
}

var (
	serverNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	overrideKeys = []string{"enabled", "exposure", "toolExposure"}
)

// Namespace returns the tool namespace of a server, mcp__<server> with `-`
// replaced by `_`.
func Namespace(server string) string {
	return "mcp__" + strings.ReplaceAll(server, "-", "_")
}

// LoadConfig reads mcp.json from agentDir and, when trusted, .tau/mcp.json
// under cwd. Project entries replace global ones of the same name.
func LoadConfig(agentDir, cwd string, projectTrusted bool) Config {
	var cfg Config
	byName := map[string]int{}
	readConfigFile(filepath.Join(agentDir, "mcp.json"), ScopeGlobal, &cfg, byName)
	if projectTrusted {
		cfg.ProjectConfig = filepath.Join(cwd, ".tau", "mcp.json")
		readConfigFile(cfg.ProjectConfig, ScopeProject, &cfg, byName)
	}
	return cfg
}

func readConfigFile(path string, scope Scope, cfg *Config, byName map[string]int) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		cfg.Errors = append(cfg.Errors, fmt.Sprintf("%s: %v", path, err))
		return
	}
	var file map[string]json.RawMessage
	if err := json.Unmarshal(data, &file); err != nil {
		cfg.Errors = append(cfg.Errors, fmt.Sprintf("%s: %v", path, err))
		return
	}
	names, servers, ok := orderedObject(file["mcpServers"])
	if file == nil || !ok {
		cfg.Errors = append(cfg.Errors, fmt.Sprintf(`%s: expected an object with an "mcpServers" object`, path))
		return
	}
	if auto, ok := file["autoEnableCodemode"]; ok {
		var value bool
		if err := json.Unmarshal(auto, &value); err != nil {
			cfg.Errors = append(cfg.Errors, path+": autoEnableCodemode must be a boolean")
		} else {
			cfg.AutoEnableCodemode = &value
		}
	}
	for _, name := range names {
		var raw map[string]json.RawMessage
		if json.Unmarshal(servers[name], &raw) != nil {
			raw = nil
		}
		if scope == ScopeProject && raw != nil && isOverride(raw) {
			if err := applyOverride(cfg, byName, name, raw, path); err != "" {
				cfg.Errors = append(cfg.Errors, err)
			}
			continue
		}
		config, err := validateServer(name, raw)
		if err != nil {
			cfg.Errors = append(cfg.Errors, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		if clash := clashingServer(cfg.Servers, name); clash != "" {
			cfg.Errors = append(cfg.Errors, fmt.Sprintf("%s: server %q conflicts with %q", path, name, clash))
			continue
		}
		if scope == ScopeProject && config.URL != "" && config.Auth != nil {
			cfg.Errors = append(cfg.Errors, fmt.Sprintf("%s: server %q: auth is only allowed in the global mcp.json", path, name))
			continue
		}
		entry := ServerEntry{Name: name, Config: config, Source: path, Scope: scope, raw: raw}
		if i, found := byName[name]; found {
			cfg.Servers[i] = entry
		} else {
			byName[name] = len(cfg.Servers)
			cfg.Servers = append(cfg.Servers, entry)
		}
	}
}

// applyOverride merges a project entry that sets only enabled, exposure or
// toolExposure into the global server of the same name, keeping the rest of
// the global entry, credentials included.
func applyOverride(cfg *Config, byName map[string]int, name string, raw map[string]json.RawMessage, path string) string {
	i, found := byName[name]
	if !found {
		return fmt.Sprintf(`%s: server %q needs "command" or "url", or a global server to override`, path, name)
	}
	for key := range raw {
		if !slices.Contains(overrideKeys, key) {
			return fmt.Sprintf("%s: server %q: an override can only set %s", path, name, strings.Join(overrideKeys, ", "))
		}
	}
	base := cfg.Servers[i]
	merged := maps.Clone(base.raw)
	maps.Copy(merged, raw)
	config, err := validateServer(name, merged)
	if err != nil {
		return fmt.Sprintf("%s: %v", path, err)
	}
	base.Config, base.raw, base.Override = config, merged, path
	cfg.Servers[i] = base
	return ""
}

func clashingServer(servers []ServerEntry, name string) string {
	for _, other := range servers {
		if other.Name != name && Namespace(other.Name) == Namespace(name) {
			return other.Name
		}
	}
	return ""
}

func isOverride(raw map[string]json.RawMessage) bool {
	_, command := raw["command"]
	_, u := raw["url"]
	_, typ := raw["type"]
	return !command && !u && !typ
}

// orderedObject decodes a JSON object keeping its key order. A missing value
// is an empty object.
func orderedObject(data json.RawMessage) ([]string, map[string]json.RawMessage, bool) {
	if len(data) == 0 {
		return nil, nil, true
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, nil, false
	}
	var names []string
	values := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, nil, false
		}
		key, _ := tok.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, nil, false
		}
		if _, seen := values[key]; !seen {
			names = append(names, key)
		}
		values[key] = value
	}
	return names, values, true
}

var loopbackHosts = []string{"localhost", "127.0.0.1", "::1"}

func validateServer(name string, raw map[string]json.RawMessage) (ServerConfig, error) {
	var cfg ServerConfig
	if !serverNameRe.MatchString(name) {
		return cfg, fmt.Errorf(`invalid server name %q (use letters, digits, "_" and "-")`, name)
	}
	if raw == nil {
		return cfg, fmt.Errorf("server %q must be an object", name)
	}
	field := func(key string, into any, want string) error {
		v, ok := raw[key]
		if !ok {
			return nil
		}
		if err := json.Unmarshal(v, into); err != nil {
			return fmt.Errorf("server %q: %s must be %s", name, key, want)
		}
		return nil
	}
	exposureList := `"codemode", "deferred", "direct", "hidden"`
	for _, f := range []struct {
		key, want string
		into      any
	}{
		{"type", "a string", &cfg.Type},
		{"enabled", "a boolean", &cfg.Enabled},
		{"description", "a string", &cfg.Description},
		{"timeout", "a positive number of seconds", &cfg.Timeout},
		{"exposure", "one of " + exposureList, &cfg.Exposure},
		{"url", "a string", &cfg.URL},
		{"command", "a string", &cfg.Command},
	} {
		if err := field(f.key, f.into, f.want); err != nil {
			return cfg, err
		}
	}
	cfg.Exposure = resolveExposure(cfg.Exposure)
	if cfg.Exposure != "" && !slices.Contains(exposures, cfg.Exposure) {
		return cfg, fmt.Errorf("server %q: exposure must be one of %s", name, exposureList)
	}
	if v, ok := raw["toolExposure"]; ok {
		keys, values, isObject := orderedObject(v)
		if !isObject {
			return cfg, fmt.Errorf("server %q: toolExposure must map tool names to exposures", name)
		}
		for _, key := range keys {
			var e Exposure
			if json.Unmarshal(values[key], &e) != nil || !slices.Contains(exposures, resolveExposure(e)) {
				return cfg, fmt.Errorf("server %q: toolExposure %q must be one of %s", name, key, exposureList)
			}
			cfg.ToolExposure = append(cfg.ToolExposure, ToolExposure{Pattern: key, Exposure: resolveExposure(e)})
		}
	}
	if _, ok := raw["timeout"]; ok && cfg.Timeout <= 0 {
		return cfg, fmt.Errorf("server %q: timeout must be a positive number of seconds", name)
	}
	if cfg.Type == "sse" {
		return cfg, fmt.Errorf("server %q: legacy SSE transport is not supported; use the streamable HTTP URL", name)
	}
	switch {
	case cfg.URL != "" && (cfg.Type == "" || cfg.Type == "http" || cfg.Type == "streamable-http"):
		u, err := url.Parse(cfg.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return cfg, fmt.Errorf("server %q: url must be an http or https URL", name)
		}
		if err := field("headers", &cfg.Headers, "a map of names to strings"); err != nil {
			return cfg, fmt.Errorf("server %q: headers must map names to strings", name)
		}
		cfg.OAuth = raw["oauth"]
		if err := field("auth", &cfg.Auth, "an object"); err != nil || (cfg.Auth != nil && cfg.Auth.Provider == "") {
			return cfg, fmt.Errorf("server %q: auth.provider must be a provider name", name)
		}
		if cfg.Auth != nil && u.Scheme != "https" && !slices.Contains(loopbackHosts, u.Hostname()) {
			return cfg, fmt.Errorf("server %q: auth requires an https URL, or http on localhost, 127.0.0.1, or [::1]", name)
		}
		return cfg, nil
	case cfg.Command != "" && (cfg.Type == "" || cfg.Type == "stdio"):
		if err := field("args", &cfg.Args, "an array of strings"); err != nil {
			return cfg, fmt.Errorf("server %q: args must be an array of strings", name)
		}
		if err := field("env", &cfg.Env, "a map"); err != nil {
			return cfg, fmt.Errorf("server %q: env must map names to strings", name)
		}
		if err := field("cwd", &cfg.CWD, "a string"); err != nil {
			return cfg, err
		}
		return cfg, nil
	}
	return cfg, fmt.Errorf(`server %q needs either "command" (stdio) or "url" (streamable HTTP)`, name)
}

func resolveExposure(e Exposure) Exposure {
	if e == "codemode-deferred" {
		return ExposureCodemode
	}
	return e
}

// ConfigPatch is a change /mcp saves to a server's mcp.json. Enabled true and
// exposure codemode are the defaults and remove their key, except in project
// overrides, which replace the global server's values.
type ConfigPatch struct {
	Enabled  *bool
	Exposure Exposure
	// Project saves the change as an override in the trusted project's
	// mcp.json instead of the file that defines the server.
	Project bool
}

var indentRe = regexp.MustCompile(`(?m)^([ \t]+)\S`)

// updateServerConfig applies patch to server name in the mcp.json at path,
// keeping the file's other content, key order and indentation. With create, a
// missing entry is added as an override.
func updateServerConfig(path, name string, patch ConfigPatch, create bool) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}")
	}
	keys, file, ok := orderedObject(data)
	if !ok {
		return fmt.Errorf(`%s: expected an object with an "mcpServers" object`, path)
	}
	names, servers, ok := orderedObject(file["mcpServers"])
	if !ok {
		return fmt.Errorf(`%s: expected an object with an "mcpServers" object`, path)
	}
	if _, found := servers[name]; !found {
		if !create {
			return fmt.Errorf("%s does not define MCP server %q", path, name)
		}
		if servers == nil {
			servers = map[string]json.RawMessage{}
		}
		names, servers[name] = append(names, name), json.RawMessage("{}")
	}
	fields, values, ok := orderedObject(servers[name])
	if !ok {
		return fmt.Errorf("%s does not define MCP server %q", path, name)
	}
	keepDefaults := isOverride(values)
	set := func(key string, value any, isDefault bool) {
		if isDefault && !keepDefaults {
			fields = slices.DeleteFunc(fields, func(f string) bool { return f == key })
			delete(values, key)
			return
		}
		if _, found := values[key]; !found {
			fields = append(fields, key)
		}
		values[key], _ = json.Marshal(value) // bool and string values always marshal
	}
	if patch.Enabled != nil {
		set("enabled", *patch.Enabled, *patch.Enabled)
	}
	if patch.Exposure != "" {
		set("exposure", patch.Exposure, patch.Exposure == ExposureCodemode)
	}
	servers[name] = encodeOrdered(fields, values)
	if _, found := file["mcpServers"]; !found {
		keys = append(keys, "mcpServers")
	}
	file["mcpServers"] = encodeOrdered(names, servers)
	indent := "  "
	if m := indentRe.FindSubmatch(data); m != nil {
		indent = string(m[1])
	}
	var out bytes.Buffer
	if err := json.Indent(&out, encodeOrdered(keys, file), "", indent); err != nil {
		return err
	}
	out.WriteByte('\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, out.Bytes(), 0o644)
}

func encodeOrdered(keys []string, values map[string]json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		name, _ := json.Marshal(key) // strings always marshal
		b.Write(name)
		b.WriteByte(':')
		b.Write(values[key])
	}
	b.WriteByte('}')
	return b.Bytes()
}
