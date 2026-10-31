// Package artifacts provides unified discovery for skills, prompt templates,
// and themes. This file contains the SkillRegistry implementation.
package artifacts // import "github.com/losinggeneration/tau/agent/artifacts"

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
)

// Diagnostic represents a validation warning or error for a discovered artifact.
type Diagnostic struct {
	Type      string
	Message   string
	Path      string
	Collision *Collision
}

// Collision represents a name collision between two resources.
type Collision struct {
	ResourceType string
	Name         string
	WinnerPath   string
	LoserPath    string
}

// DiagnosticType constants for the Type field.
const (
	DiagnosticWarning   = "warning"
	DiagnosticError     = "error"
	DiagnosticCollision = "collision"
)

// MaxNameLength is the maximum allowed skill name length.
const MaxNameLength = 64

// MaxDescriptionLength is the maximum allowed skill description length.
const MaxDescriptionLength = 1024

// Skill represents a loaded skill.
type Skill struct {
	ID          string
	Name        string
	Description string
	Version     string
	Tags        []string
	Body        []byte
	SourcePath  string
	// BaseDir is the directory used to resolve relative paths referenced from
	// the skill body. For directory skills (`<dir>/SKILL.md`) it is `<dir>`.
	// For loose `.md` skills it is the parent directory of the file.
	BaseDir string
	// DisableModelInvocation excludes the skill from the system-prompt catalog
	// so the model will not spontaneously invoke it. Slash-command invocation
	// (`/skill:<id>`) still works.
	DisableModelInvocation bool
	Tier                   SourceTier
	ExtraFields            map[string]any // Passthrough for tau skills show
}

// SkillRegistry manages skill discovery and lookup.
type SkillRegistry struct {
	mu     sync.RWMutex
	skills map[string]*Skill // ID -> Skill

	// For prefix matching
	ordered []*Skill
	log     *slog.Logger
}

// NewSkillRegistry creates a new skill registry.
func NewSkillRegistry(log *slog.Logger) *SkillRegistry {
	return &SkillRegistry{
		skills: make(map[string]*Skill),
		log:    log,
	}
}

