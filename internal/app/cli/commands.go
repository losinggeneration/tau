// Package cli implements the tau CLI using urfave/cli/v3.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/losinggeneration/tau/ai"
	"github.com/urfave/cli/v3"
)

// rootCmdKey is used to store the root command in context for flag access.
type rootCmdKey struct{}

// rootPromptDelimiterKey records whether the raw root argv contained `--`.
// urfave/cli strips the delimiter before runRoot sees cmd.Args(), but we need
// it to distinguish explicit multi-word prompt text (`tau -- model list`) from
// typo-shaped subcommand invocations (`tau model list`).
type rootPromptDelimiterKey struct{}

// Root command.
var rootCmd = &cli.Command{
	Name:      "tau",
	Usage:     "AI coding agent",
	ArgsUsage: "[prompt] [--message/-m <message>] [--]",
	Action:    runRoot,

	Flags: append(RootFlags, ConversationFlags...),
}

// Login command.
var loginCmd = &cli.Command{
	Name:      "login",
	Usage:     "Log in to an AI provider",
	ArgsUsage: "<provider>",
	Description: `Authenticate with an AI provider using API key or OAuth.
		
		Examples:
		  tau login anthropic
		  tau login openai --api-key sk-...
		  tau login anthropic --oauth`,
	Flags:  LoginFlags,
	Action: runLogin,
}

// Logout command.
var logoutCmd = &cli.Command{
	Name:      "logout",
	Usage:     "Log out from an AI provider",
	ArgsUsage: "<provider>",
	Action:    runLogout,
}

// Providers command.
var providersCmd = &cli.Command{
	Name:  "providers",
	Usage: "List configured providers",
	Description: `List all configured credential providers.
		
		Use --check to validate credentials for all providers.`,
	Flags:  ProvidersFlags,
	Action: runProviders,
}

// Models command with subcommands.
var modelsCmd = &cli.Command{
	Name:    "models",
	Aliases: []string{"model"},
	Usage:   "Manage AI models",
	Commands: []*cli.Command{
		{
			Name:   "list",
			Usage:  "List configured models",
			Flags:  ModelsListFlags,
			Action: runModelsList,
		},
		{
			Name:      "discover",
			Usage:     "Discover available models",
			ArgsUsage: "<provider>",
			Flags:     ModelsDiscoverFlags,
			Action:    runModelsDiscover,
		},
	},
}

// SetModel command.
var setModelCmd = &cli.Command{
	Name:      "set-model",
	Usage:     "Set the default model",
	ArgsUsage: "<model-spec>",
	Description: `Set the default model for conversations.
		
		Model spec format: <provider>/<model>
		Examples:
		  tau set-model anthropic/claude-sonnet-4
		  tau set-model openai/gpt-4o --project`,
	Flags:  SetModelFlags,
	Action: runSetModel,
}

// ShowModel command.
var showModelCmd = &cli.Command{
	Name:   "show-model",
	Usage:  "Show the current model configuration",
	Action: runShowModel,
}

// Install command.
var installCmd = &cli.Command{
	Name:      "install",
	Usage:     "Install a package",
	ArgsUsage: "<source>",
	Description: `Install a package from a source.
		
		Examples:
		  tau install github.com/user/skill
		  tau install ./local-skill --link`,
	Flags:  InstallFlags,
	Action: runInstall,
}

// Uninstall command.
var uninstallCmd = &cli.Command{
	Name:      "uninstall",
	Usage:     "Uninstall a package",
	ArgsUsage: "<name>",
	Description: `Uninstall a package by name.

		If installed in multiple scopes, use --project or --user to specify.`,
	Flags:  UninstallFlags,
	Action: runUninstall,
}

// Remove command (pi-compatible alias for uninstall).
var removeCmd = &cli.Command{
	Name:      "remove",
	Usage:     "Remove a package (alias for uninstall)",
	ArgsUsage: "<name>",
	Description: `Remove a package by name.

		Alias for 'uninstall' to match the pi CLI surface.`,
	Flags:  UninstallFlags,
	Action: runUninstall,
}

// List command (pi-compatible alias for `extensions list`).
var listCmd = &cli.Command{
	Name:   "list",
	Usage:  "List installed extensions (alias for `extensions list`)",
	Action: runExtensionsList,
}

