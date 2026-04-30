// Package commands defines the CommandRegistry that artifacts, extensions,
// and session specs forward-reference. Owners register slash commands
// (built-in, prompts, skills, plugins) and the registry dispatches them.
package commands // import "github.com/losinggeneration/tau/commands"

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// CommandUI is the subset of the UI interface that command handlers need.
// Satisfied by ui.UI and any future TUI implementation.
type CommandUI interface {
	Confirm(ctx context.Context, title, message string) (bool, error)
	Select(ctx context.Context, title string, options []string) (int, error)
	Input(ctx context.Context, title, placeholder string) (string, error)
}

// CommandSession is the subset of the Session interface that command handlers
// need. Satisfied by sdk.Session.
type CommandSession interface {
	// GetLastAssistantText returns the text content of the most recent assistant
	// message. Returns empty string if no assistant message exists.
	GetLastAssistantText() string
	// GetSessionName returns the session display name.
	GetSessionName() string
	// Compact triggers session compaction. customInstructions is optional and
	// focuses the generated summary.
	Compact(ctx context.Context, customInstructions ...string) error
}

// Handler is the function signature for command execution.
type Handler func(ctx context.Context, args string, ui CommandUI, sess CommandSession) (Result, error)

// Source indicates where a command was registered from.
type Source int

const (
	// Builtin indicates a built-in command.
	Builtin Source = iota
	// Prompt indicates a prompt-template command.
	Prompt
	// Skill indicates a skill command.
	Skill
	// Plugin indicates a plugin command.
	Plugin
)

// String returns a human-readable name for the source.
func (s Source) String() string {
	switch s {
	case Builtin:
		return "builtin"
	case Prompt:
		return "prompt"
	case Skill:
		return "skill"
	case Plugin:
		return "plugin"
	default:
		return "unknown"
	}
}

// Tier represents the precedence tier of a command registration.
// Higher numeric value = higher precedence.
// Tiers follow the discovery precedence: CLI > Project > User > Settings > Packages > Builtin.
type Tier int

const (
	// TierBuiltin is the lowest tier: built-in commands.
	TierBuiltin Tier = iota
	// TierPackages is for package-installed artifacts.
	TierPackages
	// TierSettings is for artifacts declared in settings.json.
	TierSettings
	// TierUser is for user-global artifacts (~/.tau/...).
	TierUser
	// TierProject is for project-local artifacts (.tau/...).
	TierProject
	// TierCLI is the highest tier: explicitly passed via CLI flags.
	TierCLI
)

// String returns a human-readable name for the tier.
func (t Tier) String() string {
	switch t {
	case TierBuiltin:
		return "builtin"
	case TierPackages:
		return "packages"
	case TierSettings:
		return "settings"
	case TierUser:
		return "user"
	case TierProject:
		return "project"
	case TierCLI:
		return "cli"
	default:
		return "unknown"
	}
}

// Info describes a registered command.
type Info struct {
	// Name is the slash-command name (without the leading /).
	Name string
	// Description is a short human-readable summary.
	Description string
	// ArgumentHint shows expected arguments, e.g. "<file>" or "[--force]".
	ArgumentHint string
	// Source indicates where this command was registered.
	Source Source
	// SourcePath is the file path of the source (for collision messages).
	SourcePath string
	// Tier is the precedence tier of this registration.
	Tier Tier
}

// Result is returned by a command handler.
type Result struct {
	// Output is the text to display to the user.
	Output string
	// IsError indicates the result represents an error.
	IsError bool
}

// Registry is the interface for registering and dispatching slash commands.
type Registry interface {
	// Register adds a command at TierBuiltin tier. Returns ErrCommandExists
	// if the name is already taken by a registration with equal or higher precedence.
	Register(info Info, handler Handler) error
	// RegisterAt adds a command at the given precedence tier. Higher-tier
	// registrations replace lower-tier ones; same tier uses first-wins.
	RegisterAt(info Info, handler Handler, sourcePath string, tier Tier) error
	// ClearSource removes all commands registered from the given source path.
	// Used during reload to remove stale commands before re-registering.
	ClearSource(sourcePath string)
	// Lookup retrieves a handler and its info by name.
	Lookup(name string) (Handler, Info, bool)
	// List returns all registered commands sorted by name.
	List() []Info
	// ParseAndExecute parses a slash-command line (e.g. "/review HEAD~1"),
	// splits into name + args, looks up the handler, and dispatches.
	ParseAndExecute(ctx context.Context, line string, ui CommandUI, sess CommandSession) (Result, error)
}

// ErrCommandExists is returned when a command name is already registered.
type ErrCommandExists struct {
	Name   string
	Source Source
}

func (e ErrCommandExists) Error() string {
	return fmt.Sprintf("command %q already registered by %s", e.Name, e.Source)
}

