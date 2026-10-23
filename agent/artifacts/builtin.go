// Package artifacts provides unified discovery for skills, prompt templates,
// and themes. This file contains the built-in themes.
package artifacts // import "github.com/losinggeneration/tau/agent/artifacts"

import _ "embed"

var (
	//go:embed themes/dark.json
	builtinDarkTheme []byte
	//go:embed themes/light.json
	builtinLightTheme []byte
)

// BuiltinThemes returns the built-in dark and light themes.
func BuiltinThemes() []Entry {
	return []Entry{
		{
			Tier:       TierBuiltin,
			Kind:       KindTheme,
			SourcePath: "builtin:dark",
			Raw:        builtinDarkTheme,
		},
		{
			Tier:       TierBuiltin,
			Kind:       KindTheme,
			SourcePath: "builtin:light",
			Raw:        builtinLightTheme,
		},
	}
}