// Update command.
var updateCmd = &cli.Command{
	Name:      "update",
	Usage:     "Update installed packages (or self with `update self`/`update pi`)",
	ArgsUsage: "[<name>...|self|pi]",
	Description: `Update one or more packages.

		If no names specified, updates all installed packages. Use 'self' or 'pi'
		to update the tau binary itself (pi-compatible surface).`,
	Flags:  UpdateFlags,
	Action: runUpdate,
	Commands: []*cli.Command{
		{
			Name:   "self",
			Usage:  "Update the tau binary itself",
			Action: runUpdateSelf,
		},
		{
			Name:   "pi",
			Usage:  "Update the tau binary itself (alias for `update self`)",
			Action: runUpdateSelf,
		},
	},
}

// Packages command with subcommands.
var packagesCmd = &cli.Command{
	Name:  "packages",
	Usage: "Manage packages",
	Commands: []*cli.Command{
		{
			Name:   "list",
			Usage:  "List installed packages",
			Flags:  PackagesListFlags,
			Action: runPackagesList,
		},
		{
			Name:      "info",
			Usage:     "Show package information",
			ArgsUsage: "<name>",
			Action:    runPackagesInfo,
		},
		{
			Name:      "search",
			Usage:     "Search installed packages",
			ArgsUsage: "<query>",
			Action:    runPackagesSearch,
		},
	},
}

// Sessions command with subcommands.
var sessionsCmd = &cli.Command{
	Name:  "sessions",
	Usage: "Manage sessions",
	Commands: []*cli.Command{
		{
			Name:   "list",
			Usage:  "List sessions",
			Flags:  SessionsListFlags,
			Action: runSessionsList,
		},
		{
			Name:      "show",
			Usage:     "Show session details",
			ArgsUsage: "<id>",
			Action:    runSessionsShow,
		},
		{
			Name:      "resume",
			Usage:     "Resume a session",
			ArgsUsage: "<id>",
			Action:    runSessionsResume,
		},
		{
			Name:      "rename",
			Usage:     "Rename a session",
			ArgsUsage: "<id> <new-name>",
			Action:    runSessionsRename,
		},
		{
			Name:      "move",
			Usage:     "Move a session to a new working directory",
			ArgsUsage: "<id> <cwd>",
			Action:    runSessionsMove,
		},
		{
			Name:      "delete",
			Usage:     "Delete a session",
			ArgsUsage: "<id>",
			Action:    runSessionsDelete,
		},
		{
			Name:   "cleanup-empty",
			Usage:  "Move sessions without messages to trash",
			Flags:  SessionsCleanupEmptyFlags,
			Action: runSessionsCleanupEmpty,
		},
		{
			Name:      "restore",
			Usage:     "Restore a session from trash",
			ArgsUsage: "<id>",
			Action:    runSessionsRestore,
		},
		{
			Name:      "compact",
			Usage:     "Compact a session",
			ArgsUsage: "<id>",
			Action:    runSessionsCompact,
		},
		{
			Name:      "summarize-branch",
			Usage:     "Summarize an abandoned branch",
			ArgsUsage: "<id> <branch-tip>",
			Action:    runSessionsSummarizeBranch,
		},
		{
			Name:      "fork",
			Usage:     "Fork a session",
			ArgsUsage: "<id>",
			Flags:     SessionsForkFlags,
			Action:    runSessionsFork,
		},
		{
			Name:      "export",
			Usage:     "Export a session",
			ArgsUsage: "<id>",
			Flags:     SessionsExportFlags,
			Action:    runSessionsExport,
		},
	},
}

// RPC command.
var rpcCmd = &cli.Command{
	Name:   "rpc",
	Usage:  "Start RPC server",
	Action: runRPC,
}

var workerCmd = &cli.Command{
	Name:   "worker",
	Hidden: true,
	Flags:  []cli.Flag{&cli.StringFlag{Name: "socket", Usage: "Coordinator socket path"}},
	Action: runWorker,
}