// Register registers a skill entry. Implements precedence rules.
// It returns diagnostics for any validation warnings.
func (r *SkillRegistry) Register(entry Entry) (Diagnostic, error) {
	skill, diag, err := parseSkillWithDiagnostics(entry)
	if err != nil {
		return diag, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Check for name collision (first wins)
	if existing, ok := r.skills[skill.ID]; ok {
		if r.log != nil {
			r.log.Warn("artifacts: skill shadowed", "id", skill.ID, "winner", existing.SourcePath, "loser", skill.SourcePath)
		}
		// Return collision diagnostic
		return Diagnostic{
			Type:    DiagnosticCollision,
			Message: fmt.Sprintf(`name "%s" collision`, skill.ID),
			Path:    skill.SourcePath,
			Collision: &Collision{
				ResourceType: "skill",
				Name:         skill.ID,
				WinnerPath:   existing.SourcePath,
				LoserPath:    skill.SourcePath,
			},
		}, nil
	}

	r.skills[skill.ID] = skill
	r.rebuildOrdered()
	return diag, nil
}

// parseSkill parses a skill entry.
func parseSkill(entry Entry) (*Skill, error) {
	skill, _, err := parseSkillWithDiagnostics(entry)
	return skill, err
}

// parseSkillWithDiagnostics parses a skill entry and returns diagnostics.
// If description is missing or empty, returns (nil, error) to reject the skill.
func parseSkillWithDiagnostics(entry Entry) (*Skill, Diagnostic, error) {
	var diag Diagnostic

	if entry.Kind != KindSkill {
		return nil, diag, fmt.Errorf("artifacts: expected skill, got %s", entry.Kind)
	}

	// Parse frontmatter
	yamlData, body, err := parseFrontmatter(entry.Raw)
	if err != nil {
		return nil, diag, fmt.Errorf("artifacts: skill parse error: %w", err)
	}

	frontmatter, err := parseYAML(yamlData)
	if err != nil {
		return nil, diag, fmt.Errorf("artifacts: skill parse error: %w", err)
	}

	name, _ := frontmatter["name"].(string)
	if name == "" {
		name = filepath.Base(filepath.Dir(entry.SourcePath))
	}
	description, _ := frontmatter["description"].(string)
	warnings := append(validateName(name), validateDescription(description)...)
	if len(warnings) > 0 {
		diag = Diagnostic{Type: DiagnosticWarning, Message: strings.Join(warnings, "; "), Path: entry.SourcePath}
	}
	if strings.TrimSpace(description) == "" {
		return nil, diag, fmt.Errorf("artifacts: skill %s: description is required", entry.SourcePath)
	}

	// Get optional fields
	version, _ := getStringField(frontmatter, "version")

	var tags []string
	if tagsRaw, ok := frontmatter["tags"].([]any); ok {
		for _, t := range tagsRaw {
			if s, ok := t.(string); ok {
				tags = append(tags, s)
			}
		}
	} else if tagsStr, ok := getStringField(frontmatter, "tags"); ok {
		tags = strings.Split(tagsStr, ",")
		for i := range tags {
			tags[i] = strings.TrimSpace(tags[i])
		}
	}

	// Compute BaseDir: for directory skills (<dir>/SKILL.md), use <dir>;
	// for loose .md skills, use the parent directory of the file.
	baseDir := filepath.Dir(entry.SourcePath)

	disableModelInvocation, _ := frontmatter["disable-model-invocation"].(bool)
	if _, present := frontmatter["disable-model-invocation"]; !present {
		switch value := frontmatter["disableModelInvocation"].(type) {
		case bool:
			disableModelInvocation = value
		case string:
			disableModelInvocation = strings.EqualFold(value, "true")
		}
	}

	// Collect extra fields
	extra := make(map[string]any)
	for k, v := range frontmatter {
		switch k {
		case "name", "description", "version", "tags", "disable-model-invocation", "disableModelInvocation":
			// Skip, already handled
		default:
			extra[k] = v
		}
	}

	return &Skill{
		ID:                     name,
		Name:                   name,
		Description:            description,
		Version:                version,
		Tags:                   tags,
		Body:                   body,
		SourcePath:             entry.SourcePath,
		BaseDir:                baseDir,
		DisableModelInvocation: disableModelInvocation,
		Tier:                   entry.Tier,
		ExtraFields:            extra,
	}, diag, nil
}

// validateName validates a skill name per Agent Skills spec.
// Returns a slice of error messages (empty if valid).
func validateName(name string) []string {
	var errors []string

	if len(name) > MaxNameLength {
		errors = append(errors, fmt.Sprintf("name exceeds %d characters (%d)", MaxNameLength, len(name)))
	}

	if !isValidSkillName(name) {
		errors = append(errors, "name contains invalid characters (must be lowercase a-z, 0-9, hyphens only)")
	}

	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		errors = append(errors, "name must not start or end with a hyphen")
	}

	if strings.Contains(name, "--") {
		errors = append(errors, "name must not contain consecutive hyphens")
	}

	return errors
}

// isValidSkillName checks if name contains only lowercase a-z, 0-9, and hyphens.
// Returns false for empty strings (must have at least one valid character).
func isValidSkillName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// validateDescription validates a skill description per Agent Skills spec.
// Returns a slice of error messages (empty if valid).
func validateDescription(description string) []string {
	var errors []string

	if description == "" || strings.TrimSpace(description) == "" {
		errors = append(errors, "description is required")
	} else if len(description) > MaxDescriptionLength {
		errors = append(errors, fmt.Sprintf("description exceeds %d characters (%d)", MaxDescriptionLength, len(description)))
	}

	return errors
}

