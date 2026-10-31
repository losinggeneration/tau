package artifacts

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestParseSkillHonorsUpstreamInvocationFlag(t *testing.T) {
	for _, tc := range []struct {
		value    string
		disabled bool
	}{
		{"true", true}, {"false", false}, {"\"true\"", false}, {"1", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			skill, err := parseSkill(Entry{Kind: KindSkill, SourcePath: "/skills/secret/SKILL.md", Raw: []byte("---\nname: secret\ndescription: x\ndisable-model-invocation: " + tc.value + "\n---\nbody")})
			require.NoError(t, err)
			assert.Equal(t, tc.disabled, skill.DisableModelInvocation)
			assert.NotContains(t, skill.ExtraFields, "disable-model-invocation")
			catalog := FormatSkillsCatalog([]*Skill{skill})
			if tc.disabled {
				assert.NotContains(t, catalog, "secret")
			} else {
				assert.Contains(t, catalog, "secret")
			}
		})
	}
}

func TestParseSkillPreservesYAMLValues(t *testing.T) {
	skill, err := parseSkill(Entry{Kind: KindSkill, SourcePath: "/skills/example/SKILL.md", Raw: []byte("---\nname: example\ndescription: |\n  First line.\n  Second line.\ndisableModelInvocation: true\ndisable-model-invocation: false\ntags: [one, two]\n---\nbody")})
	require.NoError(t, err)
	assert.Equal(t, "First line.\nSecond line.\n", skill.Description)
	assert.Equal(t, []string{"one", "two"}, skill.Tags)
	assert.False(t, skill.DisableModelInvocation)
	assert.NotContains(t, skill.ExtraFields, "disableModelInvocation")
}

func TestParseSkillLoadsValidationWarningsLikeUpstream(t *testing.T) {
	for _, tc := range []struct {
		metadata, path, name string
		warning              bool
		reject               bool
	}{
		{"name: UpperCase\ndescription: useful", "/skills/parent/SKILL.md", "UpperCase", true, false},
		{"name: trailing-\ndescription: useful", "/skills/parent/SKILL.md", "trailing-", true, false},
		{"description: useful", "/skills/parent/SKILL.md", "parent", false, false},
		{"description: useful", "/skills/parent/loose.md", "parent", false, false},
		{"name: different\ndescription: useful", "/skills/parent/SKILL.md", "different", false, false},
		{"name: 123\ndescription: useful", "/skills/parent/SKILL.md", "parent", false, false},
		{"name: parent\ndescription: 123", "/skills/parent/SKILL.md", "", true, true},
		{"name: parent\ndescription: null", "/skills/parent/SKILL.md", "", true, true},
		{"name: parent\ndescription: " + strings.Repeat("x", 1025), "/skills/parent/SKILL.md", "parent", true, false},
	} {
		t.Run(tc.metadata, func(t *testing.T) {
			registry := NewSkillRegistry(nil)
			diag, err := registry.Register(Entry{Kind: KindSkill, SourcePath: tc.path, Raw: []byte("---\n" + tc.metadata + "\n---\nbody")})
			if tc.reject {
				assert.Error(t, err)
				assert.Zero(t, registry.Len())
				return
			}
			require.NoError(t, err)
			skill, ok := registry.Get(tc.name)
			require.True(t, ok)
			assert.Equal(t, tc.name, skill.Name)
			if tc.warning {
				assert.Equal(t, DiagnosticWarning, diag.Type)
			} else {
				assert.Empty(t, diag.Type)
			}
		})
	}
}
