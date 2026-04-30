package artifacts

import (
	"context"
	"strings"
	"testing"

	"github.com/losinggeneration/tau/commands"
)

// TestLoader_SkillCommandHandlerReturnsXMLWrappedBody verifies that the
// /skill:<id> slash handler returns the FormatSkillInvocation output, not
// the raw skill body. This is the contract callers (TUI, RPC, headless print
// mode) rely on to produce the same prompt shape as the runtime path.
func TestLoader_SkillCommandHandlerReturnsXMLWrappedBody(t *testing.T) {
	skill := &Skill{
		ID:          "calendar",
		Name:        "calendar",
		Description: "schedule things",
		Body:        []byte("Use the calendar tool."),
		SourcePath:  "/tmp/skills/calendar/SKILL.md",
		BaseDir:     "/tmp/skills/calendar",
		Tier:        TierUser,
	}
	skills := NewSkillRegistry(nil)
	skills.skills[skill.ID] = skill
	skills.rebuildOrdered()

	reg := commands.NewInMemoryRegistry()
	l := &Loader{
		skills:           skills,
		prompts:          NewPromptRegistry(nil),
		themes:           NewThemeRegistry(nil),
		commandsRegistry: reg,
		commandSources:   map[string]struct{}{},
	}
	l.registerCommands()

	h, _, ok := reg.Lookup("skill:calendar")
	if !ok {
		t.Fatal("/skill:calendar not registered")
	}
	res, err := h(context.Background(), "do thing", nil, nil)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !strings.HasPrefix(res.Output, `<skill name="calendar"`) {
		t.Errorf("output should start with XML wrapper, got: %q", truncate(res.Output, 80))
	}
	if !strings.Contains(res.Output, "Use the calendar tool.") {
		t.Error("body missing from output")
	}
	if !strings.HasSuffix(res.Output, "</skill>\n\ndo thing") {
		t.Errorf("args not appended after blank line; got: %q", res.Output)
	}
}

func TestLoader_SkillCommandHandlerNoArgs(t *testing.T) {
	skill := &Skill{
		ID:         "x",
		Name:       "x",
		Body:       []byte("body"),
		SourcePath: "/p/SKILL.md",
		BaseDir:    "/p",
		Tier:       TierUser,
	}
	skills := NewSkillRegistry(nil)
	skills.skills[skill.ID] = skill
	skills.rebuildOrdered()

	reg := commands.NewInMemoryRegistry()
	l := &Loader{
		skills:           skills,
		prompts:          NewPromptRegistry(nil),
		themes:           NewThemeRegistry(nil),
		commandsRegistry: reg,
		commandSources:   map[string]struct{}{},
	}
	l.registerCommands()

	h, _, _ := reg.Lookup("skill:x")
	res, _ := h(context.Background(), "", nil, nil)
	if !strings.HasSuffix(res.Output, "</skill>") {
		t.Errorf("no-args output should end at </skill>, got: %q", res.Output)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
