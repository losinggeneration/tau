// Package cli implements the tau CLI using urfave/cli/v3.
package cli

import (
	"slices"

	"github.com/urfave/cli/v3"
)

// Exit codes matching the current behavior.
const (
	ExitSuccess        = 0
	ExitProviderError  = 1
	ExitUserError      = 2
	ExitAuthError      = 3
	ExitSubsystemError = 4
	ExitSIGINT         = 130
)

// RootFlags are global flags available on all commands.
var RootFlags = []cli.Flag{
	&cli.BoolFlag{
		Name:  "version",
		Usage: "Print version information",
	},
	&cli.StringFlag{
		Name:  "config-dir",
		Usage: "Tau config directory (default ~/.tau/agent; also TAU_CONFIG_DIR)",
	},
	&cli.BoolFlag{
		Name:    "verbose",
		Aliases: []string{"V"},
		Usage:   "Enable verbose output",
	},
	&cli.BoolFlag{
		Name:    "offline",
		Usage:   "Run in offline mode",
		Sources: cli.EnvVars("TAU_OFFLINE"),
	},
	&cli.StringFlag{
		Name:  "session-dir",
		Usage: "Override session directory",
	},
	&cli.BoolFlag{
		Name:  "ignore-env",
		Usage: "Skip environment variable credential sources",
	},
}

// ConversationFlags are flags for the root command (prompt mode).
var ConversationFlags = []cli.Flag{
	// Session flags
	&cli.StringFlag{
		Name:    "session",
		Aliases: []string{"s"},
		Usage:   "Specify session ID or path",
	},
	&cli.StringFlag{
		Name:  "session-id",
		Usage: "Use exact session ID (creates if missing)",
	},
	&cli.StringFlag{
		Name:    "name",
		Aliases: []string{"n"},
		Usage:   "Session display name",
	},
	&cli.BoolFlag{
		Name:    "continue",
		Aliases: []string{"c"},
		Usage:   "Continue previous session",
	},
	&cli.StringFlag{
		Name:    "resume",
		Aliases: []string{"r"},
		Usage:   "Resume a specific session by ID",
	},
	&cli.StringFlag{
		Name:    "fork",
		Aliases: []string{"f"},
		Usage:   "Fork from session ID",
	},
	&cli.BoolFlag{
		Name:  "no-session",
		Usage: "Run without creating a session (ephemeral)",
	},
	&cli.StringSliceFlag{
		Name:    "message",
		Aliases: []string{"m"},
		Usage:   "Add message to conversation (merged with positional messages)",
	},

	// Model flags
	&cli.StringSliceFlag{
		Name:    "models",
		Aliases: []string{"model", "M"},
		Usage:   "Specify models to use",
	},
	&cli.StringFlag{
		Name:  "thinking",
		Usage: "Set thinking level (off, minimal, low, medium, high, xhigh, max)",
		Validator: func(level string) error {
			valid := []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}
			if !slices.Contains(valid, level) {
				return cli.Exit("invalid thinking level: "+level, ExitUserError)
			}
			return nil
		},
	},

	// Provider / auth overrides (pi-compatible).
	&cli.StringFlag{
		Name:  "provider",
		Usage: "Provider name (requires --model or --models)",
	},
	&cli.StringFlag{
		Name:  "api-key",
		Usage: "API key for the resolved provider",
	},

	// Feature flags
	&cli.BoolFlag{
		Name:    "no-stream",
		Aliases: []string{"n"},
		Usage:   "Disable streaming output",
	},
	&cli.BoolFlag{
		Name:    "no-tools",
		Aliases: []string{"nt"},
		Usage:   "Disable all tools",
	},
	&cli.BoolFlag{
		Name:    "no-builtin-tools",
		Aliases: []string{"nbt"},
		Usage:   "Disable builtin tools",
	},
	&cli.StringSliceFlag{
		Name:    "tools",
		Aliases: []string{"t"},
		Usage:   "Comma-separated allowlist of tool names (built-in, extension, custom)",
	},
	&cli.StringSliceFlag{
		Name:    "exclude-tools",
		Aliases: []string{"xt"},
		Usage:   "Comma-separated denylist of tool names (built-in, extension, custom)",
	},

	// System prompt overrides (pi-compatible).
	&cli.StringFlag{
		Name:  "system-prompt",
		Usage: "Override the resolved system prompt",
	},
	&cli.StringSliceFlag{
		Name:  "append-system-prompt",
		Usage: "Append text or file contents to the system prompt (repeatable)",
	},

	// Project-local-file trust (pi-compatible).
	&cli.BoolFlag{
		Name:    "approve",
		Aliases: []string{"a"},
		Usage:   "Trust project-local files for this run",
	},
	&cli.BoolFlag{
		Name:    "no-approve",
		Aliases: []string{"na"},
		Usage:   "Ignore project-local files for this run",
	},
	&cli.StringSliceFlag{
		Name:    "extension",
		Aliases: []string{"e"},
		Usage:   "Load extension from path",
	},
	&cli.StringSliceFlag{
		Name:  "skill",
		Usage: "Preload skill by ID (resolved from registry after discovery)",
	},
	&cli.StringSliceFlag{
		Name:  "prompt-template",
		Usage: "Load prompt template from path",
	},
	&cli.StringSliceFlag{
		Name:  "theme",
		Usage: "Load theme from path",
	},
	&cli.BoolFlag{
		Name:    "no-extensions",
		Aliases: []string{"ne"},
		Usage:   "Disable extensions",
	},
	&cli.BoolFlag{
		Name:    "no-skills",
		Aliases: []string{"ns"},
		Usage:   "Disable skills",
	},
	&cli.BoolFlag{
		Name:    "no-prompt-templates",
		Aliases: []string{"np"},
		Usage:   "Disable prompt templates",
	},
	&cli.BoolFlag{
		Name:  "no-themes",
		Usage: "Disable themes",
	},
	&cli.BoolFlag{
		Name:    "no-context-files",
		Aliases: []string{"nc"},
		Usage:   "Disable automatic context files",
	},

	// I/O flags
	&cli.StringFlag{
		Name:  "export",
		Usage: "Export session to file",
	},
	&cli.BoolFlag{
		Name:    "list-models",
		Aliases: []string{"lm"},
		Usage:   "List available models",
	},
	&cli.StringSliceFlag{
		Name:    "image",
		Aliases: []string{"i"},
		Usage:   "Include image in conversation",
	},

	// Mode selection. --mode is the canonical mode selector; --json below
	// is an alias for --mode json. The default (no --mode set) is resolved
	// at runtime: non-TTY stdin selects print (text); TTY selects interactive.
	&cli.StringFlag{
		Name:  "mode",
		Usage: "Output mode: text, json, or rpc (default: text on non-TTY stdin, interactive on TTY)",
		Validator: func(mode string) error {
			switch mode {
			case "", "text", "json", "rpc":
				return nil
			default:
				return cli.Exit("invalid --mode: "+mode+" (expected text, json, or rpc)", ExitUserError)
			}
		},
	},
	&cli.BoolFlag{
		Name:    "print",
		Aliases: []string{"p"},
		Usage:   "Force non-interactive (print/text) mode regardless of TTY state",
	},
	// --json is an alias for --mode json. Honored only when --mode is unset
	// so an explicit --mode text is not clobbered by an accidental --json
	// from a script.
	&cli.BoolFlag{
		Name:    "json",
		Aliases: []string{"j"},
		Usage:   "Output in JSON format (alias for --mode json)",
	},
	// Stdin mode
	&cli.BoolFlag{
		Name:    "stdin",
		Aliases: []string{"S"},
		Usage:   "Read prompt from stdin",
	},
}

