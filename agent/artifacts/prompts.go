// Package artifacts provides unified discovery for skills, prompt templates,
// and themes. This file contains the PromptRegistry implementation.
package artifacts // import "github.com/losinggeneration/tau/agent/artifacts"

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf16"

	"gopkg.in/yaml.v3"
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

	frontmatter, err := parseYAML(yamlData)
	if err != nil {
		return nil, fmt.Errorf("artifacts: prompt parse error: %w", err)
	}

	description, _ := frontmatter["description"].(string)
	if description == "" {
		for _, line := range strings.Split(string(body), "\n") {
			if strings.TrimSpace(line) != "" {
				units := utf16.Encode([]rune(line))
				description = line
				if len(units) > 60 {
					description = string(utf16.Decode(units[:60])) + "..."
				}
				break
			}
		}
	}

	// Get argument-hint field (optional)
	argumentHint, _ := frontmatter["argument-hint"].(string)

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

func parseYAML(data []byte) (map[string]any, error) {
	result := make(map[string]any)
	if err := yaml.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse frontmatter: %w", err)
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
	var inQuote rune

	for _, ch := range argsString {
		if inQuote != 0 {
			// Inside a quoted string
			if ch == inQuote {
				// End of quoted string
				inQuote = 0
			} else {
				current.WriteRune(ch)
			}
			continue
		}

		// Outside quotes
		switch ch {
		case '"', '\'':
			// Start of quoted string
			inQuote = ch
		default:
			if unicode.Is(unicode.Zs, ch) || strings.ContainsRune("\t\v\f \n\r\u2028\u2029\ufeff", ch) {
				// Whitespace separator
				if current.Len() > 0 {
					args = append(args, current.String())
					current.Reset()
				}
			} else {
				current.WriteRune(ch)
			}
		}
	}

	// Append final argument if any
	if current.Len() > 0 {
		args = append(args, current.String())
	}

	return args
}

var argumentPlaceholder = regexp.MustCompile(`\$\{(\d+|ARGUMENTS|@):-([^}]*)\}|\$\{@:(\d+)(?::(\d+))?\}|\$(ARGUMENTS|@|\d+)`)

// SubstituteArgs substitutes argument placeholders in template content.
// Supports:
//   - $1, $2, ... for positional args
//   - $@ and $ARGUMENTS for all args
//   - ${N:-default}, ${@:-default} and ${ARGUMENTS:-default} for empty values
//   - ${@:N} for args from Nth onwards (bash-style slicing)
//   - ${@:N:L} for L args starting from Nth
//
// Note: Replacement happens on the template string only. Argument values
// containing patterns like $1, $@, or $ARGUMENTS are NOT recursively substituted.
func SubstituteArgs(content string, args []string) string {
	all := strings.Join(args, " ")
	number := func(value string) int {
		n, err := strconv.Atoi(value)
		if errors.Is(err, strconv.ErrRange) {
			return math.MaxInt
		}
		if err != nil {
			return 0
		}
		return n
	}
	positional := func(target string) string {
		if target == "@" || target == "ARGUMENTS" {
			return all
		}
		index := number(target) - 1
		if index < 0 || index >= len(args) {
			return ""
		}
		return args[index]
	}
	return argumentPlaceholder.ReplaceAllStringFunc(content, func(match string) string {
		parts := argumentPlaceholder.FindStringSubmatch(match)
		if parts[1] != "" {
			if value := positional(parts[1]); value != "" {
				return value
			}
			return parts[2]
		}
		if parts[3] != "" {
			start := min(max(number(parts[3])-1, 0), len(args))
			end := len(args)
			if parts[4] != "" {
				end = start + min(number(parts[4]), len(args)-start)
			}
			return strings.Join(args[start:end], " ")
		}
		return positional(parts[5])
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
