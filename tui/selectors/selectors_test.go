package selectors

import "testing"

func TestSessionsSelector(t *testing.T) {
	got := Sessions([]Session{{ID: "1", Name: "Project", Path: "/tmp/p", Updated: "today"}, {ID: "2", Name: "Other"}}, "proj")
	if len(got) != 1 || got[0].Value != "1" || got[0].Description != "/tmp/p · today" {
		t.Fatalf("got=%+v", got)
	}
}

func TestModelsSelectorMarksCurrentAndCapabilities(t *testing.T) {
	got := Models([]Model{{ID: "gpt", Provider: "openai", Description: "fast", SupportsTools: true, SupportsVision: true, MaxThinking: "medium"}, {ID: "claude", Provider: "anthropic"}}, "", "gpt")
	if !got[0].Selected || got[0].Description != "openai · fast · tools/vision/thinking medium" {
		t.Fatalf("got=%+v", got)
	}
}

func TestToolsSelectorAndToggle(t *testing.T) {
	tools := []Tool{{Name: "read", Description: "Read files", Enabled: true}, {Name: "bash", Description: "Run shell"}}
	got := Tools(tools, "shell")
	if len(got) != 1 || got[0].Value != "bash" || got[0].Selected {
		t.Fatalf("got=%+v", got)
	}
	toggled := ToggleTool(tools, "bash")
	if !toggled[1].Enabled || tools[1].Enabled {
		t.Fatalf("toggle failed toggled=%+v tools=%+v", toggled, tools)
	}
}

func TestThemesSelector(t *testing.T) {
	got := Themes([]Theme{{Name: "dark", Description: "Default", Dark: true}, {Name: "light", Description: "Bright"}}, "bright", "light")
	if len(got) != 1 || got[0].Value != "light" || !got[0].Selected || got[0].Description != "light · Bright" {
		t.Fatalf("got=%+v", got)
	}
}