// LoginFlags defines flags for the login command.
var LoginFlags = []cli.Flag{
	&cli.StringFlag{
		Name:  "api-key",
		Usage: "API key for the provider",
	},
	&cli.BoolFlag{
		Name:  "api-key-stdin",
		Usage: "Read API key from stdin",
	},
	&cli.BoolFlag{
		Name:  "oauth",
		Usage: "Use OAuth authentication",
	},
	&cli.StringFlag{
		Name:  "base-url",
		Usage: "Override base URL for the provider",
	},
	&cli.StringFlag{
		Name:  "default-model",
		Usage: "Set the default model for this provider",
	},
}

// ProvidersFlags defines flags for the providers command.
var ProvidersFlags = []cli.Flag{
	&cli.BoolFlag{
		Name:  "check",
		Usage: "Validate credentials for all providers",
	},
}

// ModelsListFlags defines flags for the models list subcommand.
var ModelsListFlags = []cli.Flag{
	&cli.StringFlag{
		Name:  "provider",
		Usage: "Filter by provider",
	},
	&cli.BoolFlag{
		Name:    "all",
		Aliases: []string{"a"},
		Usage:   "List models for all providers, including providers without active credentials",
	},
}

// ModelsDiscoverFlags defines flags for the models discover subcommand.
var ModelsDiscoverFlags = []cli.Flag{
	&cli.BoolFlag{
		Name:  "save",
		Usage: "Persist discovered models to models.json",
	},
}