// ErrCommandNotFound is returned when a command name is not registered.
type ErrCommandNotFound struct {
	Name string
}

func (e ErrCommandNotFound) Error() string {
	return fmt.Sprintf("command %q not found", e.Name)
}

// InMemoryRegistry is a concrete in-memory Registry implementation with
// conflict detection and tier-aware precedence.
type InMemoryRegistry struct {
	mu       sync.RWMutex
	commands map[string]entry
}

type entry struct {
	info    Info
	handler Handler
}

// NewInMemoryRegistry creates an empty InMemoryRegistry.
func NewInMemoryRegistry() *InMemoryRegistry {
	return &InMemoryRegistry{
		commands: make(map[string]entry),
	}
}

// Register adds a command at the TierBuiltin tier.
// Returns ErrCommandExists if the name is already taken by a registration
// with equal or higher precedence.
func (r *InMemoryRegistry) Register(info Info, handler Handler) error {
	return r.register(info, handler, "", TierBuiltin)
}

// RegisterAt registers a command at the given precedence tier. Use this for
// artifact and plugin registrations that have known tiers. Pass the source file
// path (or any unique identifier) as sourcePath to enable within-tier ordering.
// Higher-tier registrations replace lower-tier ones; same-tier uses first-wins
// (alphabetical by sourcePath, so earlier paths win).
func (r *InMemoryRegistry) RegisterAt(info Info, handler Handler, sourcePath string, tier Tier) error {
	return r.register(info, handler, sourcePath, tier)
}

// register implements tier-aware registration.
// When tiers differ, higher tier wins. Same tier uses first-wins alphabetical.
func (r *InMemoryRegistry) register(info Info, handler Handler, sourcePath string, tier Tier) error {
	info.Tier = tier
	if sourcePath != "" {
		info.SourcePath = sourcePath
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.commands[info.Name]; ok {
		existingTier := existing.info.Tier

		// Same tier: first wins (alphabetical by source path — earlier wins).
		if existingTier == tier {
			// If no source path for either, reject the second (first wins).
			if existing.info.SourcePath == "" && sourcePath == "" {
				return ErrCommandExists{
					Name:   info.Name,
					Source: existing.info.Source,
				}
			}
			// Alphabetical: earlier path wins.
			if sourcePath != "" && (existing.info.SourcePath == "" || sourcePath < existing.info.SourcePath) {
				r.commands[info.Name] = entry{info: info, handler: handler}
				return nil
			}
			return ErrCommandExists{
				Name:   info.Name,
				Source: existing.info.Source,
			}
		}

		// Different tiers: higher tier wins, lower tier is silently replaced.
		if tier > existingTier {
			r.commands[info.Name] = entry{info: info, handler: handler}
			return nil
		}
		// Lower or equal tier: existing wins, new is silently dropped.
		return ErrCommandExists{
			Name:   info.Name,
			Source: existing.info.Source,
		}
	}
	r.commands[info.Name] = entry{info: info, handler: handler}
	return nil
}

// Lookup retrieves a handler and its info by name.
func (r *InMemoryRegistry) Lookup(name string) (Handler, Info, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	e, ok := r.commands[name]
	if !ok {
		return nil, Info{}, false
	}
	return e.handler, e.info, true
}

// List returns all registered commands sorted by name.
func (r *InMemoryRegistry) List() []Info {
	r.mu.RLock()
	defer r.mu.RUnlock()

	infos := make([]Info, 0, len(r.commands))
	for _, e := range r.commands {
		infos = append(infos, e.info)
	}
	slices.SortFunc(infos, func(a, b Info) int {
		return cmp.Compare(a.Name, b.Name)
	})
	return infos
}

// ClearSource removes all commands registered from the given source path.
// Used during reload to remove stale commands before re-registering.
// Commands with a higher-tier registration remain; only commands from the
// exact sourcePath are removed.
func (r *InMemoryRegistry) ClearSource(sourcePath string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for name, e := range r.commands {
		if e.info.SourcePath == sourcePath {
			delete(r.commands, name)
		}
	}
}

// ParseAndExecute parses a slash-command line, splits into name + args,
// looks up the handler, and dispatches.
func (r *InMemoryRegistry) ParseAndExecute(ctx context.Context, line string, ui CommandUI, sess CommandSession) (Result, error) {
	name, args := parseCommand(line)
	handler, _, ok := r.Lookup(name)
	if !ok {
		return Result{}, ErrCommandNotFound{Name: name}
	}
	return handler(ctx, args, ui, sess)
}

// parseCommand splits "/name args..." into ("name", "args").
func parseCommand(line string) (name, args string) {
	line = strings.TrimPrefix(line, "/")
	parts := strings.SplitN(line, " ", 2)
	name = parts[0]
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}
	return name, args
}