// Get returns a skill by exact ID or unique prefix.
func (r *SkillRegistry) Get(id string) (*Skill, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Exact match
	if skill, ok := r.skills[id]; ok {
		return skill, true
	}

	// Prefix match (must be unique)
	var matches []*Skill
	prefix := strings.ToLower(id)
	for _, skill := range r.ordered {
		if strings.HasPrefix(strings.ToLower(skill.ID), prefix) {
			matches = append(matches, skill)
		}
	}

	if len(matches) == 1 {
		return matches[0], true
	}
	return nil, false
}

// Matches returns all exact or prefix matches for an ID.
func (r *SkillRegistry) Matches(id string) []*Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if skill, ok := r.skills[id]; ok {
		return []*Skill{skill}
	}

	var matches []*Skill
	prefix := strings.ToLower(id)
	for _, skill := range r.ordered {
		if strings.HasPrefix(strings.ToLower(skill.ID), prefix) {
			matches = append(matches, skill)
		}
	}
	return matches
}

// List returns all registered skills.
func (r *SkillRegistry) List() []*Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*Skill, len(r.ordered))
	copy(result, r.ordered)
	return result
}

// ListByTier returns skills filtered by source tier.
func (r *SkillRegistry) ListByTier(tier SourceTier) []*Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []*Skill
	for _, skill := range r.ordered {
		if skill.Tier == tier {
			result = append(result, skill)
		}
	}
	return result
}

// Load loads entries from a discovery walk. Validation warnings and errors
// from each entry are logged but not propagated. Use LoadWithDiagnostics
// when callers (e.g. agent reload flows) need the full diagnostic slice.
func (r *SkillRegistry) Load(entries []Entry) error {
	for _, d := range r.LoadWithDiagnostics(entries) {
		if d.Type == DiagnosticError && r.log != nil {
			r.log.Error("artifacts: skill diagnostic", "path", d.Path, "message", d.Message)
		}
	}
	return nil
}

// LoadWithDiagnostics loads entries from a discovery walk and returns one
// Diagnostic per entry that produced a warning, error, or collision. The
// returned slice preserves the order of entries. Skills rejected by
// validation are not added to the registry; skills that produce a soft
// warning are added; skills that collide with an existing ID are skipped
// in favor of the previously-registered entry.
func (r *SkillRegistry) LoadWithDiagnostics(entries []Entry) []Diagnostic {
	var diags []Diagnostic
	for _, e := range entries {
		if e.Kind != KindSkill {
			continue
		}
		d, err := r.Register(e)
		if err != nil && d.Type == "" {
			// Defensive: Register should always populate d on err. If it
			// didn't, synthesize a diagnostic so callers can still observe
			// the failure.
			d = Diagnostic{
				Type:    DiagnosticError,
				Message: err.Error(),
				Path:    e.SourcePath,
			}
		}
		if d.Type != "" {
			diags = append(diags, d)
		}
	}
	return diags
}

// Reload clears and reloads from entries.
func (r *SkillRegistry) Reload(entries []Entry) error {
	r.mu.Lock()
	r.skills = make(map[string]*Skill)
	r.mu.Unlock()
	return r.Load(entries)
}

// GetByID is an alias for Get.
func (r *SkillRegistry) GetByID(id string) (*Skill, bool) {
	return r.Get(id)
}

// Len returns the number of registered skills.
func (r *SkillRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.skills)
}

func (r *SkillRegistry) rebuildOrdered() {
	r.ordered = make([]*Skill, 0, len(r.skills))
	for _, skill := range r.skills {
		r.ordered = append(r.ordered, skill)
	}
	// Alphabetical by source path within tier
	// Note: Order here is after precedence is applied by Walk()
}

func getStringField(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	switch s := v.(type) {
	case string:
		return s, true
	case json.Number:
		return s.String(), true
	default:
		return fmt.Sprintf("%v", v), true
	}
}
