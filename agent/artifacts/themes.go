// Package artifacts provides unified discovery for skills, prompt templates,
// and themes. This file contains the ThemeRegistry implementation.
package artifacts // import "github.com/losinggeneration/tau/agent/artifacts"

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
)

// Theme represents a loaded theme.
type Theme struct {
	ID          string
	Name        string
	Raw         []byte
	SourcePath  string
	Tier        SourceTier
	ExtraFields map[string]any
}

// ThemeRegistry manages theme discovery and lookup.
type ThemeRegistry struct {
	mu     sync.RWMutex
	themes map[string]*Theme

	// For prefix matching
	ordered []*Theme
	log     *slog.Logger
}

// NewThemeRegistry creates a new theme registry.
func NewThemeRegistry(log *slog.Logger) *ThemeRegistry {
	return &ThemeRegistry{
		themes: make(map[string]*Theme),
		log:    log,
	}
}

// Register registers a theme entry. Implements precedence rules.
func (r *ThemeRegistry) Register(entry Entry) error {
	theme, err := parseTheme(entry)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Check for name collision (first wins)
	if existing, ok := r.themes[theme.ID]; ok {
		if r.log != nil {
			r.log.Warn("artifacts: theme shadowed", "id", theme.ID, "winner", existing.SourcePath, "loser", theme.SourcePath)
		}
		return nil // Skip, first wins
	}

	r.themes[theme.ID] = theme
	r.rebuildOrdered()
	return nil
}

// parseTheme parses a theme entry.
func parseTheme(entry Entry) (*Theme, error) {
	if entry.Kind != KindTheme {
		return nil, fmt.Errorf("artifacts: expected theme, got %s", entry.Kind)
	}

	// Parse JSON for name (minimal schema)
	var data map[string]any
	if err := json.Unmarshal(entry.Raw, &data); err != nil {
		return nil, fmt.Errorf("artifacts: theme parse error: %w", err)
	}

	// Get name field
	name, _ := getStringField(data, "name")
	if name == "" {
		// Fall back to filename slug
		name = slugify(strings.TrimSuffix(filepath.Base(entry.SourcePath), ".json"))
	}

	// Collect extra fields
	extra := make(map[string]any)
	for k, v := range data {
		if k != "name" {
			extra[k] = v
		}
	}

	return &Theme{
		ID:          name,
		Name:        name,
		Raw:         entry.Raw,
		SourcePath:  entry.SourcePath,
		Tier:        entry.Tier,
		ExtraFields: extra,
	}, nil
}

// Get returns a theme by exact ID or unique prefix.
func (r *ThemeRegistry) Get(id string) (*Theme, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Exact match
	if theme, ok := r.themes[id]; ok {
		return theme, true
	}

	// Prefix match (must be unique)
	var matches []*Theme
	prefix := strings.ToLower(id)
	for _, theme := range r.ordered {
		if strings.HasPrefix(strings.ToLower(theme.ID), prefix) {
			matches = append(matches, theme)
		}
	}

	if len(matches) == 1 {
		return matches[0], true
	}
	return nil, false
}

// Matches returns all exact or prefix matches for an ID.
func (r *ThemeRegistry) Matches(id string) []*Theme {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if theme, ok := r.themes[id]; ok {
		return []*Theme{theme}
	}

	var matches []*Theme
	prefix := strings.ToLower(id)
	for _, theme := range r.ordered {
		if strings.HasPrefix(strings.ToLower(theme.ID), prefix) {
			matches = append(matches, theme)
		}
	}
	return matches
}

// List returns all registered themes.
func (r *ThemeRegistry) List() []*Theme {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*Theme, len(r.ordered))
	copy(result, r.ordered)
	return result
}

// ListByTier returns themes filtered by source tier.
func (r *ThemeRegistry) ListByTier(tier SourceTier) []*Theme {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []*Theme
	for _, theme := range r.ordered {
		if theme.Tier == tier {
			result = append(result, theme)
		}
	}
	return result
}

// Load loads entries from a discovery walk.
func (r *ThemeRegistry) Load(entries []Entry) error {
	for _, e := range entries {
		if e.Kind != KindTheme {
			continue
		}
		if err := r.Register(e); err != nil {
			if r.log != nil {
				r.log.Error("artifacts: theme load failed", "path", e.SourcePath, "error", err)
			}
		}
	}
	return nil
}

// Reload clears and reloads from entries.
func (r *ThemeRegistry) Reload(entries []Entry) error {
	r.mu.Lock()
	r.themes = make(map[string]*Theme)
	r.mu.Unlock()
	return r.Load(entries)
}

// GetByID is an alias for Get.
func (r *ThemeRegistry) GetByID(id string) (*Theme, bool) {
	return r.Get(id)
}

// Len returns the number of registered themes.
func (r *ThemeRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.themes)
}

func (r *ThemeRegistry) rebuildOrdered() {
	r.ordered = make([]*Theme, 0, len(r.themes))
	for _, theme := range r.themes {
		r.ordered = append(r.ordered, theme)
	}
}