var daemonCmd = &cli.Command{
	Name:  "daemon",
	Usage: "Start the local instance coordinator",
	Flags: []cli.Flag{
		&cli.StringFlag{Name: "socket", Usage: "Unix socket path"},
		&cli.BoolFlag{Name: "legacy", Usage: "Use connection-owned legacy RPC instead of coordinated instances"},
		&cli.StringFlag{Name: "websocket", Usage: "Opt-in WebSocket bridge address (host:port)"},
		&cli.StringFlag{Name: "websocket-token", Usage: "Static bearer token for the WebSocket bridge", Sources: cli.EnvVars("TAU_WEBSOCKET_TOKEN")},
		&cli.BoolFlag{Name: "systemd-user-unit", Usage: "Print a systemd user unit"},
	},
	Commands: []*cli.Command{
		{
			Name:  "token",
			Usage: "Manage per-device tokens for the WebSocket bridge",
			Commands: []*cli.Command{
				{
					Name:      "add",
					Usage:     "Create a token for a remote device",
					ArgsUsage: "<device-name>",
					Action:    runDaemonTokenAdd,
				},
				{
					Name:    "list",
					Usage:   "List device tokens",
					Aliases: []string{"ls"},
					Action:  runDaemonTokenList,
				},
				{
					Name:      "revoke",
					Usage:     "Revoke a device token",
					ArgsUsage: "<device-name>",
					Action:    runDaemonTokenRevoke,
				},
			},
		},
	},
	Action: runDaemon,
}

var doctorCmd = &cli.Command{
	Name:   "doctor",
	Usage:  "Show local tau status",
	Action: runDoctor,
}

// Reload command.
var reloadCmd = &cli.Command{
	Name:   "reload",
	Usage:  "Reload artifacts and plugins",
	Action: runReload,
}

// Skills command.
var skillsCmd = &cli.Command{
	Name:  "skills",
	Usage: "Manage skills",
	Commands: []*cli.Command{
		{
			Name:   "list",
			Usage:  "List available skills",
			Flags:  SkillsListFlags,
			Action: runSkillsList,
		},
		{
			Name:      "show",
			Usage:     "Show skill details",
			ArgsUsage: "<skill-id>",
			Action:    runSkillsShow,
		},
	},
}

// Prompts command.
var promptsCmd = &cli.Command{
	Name:  "prompts",
	Usage: "Manage prompt templates",
	Commands: []*cli.Command{
		{
			Name:   "list",
			Usage:  "List available prompt templates",
			Flags:  PromptsListFlags,
			Action: runPromptsList,
		},
		{
			Name:      "show",
			Usage:     "Show prompt template details",
			ArgsUsage: "<prompt-id>",
			Action:    runPromptsShow,
		},
	},
}

// Themes command.
var themesCmd = &cli.Command{
	Name:  "themes",
	Usage: "Manage themes",
	Commands: []*cli.Command{
		{
			Name:   "list",
			Usage:  "List available themes",
			Flags:  ThemesListFlags,
			Action: runThemesList,
		},
		{
			Name:      "show",
			Usage:     "Show theme details",
			ArgsUsage: "<theme-id>",
			Action:    runThemesShow,
		},
	},
}

// Plugin command.
var pluginCmd = &cli.Command{
	Name:  "plugin",
	Usage: "Plugin management commands",
	Commands: []*cli.Command{
		{
			Name:   "state",
			Usage:  "Show plugin state",
			Action: runPluginState,
		},
		{
			Name:      "doctor",
			Usage:     "Validate a plugin binary",
			ArgsUsage: "<path>",
			Action:    runPluginDoctor,
		},
	},
}

// All commands registered with the root.
var Commands = []*cli.Command{
	loginCmd,
	logoutCmd,
	providersCmd,
	modelsCmd,
	setModelCmd,
	showModelCmd,
	installCmd,
	uninstallCmd,
	removeCmd,
	listCmd,
	updateCmd,
	packagesCmd,
	sessionsCmd,
	rpcCmd,
	daemonCmd,
	workerCmd,
	doctorCmd,
	reloadCmd,
	skillsCmd,
	promptsCmd,
	themesCmd,
	pluginCmd,
}

func newRootCommand() *cli.Command {
	cmd := cloneCommand(rootCmd)
	cmd.Commands = cloneCommands(Commands)
	return cmd
}

func cloneCommands(in []*cli.Command) []*cli.Command {
	out := make([]*cli.Command, 0, len(in))
	for _, cmd := range in {
		out = append(out, cloneCommand(cmd))
	}
	return out
}

func cloneCommand(src *cli.Command) *cli.Command {
	if src == nil {
		return nil
	}
	cp := *src
	cp.Flags = cloneCLIFlags(src.Flags)
	cp.Commands = cloneCommands(src.Commands)
	return &cp
}

