// Package artifacts: skill formatting helpers used by the runtime to inject
// skill content into prompts. The shapes here match the TS reference
// (coding-agent/src/core/agent-session.ts:_expandSkillCommand and
// coding-agent/src/core/skills.ts:formatSkillsForPrompt) so cross-harness
// skills behave identically under tau.
package artifacts

import "strings"

// FormatSkillInvocation returns the XML-wrapped skill body used as the user
// message when `/skill:<id> <args>` is invoked. If args is non-empty it is
// appended after a blank line.
func FormatSkillInvocation(s *Skill, args string) string {
	body := strings.TrimSpace(string(s.Body))
	var b strings.Builder
	b.WriteString(`<skill name="`)
	b.WriteString(escapeXML(s.Name))
	b.WriteString(`" location="`)
	b.WriteString(escapeXML(s.SourcePath))
	b.WriteString(`">` + "\n")
	b.WriteString("References are relative to ")
	b.WriteString(s.BaseDir)
	b.WriteString(".\n\n")
	b.WriteString(body)
	b.WriteString("\n</skill>")
	if args != "" {
		b.WriteString("\n\n")
		b.WriteString(args)
	}
	return b.String()
}

// FormatSkillsCatalog returns a system-prompt fragment advertising the
// available skills, in the agentskills.io <available_skills> XML form. Skills
// with DisableModelInvocation are excluded from the catalog (they remain
// invokable via /skill:<id>). Returns "" if no skills are advertised.
func FormatSkillsCatalog(skills []*Skill) string {
	visible := make([]*Skill, 0, len(skills))
	for _, s := range skills {
		if s == nil || s.DisableModelInvocation {
			continue
		}
		visible = append(visible, s)
	}
	if len(visible) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\nThe following skills provide specialized instructions for specific tasks.\n")
	b.WriteString("Use the read tool to load a skill's file when the task matches its description.\n")
	b.WriteString("When a skill file references a relative path, resolve it against the skill directory (parent of SKILL.md / dirname of the path) and use that absolute path in tool commands.\n")
	b.WriteString("\n<available_skills>\n")
	for _, s := range visible {
		b.WriteString("  <skill>\n    <name>")
		b.WriteString(escapeXML(s.Name))
		b.WriteString("</name>\n    <description>")
		b.WriteString(escapeXML(s.Description))
		b.WriteString("</description>\n    <location>")
		b.WriteString(escapeXML(s.SourcePath))
		b.WriteString("</location>\n  </skill>\n")
	}
	b.WriteString("</available_skills>")
	return b.String()
}

func escapeXML(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}
