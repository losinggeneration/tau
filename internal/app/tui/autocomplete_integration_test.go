package tui

import (
	"testing"

	"github.com/losinggeneration/tau/tui/autocomplete"
)

func TestAutocompleteProviderIncludesLocalArgumentCallbacks(t *testing.T) {
	a := newAppWithSelector(t)
	var prefixes []string
	a.cmds.Register(Command{Name: "inspect", CompleteArgs: func(app *App, prefix string) []autocomplete.Item {
		if app != a {
			t.Fatal("wrong app")
		}
		prefixes = append(prefixes, prefix)
		return []autocomplete.Item{{Value: "目标", Label: "目标"}}
	}})
	p, err := a.buildAutocompleteProvider()
	if err != nil {
		t.Fatal(err)
	}
	line := "  /inspect 目"
	got, err := p.GetSuggestions(t.Context(), []string{line}, 0, len(line), false)
	if err != nil || got == nil || got.Prefix != "目" || len(got.Items) != 1 || got.Items[0].Value != "目标" || len(prefixes) != 1 || prefixes[0] != "目" {
		t.Fatalf("got=%+v err=%v calls=%q", got, err, prefixes)
	}
}
