package artifacts

import (
	"strings"
	"testing"
)

func TestFormatSkillInvocation_NoArgs(t *testing.T) {
	s := &Skill{
		Name:       "calendar",
		SourcePath: "/tmp/skills/calendar/SKILL.md",
		BaseDir:    "/tmp/skills/calendar",
		Body:       []byte("Use the calendar tool to schedule meetings."),
	}
	got := FormatSkillInvocation(s, "")
	want := `<skill name="calendar" location="/tmp/skills/calendar/SKILL.md">
References are relative to /tmp/skills/calendar.

Use the calendar tool to schedule meetings.
</skill>`
	if got != want {
		t.Errorf("FormatSkillInvocation no-args:\ngot:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestFormatSkillInvocation_WithArgs(t *testing.T) {
	s := &Skill{
		Name:       "calendar",
		SourcePath: "/tmp/skills/calendar/SKILL.md",
		BaseDir:    "/tmp/skills/calendar",
		Body:       []byte("body"),
	}
	got := FormatSkillInvocation(s, "schedule a 1:1 with Bob")
	if !strings.HasSuffix(got, "</skill>\n\nschedule a 1:1 with Bob") {
		t.Errorf("args not appended after blank line: %q", got)
	}
	if !strings.Contains(got, `<skill name="calendar"`) {
		t.Errorf("XML wrapper missing: %q", got)
	}
}

func TestFormatSkillInvocation_EscapesNameAndLocation(t *testing.T) {
	s := &Skill{
		Name:       `weird"name`,
		SourcePath: "/tmp/<weird>/SKILL.md",
		BaseDir:    "/tmp/<weird>",
		Body:       []byte("body"),
	}
	got := FormatSkillInvocation(s, "")
	if !strings.Contains(got, `name="weird&quot;name"`) {
		t.Errorf("name not escaped: %q", got)
	}
	if !strings.Contains(got, `location="/tmp/&lt;weird&gt;/SKILL.md"`) {
		t.Errorf("location not escaped: %q", got)
	}
}

func TestFormatSkillsCatalog_Empty(t *testing.T) {
	if got := FormatSkillsCatalog(nil); got != "" {
		t.Errorf("empty catalog should be empty string, got %q", got)
	}
	if got := FormatSkillsCatalog([]*Skill{}); got != "" {
		t.Errorf("empty catalog should be empty string, got %q", got)
	}
}

func TestFormatSkillsCatalog_FiltersDisabled(t *testing.T) {
	skills := []*Skill{
		{Name: "a", Description: "A", SourcePath: "/a.md"},
		{Name: "b", Description: "B", SourcePath: "/b.md", DisableModelInvocation: true},
		{Name: "c", Description: "C", SourcePath: "/c.md"},
	}
	got := FormatSkillsCatalog(skills)
	if !strings.Contains(got, "<name>a</name>") {
		t.Error("a should be present")
	}
	if strings.Contains(got, "<name>b</name>") {
		t.Error("b should be filtered (disableModelInvocation)")
	}
	if !strings.Contains(got, "<name>c</name>") {
		t.Error("c should be present")
	}
	if !strings.HasPrefix(got, "\n\nThe following skills") {
		t.Errorf("preamble missing: %q", got[:80])
	}
	if !strings.Contains(got, "<available_skills>") || !strings.Contains(got, "</available_skills>") {
		t.Errorf("wrapper tags missing")
	}
}

func TestFormatSkillsCatalog_AllDisabledReturnsEmpty(t *testing.T) {
	skills := []*Skill{
		{Name: "a", Description: "A", DisableModelInvocation: true},
	}
	if got := FormatSkillsCatalog(skills); got != "" {
		t.Errorf("all-disabled catalog should be empty, got %q", got)
	}
}

func TestFormatSkillsCatalog_EscapesFields(t *testing.T) {
	skills := []*Skill{
		{Name: "a&b", Description: "<desc>", SourcePath: `/p"x.md`},
	}
	got := FormatSkillsCatalog(skills)
	if !strings.Contains(got, "<name>a&amp;b</name>") {
		t.Errorf("name not escaped: %q", got)
	}
	if !strings.Contains(got, "<description>&lt;desc&gt;</description>") {
		t.Errorf("description not escaped: %q", got)
	}
	if !strings.Contains(got, `<location>/p&quot;x.md</location>`) {
		t.Errorf("location not escaped: %q", got)
	}
}
