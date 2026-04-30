package artifacts

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSkill_BaseDirForLooseFile(t *testing.T) {
	entry := Entry{
		Kind:       KindSkill,
		SourcePath: "/tmp/skills/foo.md",
		Raw:        []byte("---\nname: foo\ndescription: x\n---\nbody"),
		Tier:       TierUser,
	}
	s, err := parseSkill(entry)
	if err != nil {
		t.Fatalf("parseSkill: %v", err)
	}
	if s.BaseDir != "/tmp/skills" {
		t.Errorf("BaseDir = %q, want /tmp/skills", s.BaseDir)
	}
}

func TestParseSkill_BaseDirForDirectorySkill(t *testing.T) {
	entry := Entry{
		Kind:       KindSkill,
		SourcePath: "/tmp/skills/calendar/SKILL.md",
		Raw:        []byte("---\nname: calendar\ndescription: x\n---\nbody"),
		Tier:       TierUser,
	}
	s, err := parseSkill(entry)
	if err != nil {
		t.Fatalf("parseSkill: %v", err)
	}
	want := filepath.Clean("/tmp/skills/calendar")
	if s.BaseDir != want {
		t.Errorf("BaseDir = %q, want %q", s.BaseDir, want)
	}
}

func TestParseSkill_DisableModelInvocation(t *testing.T) {
	entry := Entry{
		Kind:       KindSkill,
		SourcePath: "/tmp/skills/secret.md",
		Raw:        []byte("---\nname: secret\ndescription: x\ndisableModelInvocation: true\n---\nbody"),
		Tier:       TierUser,
	}
	s, err := parseSkill(entry)
	if err != nil {
		t.Fatalf("parseSkill: %v", err)
	}
	if !s.DisableModelInvocation {
		t.Error("DisableModelInvocation = false, want true")
	}
}

func TestParseSkill_DisableModelInvocationDefault(t *testing.T) {
	entry := Entry{
		Kind:       KindSkill,
		SourcePath: "/tmp/skills/normal.md",
		Raw:        []byte("---\nname: normal\ndescription: x\n---\nbody"),
		Tier:       TierUser,
	}
	s, err := parseSkill(entry)
	if err != nil {
		t.Fatalf("parseSkill: %v", err)
	}
	if s.DisableModelInvocation {
		t.Error("DisableModelInvocation = true, want false (default)")
	}
	if !strings.Contains(string(s.Body), "body") {
		t.Errorf("Body = %q", s.Body)
	}
}
