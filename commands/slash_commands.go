// Package commands defines the CommandRegistry that artifacts, extensions,
// and session specs forward-reference. Owners register slash commands
// (built-in, prompts, skills, plugins) and the registry dispatches them.
package commands // import "github.com/losinggeneration/tau/commands"

// This file defines the built-in slash command metadata, mirroring the
// TypeScript BUILTIN_SLASH_COMMANDS array in pi/coding-agent/src/core/slash-commands.ts.
//
// Decision Rationale for Each Command:
//
// Commands that are implemented in Go (via CLI subcommands or runtime):
// - login/logout: Available via CLI subcommands and handled at the runtime level
// - export: Available via sessions export subcommand
// - share: Available via sessions share subcommand
// - compact: Available via sessions compact subcommand
// - fork: Available via SDK and sessions fork subcommand
// - resume: Available via sessions resume subcommand
// - reload: Available via reload CLI command and runtime
// - new: Implicit in Go - start a new conversation without session context
// - quit: Implicit in Go - exit the TUI or conversation mode
//
// Commands intentionally out of scope for Go slash-command surface:
// These are primarily TUI/interactive-mode features that don't map well to
// Go's headless or CLI-driven execution model:
//
// - /settings: Interactive TUI settings - Go uses CLI flags and settings file
// - /model: Interactive TUI model selector - Go uses CLI flags (--model) or settings
// - /scoped-models: TUI feature for Ctrl+P cycling - Go doesn't have this TUI model
// - /session: TUI session info - Available via 'sessions show' subcommand
// - /changelog: TUI changelog display - Available via 'tau --version' or changelog file
// - /hotkeys: TUI keybindings display - Not applicable to Go's execution model
// - /tree: TUI tree navigation - Available via 'sessions' subcommands
// - /import: TUI import feature - Available via 'sessions restore' subcommand
// - /copy: TUI clipboard copy - Available via OS clipboard tools or 'sessions export'
// - /name: TUI session naming - Available via 'sessions rename' subcommand

import (
	"slices"
	"strings"
)

// BuiltinSlashCommand describes a built-in slash command's metadata.
// This mirrors the TypeScript BuiltinSlashCommand interface.
type BuiltinSlashCommand struct {
	// Name is the slash-command name (without the leading /).
	Name string
	// Description is a short human-readable summary.
	Description string
	// Implemented indicates whether this command has a Go implementation.
	// If false, the command is intentionally not supported.
	Implemented bool
	// Note explains the implementation decision for this command.
	Note string
}