func cloneCLIFlags(in []cli.Flag) []cli.Flag {
	out := make([]cli.Flag, 0, len(in))
	for _, f := range in {
		switch src := f.(type) {
		case *cli.StringFlag:
			cp := *src
			out = append(out, &cp)
		case *cli.BoolFlag:
			cp := *src
			out = append(out, &cp)
		case *cli.StringSliceFlag:
			cp := *src
			out = append(out, &cp)
		case *cli.IntFlag:
			cp := *src
			out = append(out, &cp)
		default:
			out = append(out, f)
		}
	}
	return out
}

func hasExplicitPromptDelimiter(args []string) bool {
	for _, arg := range args[1:] {
		if arg == "--" {
			return true
		}
	}
	return false
}

func normalizeOptionalResumeArgs(args []string) []string {
	out := append([]string(nil), args...)
	for i := 1; i < len(out); i++ {
		switch out[i] {
		case "--resume", "-r":
			if i+1 >= len(out) || strings.HasPrefix(out[i+1], "-") {
				out[i] = "--resume="
			}
		}
	}
	return out
}

func resumeFlagWithoutID(cmd *cli.Command) bool {
	return cmd != nil && cmd.IsSet("resume") && strings.TrimSpace(cmd.String("resume")) == ""
}

func resumeSelectionRequested(cmd *cli.Command) bool {
	return resumeFlagWithoutID(cmd) && cmd.String("session") == "" && cmd.String("session-id") == "" && !cmd.Bool("continue") && !cmd.Bool("no-session")
}

func rootPromptDelimiterFromContext(ctx context.Context) bool {
	value, _ := ctx.Value(rootPromptDelimiterKey{}).(bool)
	return value
}

func explicitPromptMode(cmd *cli.Command) bool {
	if cmd == nil {
		return false
	}
	return cmd.IsSet("print") || cmd.IsSet("json") || cmd.IsSet("mode") || cmd.IsSet("message") || cmd.IsSet("stdin")
}

func rejectCommandShapedRootArgs(ctx context.Context, cmd *cli.Command, args []string) error {
	if len(args) <= 1 || rootPromptDelimiterFromContext(ctx) || explicitPromptMode(cmd) {
		return nil
	}
	return cli.Exit(
		fmt.Sprintf("unknown command %q; use `tau -- %s` to send this as prompt text", args[0], strings.Join(args, " ")),
		ExitUserError,
	)
}

// runRoot is the root command action that handles default prompt mode.
func runRoot(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	stdinIsTTY := isTTY()

	// Handle --version flag
	if cmd.Bool("version") {
		version := pluginsVersion()
		if _, err := fmt.Fprintln(cmd.Root().Writer, "tau version", version); err != nil {
			return fmt.Errorf("write version: %w", err)
		}
		return nil
	}

	// Handle --list-models flag (can be standalone)
	if cmd.Bool("list-models") {
		var search string
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			search = args[0]
		}
		code := runListModels(ctx, search)
		if code != 0 {
			return cli.Exit("", code)
		}
		return nil
	}

	if err := rejectCommandShapedRootArgs(ctx, cmd, args); err != nil {
		return err
	}

	// Build prompt from positional args and --message flags. Positional
	// @file references are expanded into the first prompt's text and the
	// extracted image content blocks are returned alongside.
	messages := cmd.StringSlice("message")

	mode := resolveAppMode(cmd, stdinIsTTY)
	var prompts []string
	var fileImages []ai.ContentBlock
	if mode == "rpc" {
		if err := validateRpcInput(mode, append(append([]string(nil), args...), messages...), nil); err != nil {
			return cli.Exit(err.Error(), ExitUserError)
		}
	} else {
		var err error
		prompts, fileImages, err = buildPromptSequenceWithFiles(args, messages, os.Stdin, stdinIsTTY, cmd.Bool("stdin"))
		if err != nil {
			return cli.Exit(fmt.Sprintf("read stdin: %v", err), ExitUserError)
		}
	}

	// --fork targets an existing session and is mutually exclusive with
	// any flag that also targets a session. Reported before session-creation
	// work so the failure is cheap and obvious.
	if err := validateForkConflict(cmd); err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}

	switch mode {
	case "rpc":
		// --mode rpc routes to the same RPC server as `tau rpc`.
		return runRPC(ctx, cmd)
	case "json":
		code := runConversationJSONWithSignal(ctx, nil, prompts, fileImages)
		if code != 0 {
			return cli.Exit("", code)
		}
		return nil
	case "text":
		code := runConversationWithSignal(ctx, nil, prompts, fileImages)
		if code != 0 {
			return cli.Exit("", code)
		}
		return nil
	default:
		// mode == "interactive"
		code := runInteractive(ctx, cmd, prompts)
		if code != 0 {
			return cli.Exit("", code)
		}
		return nil
	}
}

