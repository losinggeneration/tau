// Package artifacts provides unified discovery for skills, prompt templates,
// and themes. The discovery walker scans source tiers and yields entries that
// each registry consumes and parses according to its schema.
package artifacts // import "github.com/losinggeneration/tau/agent/artifacts"

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/losinggeneration/tau/commands"
)

// Loader owns all three artifact registries and can reload them.
// It implements the headless `tau reload` behavior: re-discover all artifacts
// and atomically swap each registry, then trigger a plugin reload pass.
type Loader struct {
	skills  *SkillRegistry
	prompts *PromptRegistry
	themes  *ThemeRegistry
	log     *slog.Logger
	mu      sync.RWMutex // protects skills, prompts, themes for atomic reload
	// commandSources tracks artifact source paths that have registered slash commands.
	commandSources map[string]struct{}

	// cfg carries the discovery walk configuration so Reload can re-walk.
	cfg *DiscoveryConfig

	// pluginReloader is called after artifact reload. May be nil.
	pluginReloader func(context.Context) error

	// commandsRegistry is the command registry for slash-command registration.
	// May be nil.
	commandsRegistry commands.Registry
}

// LoaderOptions configures a Loader.
type LoaderOptions struct {
	// Log is used for warnings during discovery and reload. May be nil.
	Log *slog.Logger
	// PluginReloader is called after artifact reloading to trigger a plugin
	// reload pass. May be nil.
	PluginReloader func(context.Context) error
	// CWD is the current working directory for project-local discovery.
	CWD string
	// SettingsPaths are explicit paths from settings.json.
	SettingsPaths map[ArtifactKind][]string
	// UserDirs overrides the user-global artifact directories per kind.
	// Empty entries default to ~/.tau/agent/<subdir>.
	UserDirs map[ArtifactKind]string
	// CLIPaths are explicit artifact paths from CLI flags.
	CLIPaths map[ArtifactKind][]string
	// PackagePaths are directories to scan for installed packages.
	PackagePaths []string
	// DisableSkills skips skill discovery.
	DisableSkills bool
	// DisablePrompts skips prompt template discovery.
	DisablePrompts bool
	// DisableThemes skips theme discovery.
	DisableThemes bool
	// CommandsRegistry is the command registry to register prompts and skills
	// into. Commands are registered with their slash-command name.
	// May be nil to skip command registration.
	CommandsRegistry commands.Registry
}

// NewLoader creates a Loader, runs the initial discovery walk, and loads
// all three registries.
func NewLoader(opts LoaderOptions) (*Loader, error) {
	cfg := &DiscoveryConfig{
		CWD:            opts.CWD,
		SettingsPaths:  opts.SettingsPaths,
		UserDirs:       opts.UserDirs,
		CLIPaths:       opts.CLIPaths,
		PackagePaths:   opts.PackagePaths,
		DisableSkills:  opts.DisableSkills,
		DisablePrompts: opts.DisablePrompts,
		DisableThemes:  opts.DisableThemes,
		Log:            opts.Log,
	}

	entries, err := Walk(*cfg)
	if err != nil {
		return nil, err
	}

	l := &Loader{
		skills:           NewSkillRegistry(opts.Log),
		prompts:          NewPromptRegistry(opts.Log),
		themes:           NewThemeRegistry(opts.Log),
		log:              opts.Log,
		cfg:              cfg,
		pluginReloader:   opts.PluginReloader,
		commandsRegistry: opts.CommandsRegistry,
		commandSources:   make(map[string]struct{}),
	}

	if err := l.skills.Load(entries); err != nil {
		return nil, fmt.Errorf("load skills: %w", err)
	}
	if err := l.prompts.Load(entries); err != nil {
		return nil, fmt.Errorf("load prompts: %w", err)
	}
	if err := l.themes.Load(entries); err != nil {
		return nil, fmt.Errorf("load themes: %w", err)
	}

	// Register prompts and skills as slash commands.
	l.registerCommands()

	return l, nil
}

// SkillRegistry returns the skill registry.
func (l *Loader) SkillRegistry() *SkillRegistry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.skills
}

// PromptRegistry returns the prompt registry.
func (l *Loader) PromptRegistry() *PromptRegistry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.prompts
}

// ThemeRegistry returns the theme registry.
func (l *Loader) ThemeRegistry() *ThemeRegistry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.themes
}