// InstallFlags defines flags for the install command.
var InstallFlags = []cli.Flag{
	&cli.BoolFlag{
		Name:  "project",
		Usage: "Install in project scope",
	},
	&cli.BoolFlag{
		Name:  "link",
		Usage: "Create symbolic link instead of copying",
	},
	&cli.BoolFlag{
		Name:    "yes",
		Aliases: []string{"y"},
		Usage:   "Skip confirmation prompts",
	},
	&cli.BoolFlag{
		Name:    "force",
		Aliases: []string{"f"},
		Usage:   "Force overwrite existing installation",
	},
	&cli.BoolFlag{
		Name:  "pi",
		Usage: "Install a pi extension package through pi-ext-shim",
	},
}

// UninstallFlags defines flags for the uninstall command.
var UninstallFlags = []cli.Flag{
	&cli.BoolFlag{
		Name:  "project",
		Usage: "Uninstall from project scope",
	},
	&cli.BoolFlag{
		Name:  "user",
		Usage: "Uninstall from user scope",
	},
}

// UpdateFlags defines flags for the update command.
var UpdateFlags = []cli.Flag{
	&cli.BoolFlag{
		Name:  "project",
		Usage: "Update packages in project scope",
	},
}

// PackagesListFlags defines flags for the packages list subcommand.
var PackagesListFlags = []cli.Flag{
	&cli.BoolFlag{
		Name:  "user",
		Usage: "Show only user-scoped packages",
	},
	&cli.BoolFlag{
		Name:  "project",
		Usage: "Show only project-scoped packages",
	},
}

// SetModelFlags defines flags for the set-model command.
var SetModelFlags = []cli.Flag{
	&cli.BoolFlag{
		Name:  "project",
		Usage: "Set model in project scope",
	},
}

// SessionsListFlags defines flags for the sessions list subcommand.
var SessionsListFlags = []cli.Flag{
	&cli.IntFlag{
		Name:  "limit",
		Usage: "Limit number of sessions to list",
	},
	&cli.StringFlag{
		Name:  "since",
		Usage: "Show sessions since date",
	},
	&cli.StringFlag{
		Name:  "cwd",
		Usage: "Show sessions for this working directory",
	},
}

// SessionsCleanupEmptyFlags defines flags for the sessions cleanup-empty subcommand.
var SessionsCleanupEmptyFlags = []cli.Flag{
	&cli.BoolFlag{
		Name:  "dry-run",
		Usage: "List sessions that would be moved without changing files",
	},
}

// SessionsForkFlags defines flags for the sessions fork subcommand.
var SessionsForkFlags = []cli.Flag{
	&cli.StringFlag{
		Name:  "at",
		Usage: "Fork at specific entry ID",
	},
}

// SessionsExportFlags defines flags for the sessions export subcommand.
var SessionsExportFlags = []cli.Flag{
	&cli.StringFlag{
		Name:  "format",
		Usage: "Export format (html, json, md)",
	},
	&cli.StringFlag{
		Name:  "output",
		Usage: "Output file path",
	},
	&cli.BoolFlag{
		Name:  "no-collapse",
		Usage: "Expand all collapsible sections in HTML export (branch summaries, etc.)",
	},
}

// SessionsShareFlags defines flags for the sessions share subcommand.
var SessionsShareFlags = []cli.Flag{
	&cli.BoolFlag{
		Name:  "public",
		Usage: "Make the share public",
	},
}

// Prompt flags that should trigger TTY behavior.
var PromptFlags = []cli.Flag{
	&cli.BoolFlag{
		Name:    "list-models",
		Aliases: []string{"lm"},
		Usage:   "List available models",
	},
}

// --- Artifact list flags ---

// SkillsListFlags defines flags for the skills list subcommand.
var SkillsListFlags = []cli.Flag{
	&cli.StringFlag{
		Name:    "tier",
		Aliases: []string{"source"},
		Usage:   "Filter by source tier (cli, project, user, settings, packages, builtin)",
	},
	&cli.BoolFlag{
		Name:  "no-builtin",
		Usage: "Exclude built-in skills from results",
	},
}

// PromptsListFlags defines flags for the prompts list subcommand.
var PromptsListFlags = []cli.Flag{
	&cli.StringFlag{
		Name:    "tier",
		Aliases: []string{"source"},
		Usage:   "Filter by source tier (cli, project, user, settings, packages, builtin)",
	},
}

// ThemesListFlags defines flags for the themes list subcommand.
var ThemesListFlags = []cli.Flag{
	&cli.StringFlag{
		Name:    "tier",
		Aliases: []string{"source"},
		Usage:   "Filter by source tier (cli, project, user, settings, packages, builtin)",
	},
}