// resolveAppMode returns the AppMode selected by the parsed flags.
// Precedence:
//
//  1. --mode rpc   -> rpc
//  2. --mode json  -> json
//  3. --print, or non-TTY stdin (or --stdin), with no explicit --mode,
//     selects print/text. --json is an alias for --mode json.
//  4. else (TTY with no --print) -> interactive.
//
// Invalid --mode values are rejected by the flag validator at parse time.
// --json is honored only when --mode is unset, so an explicit --mode text
// is not clobbered by an accidental --json from a script.
func resolveAppMode(cmd *cli.Command, stdinIsTTY bool) string {
	if mode := cmd.String("mode"); mode != "" {
		switch mode {
		case "rpc":
			return "rpc"
		case "json":
			return "json"
		case "text":
			return "text"
		}
	}
	if cmd.Bool("json") {
		return "json"
	}
	if resumeSelectionRequested(cmd) {
		return "interactive"
	}
	// --print (or non-TTY stdin, or explicit --stdin) forces print/text mode.
	if cmd.Bool("print") || !stdinIsTTY || cmd.Bool("stdin") {
		return "text"
	}
	return "interactive"
}

// validateForkConflict enforces --fork's mutual exclusion with
// --session, --session-id, --resume, --continue, and --no-session.
// --fork targets an existing session to fork from, so it cannot be
// combined with any flag that also selects or creates a session.
func validateForkConflict(cmd *cli.Command) error {
	if cmd.String("fork") == "" {
		return nil
	}
	if cmd.String("session") != "" {
		return fmt.Errorf("--fork cannot be combined with --session")
	}
	if cmd.String("session-id") != "" {
		return fmt.Errorf("--fork cannot be combined with --session-id")
	}
	if cmd.IsSet("resume") {
		return fmt.Errorf("--fork cannot be combined with --resume")
	}
	if cmd.Bool("continue") {
		return fmt.Errorf("--fork cannot be combined with --continue")
	}
	if cmd.Bool("no-session") {
		return fmt.Errorf("--fork cannot be combined with --no-session")
	}
	return nil
}

// validateRpcInput returns an error if the resolved mode is rpc and the
// caller has supplied input that RPC cannot consume. RPC is a long-lived
// server: positional prompts are dropped on the floor (no completion is
// running to deliver them to), and @file attachments cannot be expanded
// because clients should send content through JSON-RPC commands rather
// than CLI arguments. Both classes of input are user errors and must be
// reported before the server starts.
func validateRpcInput(mode string, prompts []string, fileImages []ai.ContentBlock) error {
	if mode != "rpc" {
		return nil
	}
	if len(prompts) > 0 {
		return fmt.Errorf(
			"--mode rpc does not accept positional prompts; " +
				"send prompts via JSON-RPC after the server starts",
		)
	}
	if len(fileImages) > 0 {
		return fmt.Errorf(
			"--mode rpc does not accept @file attachments; " +
				"send content via JSON-RPC after the server starts",
		)
	}
	return nil
}

// listModelsSearchOperand returns the search string for --list-models,
// or "" if the first positional is missing or shaped like a flag. The
// search term is a fuzzy-match pattern; flag-shaped args (e.g. "-v")
// are not search terms and are left for urfave/cli to parse.
func listModelsSearchOperand(args []string) string {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0]
	}
	return ""
}

func buildPromptSequence(args []string, messages []string, stdin io.Reader, stdinIsTTY bool, forceReadStdin bool) ([]string, error) {
	prompts, _, err := buildPromptSequenceWithFiles(args, messages, stdin, stdinIsTTY, forceReadStdin)
	return prompts, err
}