// Reload re-discovers all artifacts, atomically reloads each registry,
// and then calls the plugin reloader if one is registered.
// The atomic swap ensures that reads see either the old or new state,
// never a partially updated state. Commands are re-registered after
// the registries are swapped so handlers always reference the current set.
func (l *Loader) Reload(ctx context.Context) error {
	entries, err := Walk(*l.cfg)
	if err != nil {
		return err
	}

	// Create new registries to hold the reloaded state.
	// This ensures atomic swap: readers either see the old or new state.
	newSkills := NewSkillRegistry(l.log)
	newPrompts := NewPromptRegistry(l.log)
	newThemes := NewThemeRegistry(l.log)

	// Load entries into the new registries.
	if err := newSkills.Load(entries); err != nil {
		return fmt.Errorf("load skills: %w", err)
	}
	if err := newPrompts.Load(entries); err != nil {
		return fmt.Errorf("load prompts: %w", err)
	}
	if err := newThemes.Load(entries); err != nil {
		return fmt.Errorf("load themes: %w", err)
	}

	// Atomic swap: update the fields. No reader sees partial state.
	l.mu.Lock()
	l.skills = newSkills
	l.prompts = newPrompts
	l.themes = newThemes
	l.mu.Unlock()

	// Re-register commands from the reloaded registries.
	// This also happens atomically with respect to the registry update.
	l.registerCommands()

	// Trigger plugin reload pass per the extension spec.
	if l.pluginReloader != nil {
		if err := l.pluginReloader(ctx); err != nil {
			return err
		}
	}

	return nil
}

// registerCommands registers prompts and skills into the commands registry.
// Prompts register as / (e.g., "review" from "review.md").
// Skills register as /skill:<id> (e.g., "skill:python-test-helper").
// Tier-aware: higher-tier registrations (e.g. project-local) replace lower-tier ones
// (e.g. package). Same tier uses first-wins alphabetical by source path.
func (l *Loader) registerCommands() {
	if l.commandsRegistry == nil {
		return
	}

	// Clear commands from both the previous and current artifact source paths.
	// This removes commands for deleted artifacts as well as changed ones.
	for sourcePath := range l.commandSources {
		l.commandsRegistry.ClearSource(sourcePath)
	}
	currentSources := make(map[string]struct{})

	// Register prompts as slash commands.
	for _, prompt := range l.prompts.List() {
		cmdName := prompt.ID // ID is the filename without .md
		info := commands.Info{
			Name:         cmdName,
			Description:  prompt.Description,
			ArgumentHint: prompt.ArgumentHint,
			Source:       commands.Prompt,
			SourcePath:   prompt.SourcePath,
		}
		// Create a handler that expands {{args}} and returns the result.
		// The actual dispatch (e.g., sending to LLM) is up to the caller;
		// we return the expanded content as output for now.
		handler := commands.Handler(func(ctx context.Context, args string, ui commands.CommandUI, sess commands.CommandSession) (commands.Result, error) {
			expanded, ok := l.prompts.Expand(prompt.ID, args)
			if !ok {
				return commands.Result{}, fmt.Errorf("prompt %q not found", prompt.ID)
			}
			return commands.Result{Output: expanded}, nil
		})
		if err := l.commandsRegistry.RegisterAt(info, handler, prompt.SourcePath, prompt.Tier.ToCommandsTier()); err != nil {
			if l.log != nil {
				l.log.Error("artifacts: command registration failed", "command", cmdName, "error", err)
			}
		}
		currentSources[prompt.SourcePath] = struct{}{}
	}

	// Register skills as /skill:<id> slash commands. The handler returns the
	// XML-wrapped invocation body so callers inject it as a user message
	// verbatim. Matches the TS reference _expandSkillCommand contract.
	for _, skill := range l.skills.List() {
		cmdName := "skill:" + skill.ID
		info := commands.Info{
			Name:        cmdName,
			Description: skill.Description,
			Source:      commands.Skill,
			SourcePath:  skill.SourcePath,
		}
		skillRef := skill
		handler := commands.Handler(func(ctx context.Context, args string, ui commands.CommandUI, sess commands.CommandSession) (commands.Result, error) {
			return commands.Result{Output: FormatSkillInvocation(skillRef, args)}, nil
		})
		if err := l.commandsRegistry.RegisterAt(info, handler, skill.SourcePath, skill.Tier.ToCommandsTier()); err != nil {
			if l.log != nil {
				l.log.Error("artifacts: command registration failed", "command", cmdName, "error", err)
			}
		}
		currentSources[skill.SourcePath] = struct{}{}
	}

	l.commandSources = currentSources
}
