// Package artifacts provides unified discovery for skills, prompt templates,
// and themes. This file contains the built-in themes.
package artifacts // import "github.com/losinggeneration/tau/agent/artifacts"

// BuiltinThemes returns the built-in themes: dark and light.
func BuiltinThemes() []Entry {
	return []Entry{
		{
			Tier:       TierBuiltin,
			Kind:       KindTheme,
			SourcePath: "builtin:dark",
			Raw:        []byte(`{"name": "dark"}`),
		},
		{
			Tier:       TierBuiltin,
			Kind:       KindTheme,
			SourcePath: "builtin:light",
			Raw:        []byte(`{"name": "light"}`),
		},
	}
}
