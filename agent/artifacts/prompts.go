// Package artifacts provides unified discovery for skills, prompt templates,
// and themes. This file contains the PromptRegistry implementation.
package artifacts // import "github.com/losinggeneration/tau/agent/artifacts"

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Prompt represents a loaded prompt template.
type Prompt struct {
	ID           string
	Name         string
	Description  string
	ArgumentHint string
	Body         []byte
	SourcePath   string
	Tier         SourceTier
	ExtraFields  map[string]any // Passthrough for tau prompts show
}

// PromptRegistry manages prompt template discovery and lookup.
type PromptRegistry struct {
	mu      sync.RWMutex
	prompts map[string]*Prompt // ID -> Prompt

	// For prefix matching
	ordered []*Prompt
	log     *slog.Logger
}

// NewPromptRegistry creates a new prompt registry.
func NewPromptRegistry(log *slog.Logger) *PromptRegistry {
	return &PromptRegistry{
		prompts: make(map[string]*Prompt),
		log:     log,
	}
}

// Register registers a prompt entry. Implements precedence rules.
func (r *PromptRegistry) Register(entry Entry) error {
	prompt, err := parsePrompt(entry)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Check for name collision (first wins)
	if existing, ok := r.prompts[prompt.ID]; ok {
		if r.log != nil {
			r.log.Warn("artifacts: prompt shadowed", "id", prompt.ID, "winner", existing.SourcePath, "loser", prompt.SourcePath)
		}
		return nil // Skip, first wins
	}

	r.prompts[prompt.ID] = prompt
	r.rebuildOrdered()
	return nil
}

// parsePrompt parses a prompt entry.
func parsePrompt(entry Entry) (*Prompt, error) {
	if entry.Kind != KindPrompt {
		return nil, fmt.Errorf("artifacts: expected prompt, got %s", entry.Kind)
	}

	// Parse frontmatter
	yamlData, body, err := parseFrontmatter(entry.Raw)
	if err != nil {
		return nil, fmt.Errorf("artifacts: prompt parse error: %w", err)
	}

	// Parse YAML frontmatter (lenient)
	frontmatter := make(map[string]any)
	if len(yamlData) > 0 {
		if fm, err := parseYAML(yamlData); err == nil {
			frontmatter = fm
		}
	}

	// Get description field (optional, falls back to first non-empty body line)
	description, _ := getStringField(frontmatter, "description")
	if description == "" {
		// Fall back to first non-empty body line
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				description = line
				break
			}
		}
	}

	if len([]rune(description)) > 60 {
		description = string([]rune(description)[:60])
	}

	// Get argument-hint field (optional)
	argumentHint, _ := getStringField(frontmatter, "argument-hint")

	// Command name = filename without .md
	name := strings.TrimSuffix(filepath.Base(entry.SourcePath), ".md")
	if name == "" {
		name = slugify(entry.SourcePath)
	}

	// Collect extra fields
	extra := make(map[string]any)
	for k, v := range frontmatter {
		switch k {
		case "description", "argument-hint":
			// Skip, already handled
		default:
			extra[k] = v
		}
	}

	return &Prompt{
		ID:           name,
		Name:         name,
		Description:  description,
		ArgumentHint: argumentHint,
		Body:         body,
		SourcePath:   entry.SourcePath,
		Tier:         entry.Tier,
		ExtraFields:  extra,
	}, nil
}

// parseYAML parses YAML frontmatter data into a map. Returns a lenient parser
// that doesn't fail on minor errors.
func parseYAML(data []byte) (map[string]any, error) {
	result := make(map[string]any)

	// Simple YAML parsing for frontmatter
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if key, val, found := strings.Cut(line, ":"); found && key != "" {
			key = strings.TrimSpace(key)
			val = strings.TrimSpace(val)

			// Handle array values like [python, testing]
			if strings.HasPrefix(val, "[") && strings.HasSuffix(val, "]") {
				val = strings.Trim(val, "[]")
				parts := strings.Split(val, ",")
				var arr []string
				for _, p := range parts {
					arr = append(arr, strings.TrimSpace(p))
				}
				result[key] = arr
			} else if val == "" {
				result[key] = nil
			} else {
				result[key] = val
			}
		}
	}
	return result, nil
}

// Get returns a prompt by exact ID or unique prefix.
func (r *PromptRegistry) Get(id string) (*Prompt, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Exact match
	if prompt, ok := r.prompts[id]; ok {
		return prompt, true
	}

	// Prefix match (must be unique)
	var matches []*Prompt
	prefix := strings.ToLower(id)
	for _, prompt := range r.ordered {
		if strings.HasPrefix(strings.ToLower(prompt.ID), prefix) {
			matches = append(matches, prompt)
		}
	}

	if len(matches) == 1 {
		return matches[0], true
	}
	return nil, false
}

// Matches returns all exact or prefix matches for an ID.
func (r *PromptRegistry) Matches(id string) []*Prompt {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if prompt, ok := r.prompts[id]; ok {
		return []*Prompt{prompt}
	}

	var matches []*Prompt
	prefix := strings.ToLower(id)
	for _, prompt := range r.ordered {
		if strings.HasPrefix(strings.ToLower(prompt.ID), prefix) {
			matches = append(matches, prompt)
		}
	}
	return matches
}

// List returns all registered prompts.
func (r *PromptRegistry) List() []*Prompt {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*Prompt, len(r.ordered))
	copy(result, r.ordered)
	return result
}

// ListByTier returns prompts filtered by source tier.
func (r *PromptRegistry) ListByTier(tier SourceTier) []*Prompt {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []*Prompt
	for _, prompt := range r.ordered {
		if prompt.Tier == tier {
			result = append(result, prompt)
		}
	}
	return result
}

