package tui

import (
	"encoding/json"
	"fmt"
	"strings"
)

// themeFile is the on-disk theme shape from app.md §11:
//
//	{ $schema?, name, vars?, colors, export? }
//
// Only the fields the terminal UI consumes are decoded. `export` is for HTML
// export and `$schema` is an editor hint, so neither is read here; they are
// declared so a strict decode does not reject a valid file for carrying them.
type themeFile struct {
	Schema string            `json:"$schema,omitempty"`
	Name   string            `json:"name"`
	Vars   map[string]string `json:"vars,omitempty"`
	Colors map[string]string `json:"colors"`
	Export map[string]string `json:"export,omitempty"`
}

// DecodeThemeColors reads a theme file and returns its colour tokens, with
// `vars` references resolved.
//
// Values are returned as strings rather than parsed colours because
// themeTokens.withOverrides owns parsing: it already skips values it cannot
// read, so a single bad colour costs one token instead of the whole file.
func DecodeThemeColors(raw []byte) (map[string]string, error) {
	var file themeFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("theme: parse: %w", err)
	}
	if len(file.Colors) == 0 {
		return nil, fmt.Errorf("theme %q: no colors object", file.Name)
	}

	out := make(map[string]string, len(file.Colors))
	for token, value := range file.Colors {
		out[token] = resolveThemeVar(value, file.Vars)
	}
	return out, nil
}

// resolveThemeVar expands a `vars` reference. A value that names a var is
// replaced by it; anything else passes through untouched. Both bare (`accent`)
// and sigil (`$accent`, `{accent}`) spellings are accepted because the spec
// describes `vars` as "an optional named-value mechanism" without fixing a
// syntax, and rejecting a plausible spelling would silently drop the colour.
//
// Resolution is one level deep on purpose: a var whose value names another var
// is left alone rather than followed, so a cycle cannot hang the render path.
func resolveThemeVar(value string, vars map[string]string) string {
	if len(vars) == 0 {
		return value
	}
	key := strings.TrimSpace(value)
	key = strings.TrimPrefix(key, "$")
	key = strings.TrimPrefix(key, "{")
	key = strings.TrimSuffix(key, "}")
	if resolved, ok := vars[key]; ok {
		return resolved
	}
	return value
}

// ThemeColorLookup resolves a theme name to its colour tokens. It is how
// discovered theme files reach the UI: the application layer owns discovery and
// precedence (see agent/artifacts), and hands the terminal UI a lookup rather
// than the registry, so this package stays independent of the resource loader.
//
// Returning false means "no such theme" — the caller keeps the built-in tokens
// for that name rather than blanking the UI.
type ThemeColorLookup func(name string) (map[string]string, bool)

// themeColorsFor resolves a theme's overrides through the configured lookup.
// A nil lookup, an unknown name, or an empty result all mean "use the built-in
// tokens unchanged".
func (a *App) themeColorsFor(name string) map[string]string {
	if a == nil || a.themeColors == nil {
		return nil
	}
	values, ok := a.themeColors(name)
	if !ok {
		return nil
	}
	return values
}
