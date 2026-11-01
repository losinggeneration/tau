package tui

import (
	"context"
	"strings"

	"github.com/losinggeneration/tau/tui/autocomplete"
)

// newAutocompleteCommandProvider returns a SlashCommand slice the
// CombinedProvider can consume, sourced from the app's local + session
// command registries. The returned slice is freshly allocated; the
// CombinedProvider copies it via WithCommands, so subsequent edits to
// the app's registry don't leak through to the provider until the
// caller re-derives a new slice.
func newAutocompleteCommandProvider(app *App) []autocomplete.SlashCommand {
	if app == nil {
		return nil
	}
	local := app.cmds.All()
	out := make([]autocomplete.SlashCommand, 0, len(local)+8)
	for _, c := range local {
		command := autocomplete.SlashCommand{Name: c.Name, Description: c.Description}
		if c.CompleteArgs != nil {
			command.CompleteArgs = func(ctx context.Context, prefix string) ([]autocomplete.Item, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return c.CompleteArgs(app, prefix), nil
			}
		}
		out = append(out, command)
	}
	for _, ext := range app.externalCommands() {
		out = append(out, autocomplete.SlashCommand{
			Name:         ext.Name,
			Description:  ext.Description,
			ArgumentHint: ext.ArgumentHint,
		})
	}
	return out
}

// buildAutocompleteProvider returns a CombinedProvider seeded with
// the app's interactive + session command registries. The base path
// is the session's CWD when available, falling back to ".". The
// returned provider is independent of the app's other state — callers
// that want updates (e.g. a plugin registering a new command) should
// re-call buildAutocompleteProvider and use the new instance.
//
// The function is safe to call on the app loop; it does not mutate
// any of the App's owned state. It returns nil and the underlying
// error when the base path cannot be determined.
func (a *App) buildAutocompleteProvider() (*autocomplete.CombinedProvider, error) {
	if a == nil {
		return nil, nil
	}
	base := a.fileSelectorRoot()
	if base == "" {
		base = "."
	}
	commands := newAutocompleteCommandProvider(a)
	return autocomplete.NewCombinedProvider(
		autocomplete.WithBasePath(base),
		autocomplete.WithCommands(commands),
	)
}

// autocompleteCompleteItem returns the autocomplete.Item the
// CombinedProvider would apply for the given (line, col, prefix)
// triple. Exposed for tests and the popup rendering path so the
// selector can call ApplyCompletion without re-implementing it.
func (a *App) autocompleteCompleteItem(text string, cursorLine, cursorCol int, prefix, value string) (string, int, bool) {
	provider, err := a.buildAutocompleteProvider()
	if err != nil || provider == nil {
		return "", 0, false
	}
	suggestions, err := provider.GetSuggestions(stubCtxOrBackground(), []string{text}, cursorLine, cursorCol, false)
	if err != nil || suggestions == nil || suggestions.Prefix != prefix {
		return "", 0, false
	}
	for _, item := range suggestions.Items {
		if item.Value == value {
			res := autocomplete.ApplyCompletion([]string{text}, cursorLine, cursorCol, item, prefix)
			if len(res.Lines) == 0 {
				return "", 0, false
			}
			return res.Lines[cursorLine], res.CursorCol, true
		}
	}
	return "", 0, false
}

// stubCtxOrBackground returns a fresh context the autocomplete
// provider can use without referencing the app's mutable ctx.
func stubCtxOrBackground() context.Context {
	return context.Background()
}

// completionPrefixFromLine extracts the slash-command prefix the
// CombinedProvider expects to match. Returns the empty string when
// the line does not start with "/" or contains whitespace that
// would push the cursor into argument completion territory.
func completionPrefixFromLine(line string, col int) string {
	if col < 0 || col > len(line) {
		return ""
	}
	if !strings.HasPrefix(line, "/") {
		return ""
	}
	before := line[:col]
	if strings.Contains(before, " ") {
		return ""
	}
	return before[1:]
}