// Load loads entries from a discovery walk.
func (r *PromptRegistry) Load(entries []Entry) error {
	for _, e := range entries {
		if e.Kind != KindPrompt {
			continue
		}
		if err := r.Register(e); err != nil {
			if r.log != nil {
				r.log.Error("artifacts: prompt load failed", "path", e.SourcePath, "error", err)
			}
		}
	}
	return nil
}

// Reload clears and reloads from entries.
func (r *PromptRegistry) Reload(entries []Entry) error {
	r.mu.Lock()
	r.prompts = make(map[string]*Prompt)
	r.mu.Unlock()
	return r.Load(entries)
}

// GetByID is an alias for Get.
func (r *PromptRegistry) GetByID(id string) (*Prompt, bool) {
	return r.Get(id)
}

// Len returns the number of registered prompts.
func (r *PromptRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.prompts)
}

// ParseCommandArgs parses command arguments respecting quoted strings (bash-style).
// Returns array of arguments. Handles single and double quotes, whitespace separation.
func ParseCommandArgs(argsString string) []string {
	var args []string
	var current strings.Builder
	var inQuote rune = 0 // 0 = not in quote, '"' or '\'' = in quote

	for i := 0; i < len(argsString); i++ {
		ch := rune(argsString[i])

		if inQuote != 0 {
			// Inside a quoted string
			if ch == inQuote {
				// End of quoted string
				inQuote = 0
			} else {
				current.WriteByte(byte(ch))
			}
			continue
		}

		// Outside quotes
		switch ch {
		case '"', '\'':
			// Start of quoted string
			inQuote = ch
		case ' ', '\t':
			// Whitespace separator
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
		default:
			current.WriteByte(byte(ch))
		}
	}

	// Append final argument if any
	if current.Len() > 0 {
		args = append(args, current.String())
	}

	return args
}

// SubstituteArgs substitutes argument placeholders in template content.
// Supports:
//   - $1, $2, ... for positional args
//   - $@ and $ARGUMENTS for all args
//   - ${@:N} for args from Nth onwards (bash-style slicing)
//   - ${@:N:L} for L args starting from Nth
//
// Note: Replacement happens on the template string only. Argument values
// containing patterns like $1, $@, or $ARGUMENTS are NOT recursively substituted.
func SubstituteArgs(content string, args []string) string {
	result := content

	// Replace $1, $2, etc. with positional args FIRST (before wildcards)
	// This prevents wildcard replacement values containing $<digit> patterns from being re-substituted
	result = substitutePositional(result, args)

	// Replace ${@:start} or ${@:start:length} with sliced args (bash-style)
	// Process BEFORE simple $@ to avoid conflicts
	result = substituteSlice(result, args)

	// Pre-compute all args joined (optimization)
	allArgs := strings.Join(args, " ")

	// Replace $ARGUMENTS with all args joined (new syntax, aligns with Claude, Codex, OpenCode)
	result = strings.ReplaceAll(result, "$ARGUMENTS", allArgs)

	// Replace $@ with all args joined (existing syntax)
	result = strings.ReplaceAll(result, "$@", allArgs)

	return result
}

// substitutePositional replaces $1, $2, etc. with positional args.
func substitutePositional(content string, args []string) string {
	// Use regex to find all $<digit> patterns
	re := regexp.MustCompile(`\$(\d+)`)
	return re.ReplaceAllStringFunc(content, func(match string) string {
		// Extract the digit
		numStr := match[1:] // Everything after $
		index, err := strconv.Atoi(numStr)
		if err != nil {
			return match
		}
		// Convert to 0-indexed
		idx := index - 1
		if idx >= 0 && idx < len(args) {
			return args[idx]
		}
		return ""
	})
}

// substituteSlice replaces ${@:start} or ${@:start:length} with sliced args.
func substituteSlice(content string, args []string) string {
	// Regex to match ${@:N} or ${@:N:L}
	re := regexp.MustCompile(`\$\{@:(\d+)(?::(\d+))?\}`)
	return re.ReplaceAllStringFunc(content, func(match string) string {
		// Parse the match
		// ${@:N} or ${@:N:L}
		rest := match[4 : len(match)-1] // Remove ${@: and }
		var startStr, lengthStr string
		var found bool
		startStr, lengthStr, found = strings.Cut(rest, ":")
		if !found {
			startStr = rest
		}

		start, err := strconv.Atoi(startStr)
		if err != nil {
			return match
		}

		// Convert to 0-indexed (user provides 1-indexed)
		start--
		if start < 0 {
			start = 0
		}

		if lengthStr != "" {
			length, err := strconv.Atoi(lengthStr)
			if err != nil {
				return match
			}
			return strings.Join(args[start:min(start+length, len(args))], " ")
		}
		return strings.Join(args[start:], " ")
	})
}

// Expand expands the prompt template with the given args string.
// Uses bash-style argument substitution: $1, $2, $@, $ARGUMENTS, ${@:N}, ${@:N:L}
// Returns the expanded content and true if successful, or "", false if not found.
// No recursive expansion - placeholder values are not re-substituted.
func (r *PromptRegistry) Expand(id string, args string) (string, bool) {
	prompt, ok := r.Get(id)
	if !ok {
		return "", false
	}

	// Parse args using bash-style parsing
	parsedArgs := ParseCommandArgs(args)

	// Apply bash-style substitution
	expanded := SubstituteArgs(string(prompt.Body), parsedArgs)
	return expanded, true
}

func (r *PromptRegistry) rebuildOrdered() {
	r.ordered = make([]*Prompt, 0, len(r.prompts))
	for _, prompt := range r.prompts {
		r.ordered = append(r.ordered, prompt)
	}
}
