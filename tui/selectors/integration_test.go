package selectors

import "testing"

func TestStoreStateSelectorIntegration(t *testing.T) {
	state := StoreState{
		Sessions: []Session{{ID: "old", Updated: "2024-01-01"}, {ID: "new", Updated: "2024-02-01"}},
		Models:   []Model{{ID: "m1"}, {ID: "m2"}},
		Tools:    []Tool{{Name: "bash", Enabled: false}},
		Themes:   []Theme{{Name: "dark", Dark: true}},
		Model:    "m2",
		Theme:    "dark",
	}
	if got := state.SessionItems(""); len(got) != 2 || got[0].Value != "new" {
		t.Fatalf("SessionItems = %+v, want newest first", got)
	}
	if got := state.ModelItems(""); !got[1].Selected {
		t.Fatalf("ModelItems = %+v, want current selected", got)
	}
	state = state.WithToolToggled("bash")
	if got := state.ToolItems(""); len(got) != 1 || !got[0].Selected {
		t.Fatalf("ToolItems = %+v, want toggled on", got)
	}
	if got := state.ThemeItems(""); len(got) != 1 || !got[0].Selected {
		t.Fatalf("ThemeItems = %+v, want current theme selected", got)
	}
}