// buildPromptSequenceWithFiles composes the prompt list with positional
// @<file|dir> expansion. The first prompt is built by concatenating, in order,
// (1) piped stdin content, (2) the wrapped content of any @file positional
// arguments, (3) the first non-@file positional message (or the first -m
// message if no plain positional is present). All parts are joined with no
// inserted separator. The remaining positional and -m messages are sent
// sequentially as separate prompts.
//
// @file paths resolve relative to the current working directory at the
// time of expansion. Absolute paths and parent-relative paths (`../`,
// `../../`) are honored. Directory references expand to a file list, not
// the contents of every file in the tree. Image files (by extension)
// return a content block in addition to the in-prompt text marker.
// Missing or unreadable @file arguments are fatal.
func buildPromptSequenceWithFiles(args []string, messages []string, stdin io.Reader, stdinIsTTY bool, forceReadStdin bool) ([]string, []ai.ContentBlock, error) {
	var stdinContent string
	if forceReadStdin || !stdinIsTTY {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return nil, nil, err
		}
		stdinContent = strings.TrimFunc(string(data), func(r rune) bool { return unicode.IsSpace(r) && r != '\u0085' || r == '\ufeff' })
	}

	// Split positionals into @file refs and plain text, preserving order.
	fileArgs, plainArgs := splitFileArgs(args)

	// Expand each @file arg into wrapped text content. The concatenated
	// text is the file-content segment of the initial prompt. Images
	// collected from any image-typed @file args are returned alongside
	// so callers can attach them to the first user message.
	var fileContent strings.Builder
	var fileImages []ai.ContentBlock
	for _, arg := range fileArgs {
		text, images, err := expandFileArg(arg)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", arg, err)
		}
		fileContent.WriteString(text)
		fileImages = append(fileImages, images...)
	}

	// The first prompt is the concatenation of stdin, @file text, and the
	// first plain positional/-m message. The remaining plain messages are
	// sent sequentially as separate prompts.
	initial := stdinContent + fileContent.String()
	allMessages := append(append([]string(nil), plainArgs...), messages...)
	if len(allMessages) > 0 {
		initial += allMessages[0]
	}
	var prompts []string
	if initial != "" {
		prompts = append(prompts, initial)
	}
	if len(allMessages) > 1 {
		prompts = append(prompts, allMessages[1:]...)
	}

	return prompts, fileImages, nil
}

// splitFileArgs partitions args into @<file|dir> references and plain text
// args. An @ arg is any argument whose first character is '@'. The
// '@' is stripped from the returned path; everything else is treated
// as plain text and preserved in order.
func splitFileArgs(args []string) (fileArgs []string, plainArgs []string) {
	for _, arg := range args {
		if strings.HasPrefix(arg, "@") {
			fileArgs = append(fileArgs, strings.TrimPrefix(arg, "@"))
		} else {
			plainArgs = append(plainArgs, arg)
		}
	}
	return fileArgs, plainArgs
}

// expandFileArg reads a single @file path and returns the wrapped text
// content plus any image content blocks detected by extension. The text
// is wrapped in <file name="<abs-path>">…</file> so the model can see
// the file content with its absolute path as the identifier. Image files
// return a content block (via ai.LoadImageContentBlock) and a placeholder
// text marker; the absolute path is used for both. Path resolution
// expands '~' and resolves to an absolute path before the wrapper is
// emitted. A missing or unreadable file is fatal.
func expandFileArg(path string) (string, []ai.ContentBlock, error) {
	if path == "" {
		return "", nil, fmt.Errorf("empty @file reference")
	}
	// Expand '~' to the user's home directory.
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	// Resolve to an absolute path so the wrapper's name attribute is
	// unambiguous regardless of the caller's working directory.
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", nil, fmt.Errorf("resolve %s: %w", path, err)
	}
	// Stat before reading so the error is a clean "not found" rather
	// than a generic read error from os.ReadFile.
	if _, err := os.Stat(absPath); err != nil {
		return "", nil, err
	}
	ext := strings.ToLower(filepath.Ext(absPath))
	if isImageExt(ext) {
		marker := fmt.Sprintf("<file name=%q>[image content]</file>\n", absPath)
		return marker, []ai.ContentBlock{{Type: ai.ContentBlockImage, Name: absPath}}, nil
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", nil, fmt.Errorf("read %s: %w", absPath, err)
	}
	wrapped := fmt.Sprintf("<file name=%q>\n%s\n</file>\n", absPath, string(data))
	return wrapped, nil, nil
}

// isImageExt reports whether the given lowercase file extension is one of
// the image types the model can accept as an image content block.
func isImageExt(ext string) bool {
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	}
	return false
}

// pluginsVersion returns the version string.
func pluginsVersion() string {
	return tauVersion()
}