// BUILTIN_SLASH_COMMANDS lists all built-in slash commands that exist in
// the TypeScript implementation (pi/coding-agent).
//
// This serves as documentation of the TS surface and guides Go parity decisions.
// Commands marked as Implemented=false are intentionally not supported in Go
// because they are TUI-specific features that don't map to Go's execution model.
//
// Reference: pi/coding-agent/src/core/slash-commands.ts
var BUILTIN_SLASH_COMMANDS = []BuiltinSlashCommand{
	{
		Name:        "settings",
		Description: "Open settings menu",
		Implemented: false,
		Note:        "TUI-specific interactive settings. Go uses CLI flags and settings file.",
	},
	{
		Name:        "model",
		Description: "Select model (opens selector UI)",
		Implemented: false,
		Note:        "TUI-specific model selector. Go uses --model flag or settings.",
	},
	{
		Name:        "scoped-models",
		Description: "Enable/disable models for Ctrl+P cycling",
		Implemented: false,
		Note:        "TUI-specific Ctrl+P cycling feature. Not applicable to Go.",
	},
	{
		Name:        "export",
		Description: "Export session (HTML default, or specify path: .html/.jsonl)",
		Implemented: true,
		Note:        "Available via 'tau sessions export' subcommand.",
	},
	{
		Name:        "import",
		Description: "Import and resume a session from a JSONL file",
		Implemented: true,
		Note:        "Available via 'tau sessions restore' subcommand.",
	},
	{
		Name:        "share",
		Description: "Share session as a secret GitHub gist",
		Implemented: true,
		Note:        "Available via 'tau sessions share' subcommand.",
	},
	{
		Name:        "copy",
		Description: "Copy last agent message to clipboard",
		Implemented: false,
		Note:        "TUI-specific clipboard feature. Use terminal clipboard or export.",
	},
	{
		Name:        "name",
		Description: "Set session display name",
		Implemented: true,
		Note:        "Available via 'tau sessions rename' subcommand.",
	},
	{
		Name:        "session",
		Description: "Show session info and stats",
		Implemented: true,
		Note:        "Available via 'tau sessions show' subcommand.",
	},
	{
		Name:        "changelog",
		Description: "Show changelog entries",
		Implemented: false,
		Note:        "Available via 'tau --version' or reading CHANGELOG.md directly.",
	},
	{
		Name:        "hotkeys",
		Description: "Show all keyboard shortcuts",
		Implemented: false,
		Note:        "TUI-specific keybindings display. Not applicable to Go CLI.",
	},
	{
		Name:        "fork",
		Description: "Create a new fork from a previous message",
		Implemented: true,
		Note:        "Available via SDK Session.Fork and 'tau sessions fork' subcommand.",
	},
	{
		Name:        "tree",
		Description: "Navigate session tree (switch branches)",
		Implemented: true,
		Note:        "Available via 'tau sessions' subcommands (list, show, resume).",
	},
	{
		Name:        "login",
		Description: "Login with OAuth provider",
		Implemented: true,
		Note:        "Available via 'tau login' subcommand and runtime OAuth flow.",
	},
	{
		Name:        "logout",
		Description: "Logout from OAuth provider",
		Implemented: true,
		Note:        "Available via 'tau logout' subcommand.",
	},
	{
		Name:        "new",
		Description: "Start a new session",
		Implemented: true,
		Note:        "Implicit in Go - start tau without session argument for new conversation.",
	},
	{
		Name:        "compact",
		Description: "Manually compact the session context",
		Implemented: true,
		Note:        "Available via 'tau sessions compact' subcommand.",
	},
	{
		Name:        "resume",
		Description: "Resume a different session",
		Implemented: true,
		Note:        "Available via 'tau sessions resume' subcommand.",
	},
	{
		Name:        "reload",
		Description: "Reload keybindings, extensions, skills, prompts, and themes",
		Implemented: true,
		Note:        "Available via 'tau reload' command and runtime reload API.",
	},
	{
		Name:        "quit",
		Description: "Quit tau",
		Implemented: true,
		Note:        "Exit TUI or conversation mode. Implicit - Ctrl+C or session end.",
	},
}

// LookupBuiltinCommand finds a built-in slash command by name.
// Returns nil if the command is not found.
func LookupBuiltinCommand(name string) *BuiltinSlashCommand {
	for _, cmd := range BUILTIN_SLASH_COMMANDS {
		if cmd.Name == name {
			return &BuiltinSlashCommand{
				Name:        cmd.Name,
				Description: cmd.Description,
				Implemented: cmd.Implemented,
				Note:        cmd.Note,
			}
		}
	}
	return nil
}

// ListBuiltinCommands returns all built-in slash commands as a slice.
func ListBuiltinCommands() []BuiltinSlashCommand {
	cmds := make([]BuiltinSlashCommand, 0, 20)
	for _, cmd := range BUILTIN_SLASH_COMMANDS {
		cmds = append(cmds, BuiltinSlashCommand{
			Name:        cmd.Name,
			Description: cmd.Description,
			Implemented: cmd.Implemented,
			Note:        cmd.Note,
		})
	}
	slices.SortFunc(cmds, compareBuiltinCommands)
	return cmds
}

// ListImplementedCommands returns only the implemented built-in commands.
func ListImplementedCommands() []BuiltinSlashCommand {
	cmds := make([]BuiltinSlashCommand, 0, 12)
	for _, cmd := range BUILTIN_SLASH_COMMANDS {
		if cmd.Implemented {
			cmds = append(cmds, BuiltinSlashCommand{
				Name:        cmd.Name,
				Description: cmd.Description,
				Implemented: cmd.Implemented,
				Note:        cmd.Note,
			})
		}
	}
	slices.SortFunc(cmds, compareBuiltinCommands)
	return cmds
}

func compareBuiltinCommands(a, b BuiltinSlashCommand) int {
	if a.Implemented != b.Implemented {
		// Implemented commands first
		if a.Implemented {
			return -1
		}
		return 1
	}
	return strings.Compare(a.Name, b.Name)
}
