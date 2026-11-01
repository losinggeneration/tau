// Tau is the bare executable entry point for the tau coding agent.
// It supports subcommands for auth management and model resolution,
// and defaults to running a single-shot conversation with the faux provider
// when given positional arguments.
package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/losinggeneration/tau/agent/artifacts"
	"github.com/losinggeneration/tau/agent/packages"
	"github.com/losinggeneration/tau/agent/plugins"
	"github.com/losinggeneration/tau/agent/plugins/state"
	"github.com/losinggeneration/tau/agent/session"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/ai/oauth"
	"github.com/losinggeneration/tau/ai/store"
	"github.com/losinggeneration/tau/commands"
	"github.com/losinggeneration/tau/internal/app/tui"
	"github.com/losinggeneration/tau/logging"
	"github.com/losinggeneration/tau/paths"
	"github.com/losinggeneration/tau/rpc/server"
	"github.com/losinggeneration/tau/sdk"
	"github.com/losinggeneration/tau/ui"
	"github.com/urfave/cli/v3"
)

type (
	pluginCLIFlagsKey     struct{}
	startupDiagnosticsKey struct{}
)

type startupDiagnostics struct {
	mu    sync.Mutex
	lines []string
}

func withStartupDiagnostics(ctx context.Context) (context.Context, *startupDiagnostics) {
	d := &startupDiagnostics{}
	return context.WithValue(ctx, startupDiagnosticsKey{}, d), d
}

func recordStartupDiagnostic(ctx context.Context, format string, args ...any) {
	d, _ := ctx.Value(startupDiagnosticsKey{}).(*startupDiagnostics)
	if d == nil {
		return
	}
	d.mu.Lock()
	d.lines = append(d.lines, fmt.Sprintf(format, args...))
	d.mu.Unlock()
}

func (d *startupDiagnostics) Lines() []string {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.lines...)
}

func diagnosticsFromContext(ctx context.Context) []string {
	d, _ := ctx.Value(startupDiagnosticsKey{}).(*startupDiagnostics)
	return d.Lines()
}

func CLI() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Promote --config-dir to TAU_CONFIG_DIR before any flag parsing or
	// plugin discovery so every subsystem resolves paths consistently.
	promoteConfigDirFlag(os.Args)

	root := newRootCommand()

	cleanArgs, pluginCLIFlags, err := preprocessPluginCLIArgs(ctx, os.Args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(ExitUserError)
	}
	cleanArgs = normalizeOptionalResumeArgs(cleanArgs)

	// Store the root command in context for conversation mode flag access.
	cmdCtx, diagnostics := withStartupDiagnostics(ctx)
	cmdCtx = context.WithValue(cmdCtx, rootCmdKey{}, root)
	cmdCtx = context.WithValue(cmdCtx, rootPromptDelimiterKey{}, hasExplicitPromptDelimiter(cleanArgs))
	cmdCtx = context.WithValue(cmdCtx, pluginCLIFlagsKey{}, pluginCLIFlags)

	if err := root.Run(cmdCtx, cleanArgs); err != nil {
		fmt.Fprintln(os.Stderr, err)
		// Extract exit code from error if it's a cli.ExitCoder
		if exitErr, ok := err.(cli.ExitCoder); ok {
			os.Exit(exitErr.ExitCode())
		}
		os.Exit(1)
	}
	for _, line := range diagnostics.Lines() {
		fmt.Fprintln(os.Stderr, line)
	}
}

// registryOrEmpty returns the loaded models.json registry, or an empty
// in-memory one if loading failed. The empty registry yields empty lookups,
// which the callers fall back to built-in defaults.
func registryOrEmpty() *ai.ModelRegistry {
	if reg, err := ai.LoadModelRegistry(filepath.Join(configDir(), "models.json")); err == nil {
		return reg
	}
	return ai.NewModelRegistry()
}

// providerBaseURL looks up a provider's base URL in the loaded registry.
// Returns "" when the registry has no entry; callers fall back to built-ins.
func providerBaseURL(reg *ai.ModelRegistry, provider string) string {
	if reg == nil {
		return ""
	}
	if p, ok := reg.Provider(provider); ok {
		return p.BaseURL
	}
	return ""
}

// promoteConfigDirFlag copies --config-dir from raw argv into TAU_CONFIG_DIR.
// Manual scan because consumers (plugin CLI-flag discovery, settings loads)
// run before urfave/cli parses flags.
func promoteConfigDirFlag(args []string) {
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return
		}
		var val string
		switch {
		case arg == "--config-dir":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				val = args[i+1]
			}
		case strings.HasPrefix(arg, "--config-dir="):
			val = strings.TrimPrefix(arg, "--config-dir=")
		default:
			continue
		}
		if val != "" {
			if err := os.Setenv("TAU_CONFIG_DIR", val); err != nil {
				fmt.Fprintf(os.Stderr, "set TAU_CONFIG_DIR: %v\n", err)
			}
		}
		return
	}
}

// configDir returns the tau config directory, defaulting to ~/.tau/agent.
func configDir() string {
	return paths.ConfigDir()
}

// sessionDir returns the sessions directory. Precedence: TAU_SESSION_DIR,
// then sessionDir in settings.json, then <configDir>/sessions. The
// --session-dir flag is applied by sessionDirForCmd on top of this.
func sessionDir() (string, error) {
	settings, err := loadSettings()
	if err != nil {
		return "", err
	}
	return paths.Dir("TAU_SESSION_DIR", settings.SessionDir, filepath.Join(configDir(), "sessions")), nil
}

// sessionDirForCmd applies the CLI override after validating settings.
func sessionDirForCmd(cmd *cli.Command) (string, error) {
	dir, err := sessionDir()
	if err != nil {
		return "", err
	}
	if cmd != nil && cmd.String("session-dir") != "" {
		return cmd.String("session-dir"), nil
	}
	return dir, nil
}

// loadSettings admits project settings only after resolving directory trust.
func loadSettings() (*ai.Settings, error) { return loadSettingsForTrust(context.Background(), nil) }

func loadSettingsForTrust(ctx context.Context, cmd *cli.Command) (*ai.Settings, error) {
	return loadSettingsWithUI(ctx, cmd, nil, nil)
}

// pluginTrustPass loads global and CLI plugins with the project untrusted so
// they can answer project_trust before project settings or plugins are read.
type pluginTrustPass struct {
	mgr         *plugins.Manager
	ran         bool
	discoverErr error
	// installed is set when packages were installed after the pass ran
	// discovery, so finish has to reload.
	installed bool
}

func (p *pluginTrustPass) decide(ctx context.Context, cwd string) (*bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	untrusted, err := ai.LoadTrustedSettings(ctx, configDir(), cwd, home, ai.ProjectTrustOptions{Override: new(false)})
	if err != nil {
		return nil, err
	}
	if err := untrusted.ApplyHTTPProxySettings(); err != nil {
		return nil, err
	}
	p.mgr.SetSettings(untrusted)
	p.ran = true
	p.discoverErr = p.mgr.Discover(ctx)
	decision, remember, err := p.mgr.ProjectTrustDecision(ctx, cwd)
	if err != nil || decision == nil || !remember {
		return decision, err
	}
	return decision, ai.ProjectTrustStore{Path: filepath.Join(configDir(), "trust.json")}.Set(ctx, cwd, decision)
}

// finish admits the resolved settings. A trusted project reloads the
// pre-trust plugin set so project plugins, filters and config take effect.
func (p *pluginTrustPass) finish(ctx context.Context, settings *ai.Settings) error {
	p.mgr.SetSettings(settings)
	if !p.ran {
		return p.mgr.Discover(ctx)
	}
	if !settings.ProjectResourcesTrusted() && !p.installed {
		return p.discoverErr
	}
	return errors.Join(p.discoverErr, p.mgr.Reload(ctx))
}

// installConfiguredSources installs configured npm and git sources that
// aren't installed yet, as upstream does on startup and reload.
func installConfiguredSources(ctx context.Context, settings *ai.Settings) ([]string, error) {
	userDir, projectDir := packageDirsFromSettings(settings)
	mgr := packages.NewInstallManager(userDir, projectDir)
	mgr.NPMCommand = settings.NPMCommand
	return mgr.InstallMissingSources(ctx, settings, nil)
}

// projectTrustDecisions holds each directory's resolved trust for the process,
// as upstream does, so session-only answers survive reloads and external
// trust.json edits. Headless fallthrough and unprotected directories aren't
// cached, so a later interactive prompt or new protected resources still ask.
var projectTrustDecisions sync.Map

func loadSettingsWithUI(ctx context.Context, cmd *cli.Command, u ui.UI, decide func(context.Context, string) (*bool, error)) (*ai.Settings, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolve settings directory: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	if cmd == nil {
		cmd, _ = ctx.Value(rootCmdKey{}).(*cli.Command)
	}
	var override *bool
	if cmd != nil {
		if cmd.Bool("no-approve") {
			override = new(false)
		} else if cmd.Bool("approve") {
			override = new(true)
		}
	}
	return loadTrustedSettingsCached(ctx, cwd, home, override, u, decide)
}

func loadTrustedSettingsCached(ctx context.Context, cwd, home string, override *bool, u ui.UI, decide func(context.Context, string) (*bool, error)) (*ai.Settings, error) {
	if override == nil {
		if cached, ok := projectTrustDecisions.Load(cwd); ok {
			return ai.LoadTrustedSettings(ctx, configDir(), cwd, home, ai.ProjectTrustOptions{Override: new(cached.(bool))})
		}
	}
	var protected, undecided bool
	opts := ai.ProjectTrustOptions{
		Override: override,
		Decide: func(ctx context.Context, cwd string) (*bool, error) {
			protected = true
			if decide == nil {
				return nil, nil
			}
			return decide(ctx, cwd)
		},
		Prompt: func(ctx context.Context, cwd string) (*bool, error) {
			if u == nil {
				undecided = true
				return nil, nil
			}
			return ai.PromptProjectTrust(ctx, ai.ProjectTrustStore{Path: filepath.Join(configDir(), "trust.json")}, cwd, u.Select)
		},
	}
	settings, err := ai.LoadTrustedSettings(ctx, configDir(), cwd, home, opts)
	if err == nil && protected && !undecided {
		projectTrustDecisions.Store(cwd, settings.ProjectResourcesTrusted())
	}
	return settings, err
}

// errProjectUntrusted refuses project-scope writes in an untrusted directory.
var errProjectUntrusted = cli.Exit("Project is not trusted. Use --approve to modify project config.", ExitUserError)

func projectTrusted(ctx context.Context, cmd *cli.Command) (bool, error) {
	settings, err := loadSettingsForTrust(ctx, cmd)
	if err != nil {
		return false, cli.Exit(err.Error(), ExitUserError)
	}
	return settings.ProjectResourcesTrusted(), nil
}

func requireTrustedProject(ctx context.Context, cmd *cli.Command) error {
	trusted, err := projectTrusted(ctx, cmd)
	if err != nil || trusted {
		return err
	}
	return errProjectUntrusted
}

// openStore creates a FileBackend credential store at the standard path.
func openStore() (*store.FileBackend, error) {
	cfgDir := configDir()
	authPath := filepath.Join(cfgDir, "auth.json")
	fb := store.NewFileBackend(authPath, store.Options{RefreshFuncs: store.OAuthProviderRefreshFuncs()})
	if err := fb.EnsureDir(); err != nil {
		return nil, fmt.Errorf("ensure auth dir: %w", err)
	}
	return fb, nil
}

// reloadSession reloads settings, artifacts, plugins, tools, and the system
// prompt for /reload.
// Settings files, artifact/plugin discovery, and session settings are validated
// before anything is replaced, so those failures leave the previous state.
// SDK sessions stage plugin startups and persist the prompt before admission.
func reloadSession(ctx context.Context, plugin string, sess commands.CommandSession) (commands.Result, error) {
	if err := context.Cause(ctx); err != nil {
		return commands.Result{}, err
	}
	settings, err := loadSettingsForTrust(ctx, nil)
	if err != nil {
		return commands.Result{}, fmt.Errorf("reload settings: %w", err)
	}
	if _, err := installConfiguredSources(ctx, settings); err != nil {
		return commands.Result{}, fmt.Errorf("reload package sources: %w", err)
	}
	var prepared *artifacts.PreparedReload
	if loader, ok := sdk.GlobalArtifactLoader.(*artifacts.Loader); ok {
		userDir, projectDir := packageDirsFromSettings(settings)
		resolved, resolveErr := packages.ResolvePackageSources(settings, userDir, projectDir)
		if resolveErr != nil {
			return commands.Result{}, fmt.Errorf("reload package sources: %w", resolveErr)
		}
		prepared, err = loader.Prepare(artifacts.ReloadOptions{
			PackagePaths:   []string{projectDir, userDir},
			Packages:       resolved,
			SettingsScopes: getSettingsArtifactScopes(settings),
			ProjectTrusted: new(settings.ProjectResourcesTrusted()),
			UserDirs:       artifactUserDirs(settings),
		})
		if err != nil {
			return commands.Result{}, fmt.Errorf("reload artifacts: %w", err)
		}
	} else if sdk.GlobalArtifactLoader != nil {
		return commands.Result{}, errors.New("artifact loader does not support prepared reload")
	}
	var preparedPlugins *plugins.PreparedReload
	if GlobalPluginManager != nil {
		preparedPlugins, err = GlobalPluginManager.PrepareReload(ctx, settings)
		if err != nil {
			return commands.Result{}, fmt.Errorf("reload plugins: %w", err)
		}
	}

	if err := context.Cause(ctx); err != nil {
		return commands.Result{}, err
	}
	reloader, ok := sess.(sdk.ResourceReloader)
	if !ok {
		return commands.Result{}, errors.New("session does not support coordinated resource reload")
	}
	if err := reloader.ReloadResources(ctx, settings, prepared, preparedPlugins, plugin); err != nil {
		return commands.Result{}, fmt.Errorf("reload resources: %w", err)
	}

	output := "reloaded settings, artifacts, and plugins"
	if plugin != "" {
		output = fmt.Sprintf("reloaded settings, artifacts, and plugin %q", plugin)
	}
	if prepared != nil {
		for _, line := range prepared.ThemeDiagnostics() {
			output += "\nwarning: invalid theme " + line
		}
	}
	return commands.Result{Output: output}, nil
}

// registerBuiltInCommands registers the built-in tau commands into the registry.
func registerBuiltInCommands(reg *commands.InMemoryRegistry) {
	mustRegisterBuiltin(reg, commands.Info{
		Name:         "login",
		Description:  "Log in to an AI provider",
		ArgumentHint: "<provider>",
		Source:       commands.Builtin,
	}, func(ctx context.Context, args string, ui commands.CommandUI, sess commands.CommandSession) (commands.Result, error) {
		return commands.Result{Output: "use `tau login` from the shell instead"}, nil
	})

	mustRegisterBuiltin(reg, commands.Info{
		Name:        "help",
		Description: "List available slash commands",
		Source:      commands.Builtin,
	}, func(ctx context.Context, args string, ui commands.CommandUI, sess commands.CommandSession) (commands.Result, error) {
		var b strings.Builder
		b.WriteString("Available slash commands:\n")
		for _, info := range reg.List() {
			fmt.Fprintf(&b, "  /%s", info.Name)
			if info.ArgumentHint != "" {
				fmt.Fprintf(&b, " %s", info.ArgumentHint)
			}
			if info.Description != "" {
				fmt.Fprintf(&b, " — %s", info.Description)
			}
			b.WriteString("\n")
		}
		return commands.Result{Output: b.String()}, nil
	})

	mustRegisterBuiltin(reg, commands.Info{
		Name:         "reload",
		Description:  "Re-discover artifacts (skills, prompts, themes) and plugins",
		ArgumentHint: "<plugin>",
		Source:       commands.Builtin,
	}, func(ctx context.Context, args string, ui commands.CommandUI, sess commands.CommandSession) (commands.Result, error) {
		return reloadSession(ctx, strings.TrimSpace(args), sess)
	})

	mustRegisterBuiltin(reg, commands.Info{
		Name:        "sessions",
		Description: "List, fork and export sessions (run `tau sessions --help` for subcommands)",
		Source:      commands.Builtin,
	}, func(ctx context.Context, args string, ui commands.CommandUI, sess commands.CommandSession) (commands.Result, error) {
		return commands.Result{Output: "use `tau sessions <subcommand>` from the shell"}, nil
	})

	mustRegisterBuiltin(reg, commands.Info{
		Name:         "set-model",
		Description:  "Set the default model for new sessions",
		ArgumentHint: "<provider>/<model>",
		Source:       commands.Builtin,
	}, func(ctx context.Context, args string, ui commands.CommandUI, sess commands.CommandSession) (commands.Result, error) {
		return commands.Result{Output: "use `tau set-model` from the shell"}, nil
	})

	mustRegisterBuiltin(reg, commands.Info{
		Name:        "show-model",
		Description: "Show the currently resolved model and capabilities",
		Source:      commands.Builtin,
	}, func(ctx context.Context, args string, ui commands.CommandUI, sess commands.CommandSession) (commands.Result, error) {
		return commands.Result{Output: "use `tau show-model` from the shell"}, nil
	})

	mustRegisterBuiltin(reg, commands.Info{
		Name:         "compact",
		Description:  "Manually compact the session context",
		ArgumentHint: "[instructions]",
		Source:       commands.Builtin,
	}, func(ctx context.Context, args string, ui commands.CommandUI, sess commands.CommandSession) (commands.Result, error) {
		if sess == nil {
			return commands.Result{Output: "No active session to compact.", IsError: true}, nil
		}
		if err := sess.Compact(ctx, strings.TrimSpace(args)); err != nil {
			return commands.Result{}, fmt.Errorf("compact: %w", err)
		}
		return commands.Result{Output: "Compaction requested"}, nil
	})

	mustRegisterBuiltin(reg, commands.Info{
		Name:        "copy",
		Description: "Copy last agent message to clipboard",
		Source:      commands.Builtin,
	}, func(ctx context.Context, args string, ui commands.CommandUI, sess commands.CommandSession) (commands.Result, error) {
		text := sess.GetLastAssistantText()
		if text == "" {
			return commands.Result{Output: "No agent messages to copy yet.", IsError: true}, nil
		}
		if !copyToClipboard(text) {
			return commands.Result{Output: "Failed to copy to clipboard (xclip/xsel not available)", IsError: true}, nil
		}
		return commands.Result{Output: "Copied last agent message to clipboard"}, nil
	})

	mustRegisterBuiltin(reg, commands.Info{
		Name:         "name",
		Description:  "Set session display name",
		ArgumentHint: "<name>",
		Source:       commands.Builtin,
	}, func(ctx context.Context, args string, ui commands.CommandUI, sess commands.CommandSession) (commands.Result, error) {
		name := strings.TrimSpace(args)
		if name == "" {
			currentName := sess.GetSessionName()
			if currentName != "" {
				return commands.Result{Output: fmt.Sprintf("Session name: %s", currentName)}, nil
			}
			return commands.Result{Output: "Usage: /name <name>", IsError: true}, nil
		}
		// Note: The actual session naming is handled by the session host.
		// This command is primarily for UI feedback purposes.
		return commands.Result{Output: fmt.Sprintf("Session name set to: %s", name)}, nil
	})
}

func mustRegisterBuiltin(reg *commands.InMemoryRegistry, info commands.Info, handler commands.Handler) {
	if err := reg.Register(info, handler); err != nil {
		panic(fmt.Sprintf("register built-in command %q: %v", info.Name, err))
	}
}

// GlobalCommandRegistry exposes the command registry for slash-command handling.
var GlobalCommandRegistry commands.Registry

// GlobalPluginManager exposes the plugin manager for named reload dispatch.
// It is set during runtime initialization.
var GlobalPluginManager *plugins.Manager

// runtimeBuildConfig carries optional buildRuntimeOptions behavior.
type runtimeBuildConfig struct {
	trustPrompt ui.UI
	skipPlugins bool
}

// runtimeBuildOption configures buildRuntimeOptions.
type runtimeBuildOption func(*runtimeBuildConfig)

// withoutPlugins skips plugin manager creation and discovery entirely. For
// CLI commands that never dispatch to plugins (artifact listings,
// show-model): no subprocesses, no discovery cost.
func withoutPlugins() runtimeBuildOption {
	return func(c *runtimeBuildConfig) { c.skipPlugins = true }
}

// withTrustPrompt asks about project trust with u before bootstrap. Like
// upstream, only interactive startup prompts; other modes stay headless.
func withTrustPrompt(u ui.UI) runtimeBuildOption {
	return func(c *runtimeBuildConfig) { c.trustPrompt = u }
}

// buildRuntimeOptions creates RuntimeOptions with all shared state initialized.
// cmd may be nil for commands that don't have conversation flags.
func buildRuntimeOptions(ctx context.Context, uiImpl ui.UI, cmd *cli.Command, buildOpts ...runtimeBuildOption) (_ sdk.RuntimeOptions, _ *plugins.Manager, _ *artifacts.Loader, _ *commands.InMemoryRegistry, err error) {
	var started *plugins.Manager
	defer func() {
		if err == nil || started == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), plugins.DefaultDrainTimeout)
		defer cancel()
		err = errors.Join(err, started.Shutdown(cleanupCtx))
	}()
	var cfg runtimeBuildConfig
	for _, opt := range buildOpts {
		opt(&cfg)
	}

	reg := commands.NewInMemoryRegistry()
	registerBuiltInCommands(reg)

	cfgDir := configDir()
	var (
		pluginMgr *plugins.Manager
		trustPass *pluginTrustPass
		decide    func(context.Context, string) (*bool, error)
	)
	if !cfg.skipPlugins {
		pluginMgr = plugins.NewManager(cfgDir, uiImpl, reg)
		started = pluginMgr
		if cmd != nil {
			for _, extPath := range cmd.StringSlice("extension") {
				if extPath != "" {
					pluginMgr.AddExtensionPath(extPath)
				}
			}
		}
		if cmd == nil || !cmd.Bool("no-extensions") {
			trustPass = &pluginTrustPass{mgr: pluginMgr}
			decide = trustPass.decide
		}
	}

	settings, err := loadSettingsWithUI(ctx, cmd, cfg.trustPrompt, decide)
	if err != nil {
		return sdk.RuntimeOptions{}, nil, nil, nil, err
	}

	if err := settings.ApplyHTTPProxySettings(); err != nil {
		return sdk.RuntimeOptions{}, nil, nil, nil, err
	}
	installed, err := installConfiguredSources(ctx, settings)
	if err != nil {
		recordStartupDiagnostic(ctx, "package install error: %v", err)
	}
	if trustPass != nil {
		trustPass.installed = len(installed) > 0
	}

	opts := sdk.RuntimeOptions{UI: uiImpl, CommandRegistry: reg}
	fb, err := openStore()
	if err != nil {
		return sdk.RuntimeOptions{}, nil, nil, nil, err
	}
	opts.CredentialStore = fb

	opts.Settings = settings

	sessDir := paths.Dir("TAU_SESSION_DIR", settings.SessionDir, filepath.Join(cfgDir, "sessions"))
	if cmd != nil && cmd.String("session-dir") != "" {
		sessDir = cmd.String("session-dir")
	}
	opts.SessionsDir = sessDir
	sessionStorage, err := session.NewStorageAt(sessDir)
	if err != nil {
		return sdk.RuntimeOptions{}, nil, nil, nil, err
	}
	if err := sessionStorage.CleanupStaleOwners(); err != nil {
		return sdk.RuntimeOptions{}, nil, nil, nil, fmt.Errorf("clean stale session owners: %w", err)
	}

	if pluginMgr != nil {
		pluginMgr.SetSessionAccessor(session.NewSessionStoreAccessor(sessionStorage))
		pluginMgr.SetSettings(settings)
		if trustPass != nil {
			if err := trustPass.finish(ctx, settings); err != nil {
				recordStartupDiagnostic(ctx, "plugin discovery error: %v", err)
			}
		}
		applyPluginCLIFlags(ctx, pluginMgr)
	}

	cwd, _ := os.Getwd()
	userDir, projectDir := packageDirsFromSettings(settings)
	var cliPaths map[artifacts.ArtifactKind][]string
	if cmd != nil {
		cliPaths = map[artifacts.ArtifactKind][]string{
			artifacts.KindSkill:  cmd.StringSlice("skill"),
			artifacts.KindPrompt: cmd.StringSlice("prompt-template"),
			artifacts.KindTheme:  cmd.StringSlice("theme"),
		}
	}
	resolved, resolveErr := packages.ResolvePackageSources(settings, userDir, projectDir)
	if resolveErr != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to resolve package sources: %v\n", resolveErr)
		resolved = []packages.ResolvedPackage{}
	}
	loaderOpts := artifacts.LoaderOptions{
		ThemeValidator:   func(raw []byte) error { _, err := tui.DecodeThemeColors(raw); return err },
		Log:              nil,
		CWD:              cwd,
		SettingsScopes:   getSettingsArtifactScopes(settings),
		ProjectTrusted:   new(settings.ProjectResourcesTrusted()),
		CLIPaths:         cliPaths,
		PackagePaths:     []string{projectDir, userDir},
		Packages:         resolved,
		UserDirs:         artifactUserDirs(settings),
		CommandsRegistry: reg,
		// Honor artifact discovery control flags.
		DisableSkills:  cmd != nil && cmd.Bool("no-skills"),
		DisablePrompts: cmd != nil && cmd.Bool("no-prompt-templates"),
		DisableThemes:  cmd != nil && cmd.Bool("no-themes"),
	}
	if pluginMgr != nil {
		loaderOpts.PluginReloader = func(ctx context.Context) error {
			return pluginMgr.Reload(ctx)
		}
	}
	loader, err := artifacts.NewLoader(loaderOpts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to initialize artifacts: %v\n", err)
		loader = nil
	} else {
		for _, line := range loader.ThemeRegistry().Diagnostics() {
			recordStartupDiagnostic(ctx, "warning: invalid theme %s", line)
		}
	}

	// Pass --skill flags as ID-based preloaded skills to the runtime.
	// These are resolved from the skill registry after discovery is complete.
	if cmd != nil && len(cmd.StringSlice("skill")) > 0 {
		opts.PreloadSkills = cmd.StringSlice("skill")
	}

	return opts, pluginMgr, loader, reg, nil
}

func buildConversationRuntimeOptions(ctx context.Context, uiImpl ui.UI, buildOpts ...runtimeBuildOption) (sdk.RuntimeOptions, *plugins.Manager, *artifacts.Loader, *commands.InMemoryRegistry, SessionFlags, error) {
	cmd := rootCmdFromContext(ctx)
	rtOpts, pluginMgr, loader, reg, err := buildRuntimeOptions(ctx, uiImpl, cmd, buildOpts...)
	if err != nil {
		return sdk.RuntimeOptions{}, nil, nil, nil, SessionFlags{}, err
	}
	if loader != nil {
		sdk.GlobalArtifactLoader = loader
		rtOpts.ArtifactLoader = loader
	}
	rtOpts.PluginManager = pluginMgr

	var sessionFlags SessionFlags
	if cmd != nil {
		sessionFlags = applyConversationFlags(&rtOpts, cmd)
	}

	return rtOpts, pluginMgr, loader, reg, sessionFlags, nil
}

// writeLine writes a formatted line to w, ignoring write errors.
func writeLine(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func stdoutLine(format string, args ...any) error {
	if _, err := fmt.Fprintf(os.Stdout, format, args...); err != nil {
		return fmt.Errorf("write stdout: %w", err)
	}
	return nil
}

func stdoutBytes(data []byte) error {
	if _, err := os.Stdout.Write(data); err != nil {
		return fmt.Errorf("write stdout: %w", err)
	}
	return nil
}

// artifactUserDirs resolves the user-global artifact directories per kind.
// Precedence: TAU_SKILLS_DIR/TAU_PROMPTS_DIR/TAU_THEMES_DIR, then the
// matching settings.json field, then <configDir>/<kind>.
func artifactUserDirs(settings *ai.Settings) map[artifacts.ArtifactKind]string {
	var skillsSetting, promptsSetting, themesSetting string
	if settings != nil {
		skillsSetting = settings.SkillDir
		promptsSetting = settings.PromptDir
		themesSetting = settings.ThemeDir
	}
	cfgDir := configDir()
	return map[artifacts.ArtifactKind]string{
		artifacts.KindSkill:  paths.Dir("TAU_SKILLS_DIR", skillsSetting, filepath.Join(cfgDir, "skills")),
		artifacts.KindPrompt: paths.Dir("TAU_PROMPTS_DIR", promptsSetting, filepath.Join(cfgDir, "prompts")),
		artifacts.KindTheme:  paths.Dir("TAU_THEMES_DIR", themesSetting, filepath.Join(cfgDir, "themes")),
	}
}

func getSettingsArtifactScopes(settings *ai.Settings) map[artifacts.SourceTier]map[artifacts.ArtifactKind][]string {
	result := map[artifacts.SourceTier]map[artifacts.ArtifactKind][]string{
		artifacts.TierUser: {}, artifacts.TierProject: {},
	}
	for kind, name := range map[artifacts.ArtifactKind]string{
		artifacts.KindSkill: "skills", artifacts.KindPrompt: "prompts", artifacts.KindTheme: "themes",
	} {
		user, project := settings.ResourcePathsByScope(name)
		result[artifacts.TierUser][kind] = user
		result[artifacts.TierProject][kind] = project
	}
	return result
}

// packagesDirs returns the user and project package directories. User dir
// precedence: TAU_PACKAGE_DIR, then packageDir in settings.json, then
// <configDir>/packages.
func packagesDirs() (userDir, projectDir string, err error) {
	settings, err := loadSettings()
	if err != nil {
		return "", "", err
	}
	userDir, projectDir = packageDirsFromSettings(settings)
	return userDir, projectDir, nil
}

func packageDirsFromSettings(settings *ai.Settings) (userDir, projectDir string) {
	var configured string
	if settings != nil {
		configured = settings.PackageDir
	}
	return paths.Dir("TAU_PACKAGE_DIR", configured, filepath.Join(configDir(), "packages")), ".tau/packages"
}

// isTTY returns true if stdin is a terminal.
func isTTY() bool {
	info, _ := os.Stdin.Stat()
	return (info.Mode() & os.ModeCharDevice) != 0
}

// normalizeToolList accepts either repeated single-name flags or a single
// comma-separated value, and returns a flat trimmed, deduplicated name list.
// The pi CLI surface uses comma-separated values (--tools a,b,c), while the
// urfave/cli/v3 StringSliceFlag convention is repeated flags (--tools a --tools b).
// Supporting both keeps the pi-style invocation ergonomic.
func normalizeToolList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		for _, part := range strings.Split(raw, ",") {
			name := strings.TrimSpace(part)
			if name == "" {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	return out
}

func providerAllowsNoCredential(provider string) bool {
	return ai.ProviderAllowsNoCredential(provider)
}

func missingCredentialLoginError(provider string, oauthSupported bool) error {
	if envVar := ai.EnvVarForProvider(provider); envVar != "" {
		return cli.Exit(
			fmt.Sprintf("missing API key for %s: pass --api-key, use --api-key-stdin, or set %s", provider, envVar),
			ExitUserError,
		)
	}
	if oauthSupported {
		return cli.Exit(fmt.Sprintf("missing credential for %s: pass --api-key or use --oauth", provider), ExitUserError)
	}
	return cli.Exit(fmt.Sprintf("missing credential for %s: pass --api-key or use --api-key-stdin", provider), ExitUserError)
}

// runLogin handles the `tau login` command.
func runLogin(ctx context.Context, cmd *cli.Command) error {
	if !cmd.Args().Present() {
		return cli.Exit("usage: tau login <provider>", ExitUserError)
	}

	provider := cmd.Args().First()

	apiKey := cmd.String("api-key")
	apiKeyStdin := cmd.Bool("api-key-stdin")
	useOAuth := cmd.Bool("oauth")
	baseURL := cmd.String("base-url")
	defaultModel := cmd.String("default-model")

	interactive := isTTY()

	if apiKeyStdin && apiKey == "" {
		fmt.Fprint(os.Stderr, "Enter API key: ")
		scanner := bufio.NewScanner(os.Stdin)
		if scanner.Scan() {
			apiKey = scanner.Text()
		}
		if err := scanner.Err(); err != nil {
			return cli.Exit(fmt.Sprintf("read stdin: %v", err), ExitUserError)
		}
	}

	oauthProvider, isOAuth := oauth.GetProvider(oauth.ProviderID(provider))
	if useOAuth && !isOAuth {
		return cli.Exit(fmt.Sprintf("OAuth login is not supported for %s", provider), ExitUserError)
	}

	var cred ai.Credential
	usedOAuth := false
	if providerAllowsNoCredential(provider) && apiKey == "" && !useOAuth {
		cred = ai.NewNoneCredential()
	} else if isOAuth && (apiKey == "" || useOAuth) {
		if oauthProvider.ID() == oauth.ProviderOpenAICodex {
			oauthProvider = oauth.NewOpenAICodexProvider("tau", oauth.WithOpenAIDeviceID(func(ctx context.Context) (string, error) {
				return ai.GetOrCreateDeviceID(ctx, filepath.Join(configDir(), "settings.json"))
			}))
		}
		globalSettings, err := ai.LoadSettings(filepath.Join(configDir(), "settings.json"))
		if err != nil {
			return cli.Exit(fmt.Sprintf("load authentication settings: %v", err), ExitSubsystemError)
		}
		if err := globalSettings.ApplyHTTPProxySettings(); err != nil {
			return cli.Exit(err.Error(), ExitSubsystemError)
		}
		callbacks := &loginCallbacks{interactive: interactive}
		cred, err = loginOAuthCredential(ctx, oauthProvider, callbacks)
		if err != nil {
			return cli.Exit(fmt.Sprintf("OAuth login failed: %v", err), ExitAuthError)
		}
		usedOAuth = true
	} else if apiKey != "" {
		cred = ai.NewAPIKeyCredential(apiKey)
	} else if providerAllowsNoCredential(provider) {
		cred = ai.NewNoneCredential()
	} else {
		return missingCredentialLoginError(provider, isOAuth)
	}

	fb, err := openStore()
	if err != nil {
		return cli.Exit(err.Error(), ExitSubsystemError)
	}

	if err := fb.Set(ctx, provider, cred); err != nil {
		return cli.Exit(fmt.Sprintf("store credential: %v", err), ExitSubsystemError)
	}

	cfgDir := configDir()
	settingsPath := filepath.Join(cfgDir, "settings.json")
	// Provider endpoints belong in models.json; login only updates defaults.
	if baseURL != "" || defaultModel != "" {
		fmt.Fprintf(os.Stderr, "warning: --base-url and --default-model are deprecated; configure provider endpoints via models.json\n")
	}
	if defaultModel != "" && provider != "" {
		if err := ai.UpdateSettings(ctx, settingsPath, map[string]any{"defaultProvider": provider, "defaultModel": defaultModel}); err != nil {
			return cli.Exit(fmt.Sprintf("save settings: %v", err), ExitSubsystemError)
		}
	}

	if usedOAuth {
		fmt.Fprintf(os.Stderr, "Logged in to %s via OAuth\n", provider)
	} else if apiKey != "" {
		fmt.Fprintf(os.Stderr, "Logged in to %s with API key\n", provider)
	} else {
		fmt.Fprintf(os.Stderr, "Configured %s (no credential required)\n", provider)
	}
	return nil
}

func loginOAuthCredential(ctx context.Context, provider oauth.Provider, callbacks oauth.LoginCallbacks) (ai.Credential, error) {
	if err := ctx.Err(); err != nil {
		return ai.Credential{}, fmt.Errorf("authenticate: %w", err)
	}
	creds, err := provider.Login(ctx, callbacks)
	if err != nil {
		return ai.Credential{}, fmt.Errorf("authenticate: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ai.Credential{}, fmt.Errorf("authenticate: %w", err)
	}
	if creds == nil || creds.Access == "" {
		return ai.Credential{}, errors.New("provider returned no access token")
	}
	return store.CredentialFromOAuth(creds), nil
}

// loginCallbacks implements oauth.LoginCallbacks for the CLI.
type loginCallbacks struct {
	interactive bool
}

func (c *loginCallbacks) OnAuth(url string, instructions string) {
	fmt.Fprintf(os.Stderr, "\n%s\n\n%s\n", url, instructions)
}

func (c *loginCallbacks) OnPrompt(ctx context.Context, message string, placeholder string) (string, error) {
	if !c.interactive {
		return "", fmt.Errorf("non-interactive mode: %w", oauth.ErrPromptUnavailable)
	}
	fmt.Fprint(os.Stderr, message)
	return oauth.ReadStdinLine(ctx)
}

func (c *loginCallbacks) OnProgress(message string) {
	fmt.Fprintln(os.Stderr, message)
}

func (c *loginCallbacks) OnManualCodeInput(ctx context.Context) (string, error) {
	if !c.interactive {
		return "", fmt.Errorf("non-interactive mode: %w", oauth.ErrPromptUnavailable)
	}
	fmt.Fprint(os.Stderr, "Enter authorization code or redirect URL: ")
	return oauth.ReadStdinLine(ctx)
}

// runLogout handles the `tau logout` command.
func runLogout(ctx context.Context, cmd *cli.Command) error {
	if !cmd.Args().Present() {
		return cli.Exit("usage: tau logout <provider>", ExitUserError)
	}

	provider := cmd.Args().First()

	fb, err := openStore()
	if err != nil {
		return cli.Exit(err.Error(), ExitSubsystemError)
	}

	if err := fb.Delete(ctx, provider); err != nil {
		return cli.Exit(fmt.Sprintf("delete credential: %v", err), ExitSubsystemError)
	}

	fmt.Fprintf(os.Stderr, "Logged out of %s\n", provider)
	return nil
}

// runProviders handles the `tau providers` command.
func runProviders(ctx context.Context, cmd *cli.Command) error {
	check := cmd.Bool("check")

	fb, err := openStore()
	if err != nil {
		return cli.Exit(err.Error(), ExitSubsystemError)
	}

	refs, err := fb.List(ctx)
	if err != nil {
		return cli.Exit(fmt.Sprintf("list credentials: %v", err), ExitSubsystemError)
	}

	refs = mergeProviderRefsWithEnv(refs)
	if len(refs) == 0 {
		fmt.Fprintln(os.Stderr, "No providers configured.")
		return nil
	}

	if check {
		return runProvidersCheck(ctx, cmd, fb, refs)
	}

	for _, ref := range refs {
		cred, err := providerCredential(ctx, fb, ref)
		if err != nil {
			writeLine(os.Stdout, "%s  [error: %v]\n", ref.Provider, err)
			continue
		}

		status := string(cred.Type)
		if cred.NeedsReauth {
			status += " (needs re-auth)"
		}
		writeLine(os.Stdout, "%s  [%s]\n", ref.Provider, status)
	}

	return nil
}

// mergeProviderRefsWithEnv adds environment-backed providers that are not
// already represented by stored credentials.
func mergeProviderRefsWithEnv(refs []ai.CredentialRef) []ai.CredentialRef {
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		seen[ref.Provider] = struct{}{}
	}

	merged := append([]ai.CredentialRef(nil), refs...)
	for _, provider := range ai.ListAvailableProviders() {
		if !ai.HasEnvCredentials(provider) {
			continue
		}
		if _, ok := seen[provider]; ok {
			continue
		}
		seen[provider] = struct{}{}
		merged = append(merged, ai.CredentialRef{
			Provider: provider,
			Type:     ai.CredentialTypeAPIKey,
		})
	}
	return merged
}

// providerCredential resolves the credential for a provider, trying the
// store first and falling back to environment variables.
func providerCredential(ctx context.Context, fb *store.FileBackend, ref ai.CredentialRef) (ai.Credential, error) {
	cred, err := fb.Get(ctx, ref.Provider)
	if err == nil {
		return cred, nil
	}
	if !errors.Is(err, ai.ErrCredentialNotFound) && !errors.Is(err, os.ErrNotExist) {
		return cred, err
	}
	// Header-owned authentication is resolved by the provider at request time.
	if len(ai.GetEnvHeaders(ref.Provider)) != 0 {
		return ai.NewAPIKeyCredential(""), nil
	}
	if apiKey := ai.GetEnvAPIKey(ref.Provider); apiKey != "" {
		return ai.NewAPIKeyCredential(apiKey), nil
	}
	return cred, err
}

func runProvidersCheck(ctx context.Context, cmd *cli.Command, fb *store.FileBackend, refs []ai.CredentialRef) error {
	settings, _ := loadSettings()

	for _, ref := range refs {
		cred, err := providerCredential(ctx, fb, ref)
		if err != nil {
			writeLine(os.Stdout, "%s  ERROR: %v\n", ref.Provider, err)
			continue
		}

		var detail string

		resolver := ai.NewModelResolver(settings)
		_ = resolver // provider check uses credential + base URL only

		// Use the default model if this provider matches, otherwise we
		// cannot validate without a configured default for the provider.
		defaultSpec := resolver.ResolveDefaultModel()
		var modelID, baseURL string
		if defaultSpec.Provider == ref.Provider {
			modelID = defaultSpec.Model
		}
		if modelID == "" {
			writeLine(os.Stdout, "%s  no model configured  (%s)\n", ref.Provider, detail)
			continue
		}

		model := &ai.Model{
			ID:       modelID,
			Provider: ref.Provider,
			BaseURL:  baseURL,
		}

		apiType := ai.APIForProvider(ref.Provider)
		model.API = apiType

		var status string
		valid, err := checkProvider(ctx, model, cred)
		if err != nil {
			status = "error"
			detail = err.Error()
		} else if valid {
			status = "ok"
		} else {
			status = "invalid"
			detail = "authentication failed"
		}

		if detail != "" {
			writeLine(os.Stdout, "%s  %s  (%s)\n", ref.Provider, status, detail)
		} else {
			writeLine(os.Stdout, "%s  %s\n", ref.Provider, status)
		}
	}

	return nil
}

// checkProvider makes a lightweight API call to validate a provider credential.
func checkProvider(ctx context.Context, model *ai.Model, cred ai.Credential) (bool, error) {
	var apiKey string

	switch cred.Type {
	case ai.CredentialTypeNone:
		if providerAllowsNoCredential(model.Provider) {
			return true, nil
		}
		return false, fmt.Errorf("provider %s does not support no-credential login", model.Provider)
	default:
		var err error
		apiKey, err = store.APIKeyFromCredential(ctx, model.Provider, cred)
		if err != nil {
			return false, err
		}
	}

	client, err := ai.GetClient(model)
	if err != nil {
		if !strings.HasPrefix(model.Provider, "openai-compat") &&
			!strings.HasPrefix(model.Provider, "anthropic-compat") {
			return false, fmt.Errorf("no provider for %s", model.Provider)
		}
		model.API = ai.APIOpenAICompatible
		client, err = ai.GetClient(model)
		if err != nil {
			return false, fmt.Errorf("no compatible provider: %w", err)
		}
	}

	checkPrompt := &ai.Context{
		Messages: []ai.Message{
			{Role: "user", Content: "hi"},
		},
	}

	opts := []ai.Option{ai.WithAPIKey(apiKey)}

	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, err = client.Complete(checkCtx, model, checkPrompt, opts...)
	if err != nil {
		if strings.Contains(err.Error(), "401") ||
			strings.Contains(err.Error(), "unauthorized") ||
			strings.Contains(err.Error(), "invalid") ||
			strings.Contains(err.Error(), "authentication") {
			return false, nil
		}
		return false, fmt.Errorf("request failed: %w", err)
	}

	return true, nil
}

// runModelsList handles the `tau models list` subcommand.
func runModelsList(ctx context.Context, cmd *cli.Command) error {
	if cmd.Bool("all") {
		if code := runListModels(ctx, ""); code != ExitSuccess {
			return cli.Exit("", code)
		}
		return nil
	}

	providerFilter := cmd.String("provider")

	fb, err := openStore()
	if err != nil {
		return cli.Exit(fmt.Sprintf("open store: %v", err), ExitSubsystemError)
	}

	for _, model := range ai.AuthenticatedModels(ctx, fb, providerFilter) {
		writeLine(os.Stdout, "%s/%s\n", model.Provider, model.ID)
	}

	return nil
}

// runModelsDiscover handles the `tau models discover` subcommand.
func runModelsDiscover(ctx context.Context, cmd *cli.Command) (err error) {
	if !cmd.Args().Present() {
		return cli.Exit("usage: tau models discover <provider>", ExitUserError)
	}

	providerKey := cmd.Args().First()
	if strings.Contains(providerKey, "/") {
		return cli.Exit("usage: tau models discover <provider>", ExitUserError)
	}

	fb, err := openStore()
	if err != nil {
		return cli.Exit(fmt.Sprintf("open store: %v", err), ExitSubsystemError)
	}

	cred, err := providerCredential(ctx, fb, ai.CredentialRef{Provider: providerKey})
	if err != nil {
		return cli.Exit(fmt.Sprintf("get credential for provider %q: %v", providerKey, err), ExitAuthError)
	}

	apiKey, err := getAPIKeyFromCredential(ctx, providerKey, cred, fb)
	if err != nil {
		return cli.Exit(fmt.Sprintf("get API key: %v", err), ExitAuthError)
	}

	// Base URL for the provider comes from models.json's providers.<id>.baseUrl
	// in the new flat schema; fall back to the built-in default.
	baseURL := providerBaseURL(registryOrEmpty(), providerKey)
	if baseURL == "" {
		baseURL = ai.DefaultBaseURLs[providerKey]
		if baseURL == "" {
			return cli.Exit(fmt.Sprintf("no base URL known for provider %q", providerKey), ExitUserError)
		}
	}

	url := strings.TrimSuffix(baseURL, "/") + "/v1/models"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return cli.Exit(fmt.Sprintf("create request: %v", err), ExitSubsystemError)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		for name, value := range ai.GetEnvHeaders(providerKey) {
			req.Header.Set(name, value)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return cli.Exit(fmt.Sprintf("discover models: %v", err), ExitProviderError)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close models response body: %w", closeErr)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return cli.Exit(fmt.Sprintf("discover models: status %d: %s", resp.StatusCode, string(body)), ExitProviderError)
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Object string `json:"object"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return cli.Exit(fmt.Sprintf("parse response: %v", err), ExitSubsystemError)
	}

	if len(result.Data) == 0 {
		fmt.Fprintln(os.Stderr, "No models discovered")
		return nil
	}

	fmt.Fprintf(os.Stderr, "Discovered %d models for %s:\n", len(result.Data), providerKey)
	for _, m := range result.Data {
		if err := stdoutLine("%s/%s\n", providerKey, m.ID); err != nil {
			return err
		}
	}

	if cmd.Bool("save") {
		// Discovered models belong in models.json under the provider's models
		// list. The flat settings.json schema has no place for them.
		modelsPath := filepath.Join(configDir(), "models.json")
		reg, err := ai.LoadModelRegistry(modelsPath)
		if err != nil {
			reg = ai.NewModelRegistry()
		}
		models := make([]string, len(result.Data))
		for i, m := range result.Data {
			models[i] = m.ID
		}
		if err := reg.SetCustomModels(providerKey, models); err != nil {
			return cli.Exit(fmt.Sprintf("save models: %v", err), ExitSubsystemError)
		}
		if err := ai.SaveModelRegistry(modelsPath, reg); err != nil {
			return cli.Exit(fmt.Sprintf("save models.json: %v", err), ExitSubsystemError)
		}
		fmt.Fprintf(os.Stderr, "Saved %d models to models.json\n", len(models))
	}

	return nil
}

// getAPIKeyFromCredential extracts an API key from a credential.
func getAPIKeyFromCredential(ctx context.Context, providerKey string, cred ai.Credential, fb *store.FileBackend) (string, error) {
	return store.APIKeyFromCredential(ctx, providerKey, cred)
}

// runSetModel handles the `tau set-model` command.
func runSetModel(ctx context.Context, cmd *cli.Command) error {
	if !cmd.Args().Present() {
		return cli.Exit("usage: tau set-model <model-spec>", ExitUserError)
	}

	modelSpec := cmd.Args().First()
	project := cmd.Bool("project")
	if project {
		if err := requireTrustedProject(ctx, cmd); err != nil {
			return err
		}
	}

	spec, err := ai.ParseModelSpec(modelSpec)
	if err != nil {
		return cli.Exit(fmt.Sprintf("parse model spec: %v", err), ExitUserError)
	}

	if !spec.IsFullyQualified() {
		settings, err := loadSettings()
		if err != nil {
			return cli.Exit(err.Error(), ExitSubsystemError)
		}
		resolved, err := ai.ResolveShortModel(spec.Model, settings)
		if err != nil {
			return cli.Exit(fmt.Sprintf("resolve model: %v", err), ExitProviderError)
		}
		spec = resolved
	}

	var settingsPath string
	if project {
		settingsPath = ".tau/settings.json"
	} else {
		settingsPath = filepath.Join(configDir(), "settings.json")
	}

	if err := ai.UpdateSettings(ctx, settingsPath, map[string]any{"defaultProvider": spec.Provider, "defaultModel": spec.Model}); err != nil {
		return cli.Exit(fmt.Sprintf("save settings: %v", err), ExitSubsystemError)
	}

	fmt.Fprintf(os.Stderr, "Default model set to %s/%s\n", spec.Provider, spec.Model)
	return nil
}

// runShowModel handles the `tau show-model` command. Uses the same
// resolution chain a live session would: explicit override → settings →
// env-var fallback. Honors --model on the root command if set.
func runShowModel(ctx context.Context, cmd *cli.Command) error {
	rtOpts, _, _, _, err := buildRuntimeOptions(ctx, ui.NewNoOpUI(), cmd, withoutPlugins())
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	if explicit := cmd.String("model"); explicit != "" {
		rtOpts.ExplicitModel = explicit
	}
	rt, err := sdk.NewRuntime(rtOpts)
	if err != nil {
		return cli.Exit(fmt.Sprintf("init runtime: %v", err), ExitSubsystemError)
	}

	resolved, err := rt.ResolveModel()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		fmt.Fprintln(os.Stderr, "Run: tau set-model <provider>/<model>")
		return nil
	}

	writeLine(os.Stdout, "Provider: %s\n", resolved.Spec.Provider)
	writeLine(os.Stdout, "Model:    %s\n", resolved.Spec.Model)
	if resolved.Model != nil && resolved.Model.ID != resolved.Spec.Model {
		writeLine(os.Stdout, "Resolved Model: %s\n", resolved.Model.ID)
	}

	return nil
}

// runInstall handles the `tau install` command.
func runInstall(ctx context.Context, cmd *cli.Command) error {
	if !cmd.Args().Present() {
		return cli.Exit("usage: tau install <source> [--project] [--link] [--yes] [--force]", ExitUserError)
	}

	source := cmd.Args().First()
	var cfg packages.Config
	cfg.Source = source
	cfg.Project = cmd.Bool("project")
	cfg.Link = cmd.Bool("link")
	cfg.Yes = cmd.Bool("yes")
	cfg.Force = cmd.Bool("force")
	cfg.Pi = cmd.Bool("pi")
	if cfg.Project {
		if err := requireTrustedProject(ctx, cmd); err != nil {
			return err
		}
	}

	userDir, projectDir, err := packagesDirs()
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	mgr := packages.NewInstallManager(userDir, projectDir)
	settings, err := loadSettings()
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	mgr.NPMCommand = settings.NPMCommand

	mgr.ArtifactReloader = func(ctx context.Context) error {
		if sdk.GlobalArtifactLoader != nil {
			return sdk.GlobalArtifactLoader.Reload(ctx)
		}
		return nil
	}

	if err := mgr.Install(ctx, cfg); err != nil {
		return cli.Exit(fmt.Sprintf("install failed: %v", err), getExitCode(err))
	}

	return nil
}

// getExitCode extracts the exit code from an error.
func getExitCode(err error) int {
	if exitErr, ok := err.(cli.ExitCoder); ok {
		return exitErr.ExitCode()
	}
	return ExitSubsystemError
}

// runUninstall handles the `tau uninstall` command.
func runUninstall(ctx context.Context, cmd *cli.Command) error {
	if !cmd.Args().Present() {
		return cli.Exit("usage: tau uninstall <name> [--project] [--user]", ExitUserError)
	}

	name := cmd.Args().First()

	var scopeFlag packages.Scope
	if cmd.Bool("project") {
		scopeFlag = packages.ScopeProject
	} else if cmd.Bool("user") {
		scopeFlag = packages.ScopeUser
	}

	userDir, projectDir, err := packagesDirs()
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	mgr := packages.NewManager(userDir, projectDir)

	trusted, err := projectTrusted(ctx, cmd)
	if err != nil {
		return err
	}
	var scopesToCheck []packages.Scope
	switch {
	case scopeFlag == packages.ScopeProject && !trusted:
		return errProjectUntrusted
	case scopeFlag != "":
		scopesToCheck = []packages.Scope{scopeFlag}
	case trusted:
		scopesToCheck = []packages.Scope{packages.ScopeProject, packages.ScopeUser}
	default:
		scopesToCheck = []packages.Scope{packages.ScopeUser}
	}

	// Check for existence and scope conflict before uninstalling
	var installPath string
	var foundIn []packages.Scope
	for _, scope := range scopesToCheck {
		path := mgr.InstallPath(name, scope)
		if _, err := os.Stat(path); err == nil {
			foundIn = append(foundIn, scope)
			installPath = path
		}
	}

	if len(foundIn) == 0 {
		return cli.Exit(fmt.Sprintf("package %q not found", name), ExitUserError)
	}

	if len(foundIn) > 1 {
		return cli.Exit(
			fmt.Sprintf("package %q installed in multiple scopes (project, user); use --project or --user to specify which to uninstall", name),
			ExitUserError,
		)
	}

	scope := foundIn[0]

	// Load package metadata BEFORE removing install artifacts so we can
	// inspect plugin ownership.
	var manifest *packages.Manifest
	{
		manifestPath := filepath.Join(installPath, "tau-package.toml")
		if _, statErr := os.Stat(manifestPath); statErr == nil {
			var loadErr error
			manifest, loadErr = packages.Load(manifestPath)
			if loadErr != nil {
				// Package metadata is corrupt; proceed without plugin cleanup.
				manifest = nil
			}
		}
	}

	settings, err := loadSettings()
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	mgr.NPMCommand = settings.NPMCommand
	// Use the Manager's Uninstall method which handles npm packages correctly
	if err := mgr.Uninstall(ctx, name, scope); err != nil {
		return cli.Exit(fmt.Sprintf("uninstall failed: %v", err), ExitSubsystemError)
	}

	// Clear plugin state by owned plugin name (from manifest), not package name.
	// Plugin state lives at ~/.tau/agent/plugin-state/<plugin-name>/.
	if manifest != nil && manifest.HasPlugin() {
		// Use the explicit [plugin].name if present, otherwise fall back to
		// the package name as the conventional plugin name.
		pluginName := manifest.Plugin.Name
		if pluginName == "" {
			pluginName = manifest.Package.Name
		}
		if err := state.RemovePlugin(configDir(), pluginName); err != nil {
			fmt.Fprintf(os.Stderr, "warning: clear plugin state: %v\n", err)
		}
	}

	fmt.Fprintf(os.Stderr, "Uninstalled %s\n", name)
	return nil
}

// runUpdate handles the `tau update` command.
func runUpdate(ctx context.Context, cmd *cli.Command) error {
	scope := packages.ScopeUser
	if cmd.Bool("project") {
		if err := requireTrustedProject(ctx, cmd); err != nil {
			return err
		}
		scope = packages.ScopeProject
	}

	names := cmd.Args().Slice()

	userDir, projectDir, err := packagesDirs()
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	updateMgr := packages.NewUpdateManager(userDir, projectDir)
	settings, err := loadSettings()
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	updateMgr.NPMCommand = settings.NPMCommand

	updateMgr.ArtifactReloader = func(ctx context.Context) error {
		if sdk.GlobalArtifactLoader != nil {
			return sdk.GlobalArtifactLoader.Reload(ctx)
		}
		return nil
	}
	updateMgr.PluginReloader = func(ctx context.Context, pluginName string) error {
		rtOpts, pluginMgr, _, _, err := buildRuntimeOptions(ctx, ui.NewNoOpUI(), nil)
		if err != nil {
			return cli.Exit(err.Error(), ExitUserError)
		}
		defer func() {
			if pluginMgr != nil {
				reportShutdownError(pluginMgr.Shutdown(ctx))
			}
		}()
		if pluginMgr != nil {
			if err := pluginMgr.ReloadNamed(ctx, pluginName); err != nil {
				return err
			}
		}
		_ = rtOpts
		return nil
	}

	if len(names) == 0 {
		manager := packages.NewManager(userDir, projectDir)
		installed, err := manager.ListInstalled(scope)
		if err != nil {
			return cli.Exit(fmt.Sprintf("list installed: %v", err), ExitSubsystemError)
		}
		names = installed
	}

	var updateErr error
	for _, name := range names {
		result, err := updateMgr.Update(ctx, name, scope)
		if err != nil {
			updateErr = errors.Join(updateErr, fmt.Errorf("update %s: %w", name, err))
			if context.Cause(ctx) != nil {
				break
			}
			continue
		}

		if result.Updated {
			fmt.Fprintf(os.Stderr, "Updated %s: %s\n", name, result.Message)
		} else {
			fmt.Fprintf(os.Stderr, "Skipped %s: %s\n", name, result.Message)
		}
	}

	if updateErr != nil {
		return cli.Exit(updateErr.Error(), ExitSubsystemError)
	}
	return nil
}

// runPackagesList handles the `tau packages list` subcommand.
func runPackagesList(ctx context.Context, cmd *cli.Command) error {
	userOnly := cmd.Bool("user")
	projectOnly := cmd.Bool("project")

	userDir, projectDir, err := packagesDirs()
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	mgr := packages.NewManager(userDir, projectDir)

	showUser := !projectOnly
	showProject := !userOnly

	if showUser {
		names, err := mgr.ListInstalled(packages.ScopeUser)
		if err != nil {
			return cli.Exit(fmt.Sprintf("list user packages: %v", err), ExitSubsystemError)
		}
		for _, name := range names {
			path := mgr.InstallPath(name, packages.ScopeUser)
			info, _ := packages.GetInfo(path)
			if info != nil {
				writeLine(os.Stdout, "%s | %s | user | %s | %s | %s\n", packageListName(info.Name, path), info.Version, info.Source, info.Ref, info.InstalledAt)
			} else {
				writeLine(os.Stdout, "%s | (unknown) | user\n", packageListName(name, path))
			}
		}
	}

	if showProject {
		names, err := mgr.ListInstalled(packages.ScopeProject)
		if err != nil {
			return cli.Exit(fmt.Sprintf("list project packages: %v", err), ExitSubsystemError)
		}
		for _, name := range names {
			path := mgr.InstallPath(name, packages.ScopeProject)
			info, _ := packages.GetInfo(path)
			if info != nil {
				writeLine(os.Stdout, "%s | %s | project | %s | %s | %s\n", packageListName(info.Name, path), info.Version, info.Source, info.Ref, info.InstalledAt)
			} else {
				writeLine(os.Stdout, "%s | (unknown) | project\n", packageListName(name, path))
			}
		}
	}

	return nil
}

func packageListName(name, path string) string {
	sidecar, err := packages.LoadSidecar(path)
	if err == nil && sidecar.Pi {
		return name + " (pi-ext)"
	}
	return name
}

// runPackagesInfo handles the `tau packages info` subcommand.
func runPackagesInfo(ctx context.Context, cmd *cli.Command) error {
	if !cmd.Args().Present() {
		return cli.Exit("usage: tau packages info <name>", ExitUserError)
	}

	name := cmd.Args().First()
	userDir, projectDir, err := packagesDirs()
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	mgr := packages.NewManager(userDir, projectDir)

	var info *packages.Info
	var pkgPath string

	userPath := mgr.InstallPath(name, packages.ScopeUser)
	info, _ = packages.GetInfo(userPath)
	if info != nil {
		pkgPath = userPath
	} else {
		projectPath := mgr.InstallPath(name, packages.ScopeProject)
		info, _ = packages.GetInfo(projectPath)
		if info != nil {
			pkgPath = projectPath
		}
	}

	if info == nil {
		return cli.Exit(fmt.Sprintf("package %q not found", name), ExitUserError)
	}

	manifestPath := filepath.Join(pkgPath, "tau-package.toml")
	manifest, _ := packages.Load(manifestPath)
	sidecar, _ := packages.LoadSidecar(pkgPath)

	if err := stdoutLine("Name:        %s\n", info.Name); err != nil {
		return err
	}
	if err := stdoutLine("Version:     %s\n", info.Version); err != nil {
		return err
	}
	writeLine(os.Stdout, "Description: %s\n", info.Description)

	if manifest != nil {
		if len(manifest.Package.Authors) > 0 {
			writeLine(os.Stdout, "Authors:     %s\n", strings.Join(manifest.Package.Authors, ", "))
		}
		if manifest.Package.License != "" {
			writeLine(os.Stdout, "License:     %s\n", manifest.Package.License)
		}
		if manifest.Package.Homepage != "" {
			writeLine(os.Stdout, "Homepage:    %s\n", manifest.Package.Homepage)
		}
		if manifest.HasPlugin() {
			writeLine(os.Stdout, "Plugin:      api_version=%s\n", manifest.Plugin.APIVersion)
		}
		if manifest.Build.Command != "" {
			writeLine(os.Stdout, "Build:       %s\n", manifest.Build.Command)
		}
		if manifest.Run.Command != "" {
			writeLine(os.Stdout, "Run:         %s\n", manifest.Run.Command)
		}
		skills := manifest.AllSkills()
		if len(skills) > 0 {
			writeLine(os.Stdout, "Skills:      %d artifact(s)\n", len(skills))
			for _, s := range skills {
				writeLine(os.Stdout, "             - %s\n", s.Path)
			}
		}
		prompts := manifest.AllPrompts()
		if len(prompts) > 0 {
			writeLine(os.Stdout, "Prompts:     %d artifact(s)\n", len(prompts))
			for _, p := range prompts {
				writeLine(os.Stdout, "             - %s\n", p.Path)
			}
		}
		themes := manifest.AllThemes()
		if len(themes) > 0 {
			writeLine(os.Stdout, "Themes:      %d artifact(s)\n", len(themes))
			for _, t := range themes {
				writeLine(os.Stdout, "             - %s\n", t.Path)
			}
		}
	}

	writeLine(os.Stdout, "\n## Install Metadata\n\n")
	writeLine(os.Stdout, "Source:      %s\n", info.Source)
	writeLine(os.Stdout, "Ref:         %s\n", info.Ref)
	writeLine(os.Stdout, "Commit:      %s\n", info.CommitSHA)
	writeLine(os.Stdout, "Installed:   %s\n", info.InstalledAt)
	writeLine(os.Stdout, "Scope:       %s\n", info.Scope)
	writeLine(os.Stdout, "Path:        %s\n", info.Path)

	if sidecar != nil {
		writeLine(os.Stdout, "\n## .tau-install.json\n\n")
		sidecarBytes, _ := json.MarshalIndent(sidecar, "", "  ")
		writeLine(os.Stdout, "%s\n", string(sidecarBytes))
	}

	if manifest != nil {
		manifestBytes, _ := os.ReadFile(manifestPath)
		if len(manifestBytes) > 0 {
			writeLine(os.Stdout, "\n## tau-package.toml\n\n")
			writeLine(os.Stdout, "%s\n", string(manifestBytes))
		}
	}

	return nil
}

// runPackagesSearch handles the `tau packages search` subcommand.
func runPackagesSearch(ctx context.Context, cmd *cli.Command) error {
	if !cmd.Args().Present() {
		return cli.Exit("usage: tau packages search <query>", ExitUserError)
	}

	query := strings.ToLower(cmd.Args().First())
	userDir, projectDir, err := packagesDirs()
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	mgr := packages.NewManager(userDir, projectDir)

	names, err := mgr.ListInstalled(packages.ScopeUser)
	if err != nil {
		return cli.Exit(fmt.Sprintf("list user packages: %v", err), ExitSubsystemError)
	}

	found := false
	for _, name := range names {
		if strings.Contains(strings.ToLower(name), query) {
			path := mgr.InstallPath(name, packages.ScopeUser)
			info, _ := packages.GetInfo(path)
			if info != nil {
				writeLine(os.Stdout, "%s (user, %s)\n", info.Name, info.Version)
			} else {
				writeLine(os.Stdout, "%s (user)\n", name)
			}
			found = true
		}
	}

	names, err = mgr.ListInstalled(packages.ScopeProject)
	if err != nil {
		return cli.Exit(fmt.Sprintf("list project packages: %v", err), ExitSubsystemError)
	}

	for _, name := range names {
		if strings.Contains(strings.ToLower(name), query) {
			path := mgr.InstallPath(name, packages.ScopeProject)
			info, _ := packages.GetInfo(path)
			if info != nil {
				writeLine(os.Stdout, "%s (project, %s)\n", info.Name, info.Version)
			} else {
				writeLine(os.Stdout, "%s (project)\n", name)
			}
			found = true
		}
	}

	if !found {
		fmt.Fprintf(os.Stderr, "No packages found matching %q\n", query)
	}

	return nil
}

// --- Sessions Commands ---

// runSessionsList handles the `tau sessions list` subcommand.
func runSessionsList(ctx context.Context, cmd *cli.Command) error {
	opts := session.ListOptions{
		Limit: cmd.Int("limit"),
		Since: cmd.String("since"),
		CWD:   cmd.String("cwd"),
	}

	dir, err := sessionDirForCmd(cmd)
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	sessCLI, err := session.NewCLI(dir)
	if err != nil {
		return cli.Exit(fmt.Sprintf("create sessions CLI: %v", err), ExitSubsystemError)
	}

	summaries, err := sessCLI.List(ctx, opts)
	if err != nil {
		return cli.Exit(fmt.Sprintf("list sessions: %v", err), ExitSubsystemError)
	}

	if len(summaries) == 0 {
		fmt.Fprintln(os.Stderr, "No sessions found.")
		return nil
	}

	if err := renderSessionsList(os.Stdout, summaries, terminalWidth()); err != nil {
		return err
	}

	return nil
}

func sessionListName(s session.SessionSummary) string {
	if s.FirstPrompt != "" && session.SlugFromName(sessionListPromptName(s.FirstPrompt)) == s.Name {
		return sessionListPromptName(s.FirstPrompt)
	}
	if s.Name != "" {
		return s.Name
	}
	if s.FirstPrompt != "" {
		return sessionListPromptName(s.FirstPrompt)
	}
	return "(unnamed)"
}

func sessionListPromptName(prompt string) string {
	for line := range strings.Lines(prompt) {
		line = strings.Join(strings.Fields(line), " ")
		if line != "" {
			return line
		}
	}
	return "(unnamed)"
}

func terminalWidth() int {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || width <= 0 {
		return 120
	}
	return width
}

type sessionListRow struct {
	id     string
	date   string
	name   string
	cwd    string
	msgs   string
	tokens string
}

func renderSessionsList(w io.Writer, summaries []session.SessionSummary, width int) error {
	rows := make([]sessionListRow, len(summaries))
	idWidth := shortestUniqueIDWidth(summaries)
	widths := []int{idWidth, len("Date"), len("Name"), len("CWD"), len("Msgs"), len("Tokens")}

	for i, s := range summaries {
		name := sessionListName(s)
		date := s.Created
		if len(date) > len("2006-01-02") {
			date = date[:len("2006-01-02")]
		}
		row := sessionListRow{
			id:     prefixRunes(s.ID, idWidth),
			date:   date,
			name:   name,
			cwd:    s.CWD,
			msgs:   strconv.Itoa(s.MessageCount),
			tokens: strconv.Itoa(s.Tokens.Input + s.Tokens.Output),
		}
		rows[i] = row
		widths[1] = max(widths[1], cellWidth(row.date))
		widths[4] = max(widths[4], cellWidth(row.msgs))
		widths[5] = max(widths[5], cellWidth(row.tokens))
	}

	fixed := widths[0] + widths[1] + widths[4] + widths[5] + len(" | ")*5
	flex := width - fixed
	if flex < 2 {
		flex = 2
	}
	widths[2] = flex / 2
	widths[3] = flex - widths[2]

	out := &sessionListWriter{w: w}
	out.Printf("%s | %s | %s | %s | %s | %s\n",
		padRight("ID", widths[0]), padRight("Date", widths[1]), padRight("Name", widths[2]), padRight("CWD", widths[3]), padLeft("Msgs", widths[4]), padLeft("Tokens", widths[5]))
	out.Printf("%s-|-%s-|-%s-|-%s-|-%s-|-%s\n",
		strings.Repeat("-", widths[0]), strings.Repeat("-", widths[1]), strings.Repeat("-", widths[2]), strings.Repeat("-", widths[3]), strings.Repeat("-", widths[4]), strings.Repeat("-", widths[5]))
	for _, row := range rows {
		out.Printf("%s | %s | %s | %s | %s | %s\n",
			padRight(row.id, widths[0]), padRight(row.date, widths[1]), padRight(row.name, widths[2]), padRight(row.cwd, widths[3]), padLeft(row.msgs, widths[4]), padLeft(row.tokens, widths[5]))
	}
	if out.err != nil {
		return fmt.Errorf("write sessions list: %w", out.err)
	}
	return nil
}

type sessionListWriter struct {
	w   io.Writer
	err error
}

func (w *sessionListWriter) Printf(format string, args ...any) {
	if w.err != nil {
		return
	}
	_, w.err = fmt.Fprintf(w.w, format, args...)
}

func shortestUniqueIDWidth(summaries []session.SessionSummary) int {
	maxLen := len("ID")
	for _, s := range summaries {
		maxLen = max(maxLen, cellWidth(s.ID))
	}
	for width := len("ID"); width <= maxLen; width++ {
		seen := make(map[string]struct{}, len(summaries))
		unique := true
		for _, s := range summaries {
			id := prefixRunes(s.ID, width)
			if _, ok := seen[id]; ok {
				unique = false
				break
			}
			seen[id] = struct{}{}
		}
		if unique {
			return width
		}
	}
	return maxLen
}

func padRight(s string, width int) string {
	s = truncateCell(s, width)
	return s + strings.Repeat(" ", max(0, width-cellWidth(s)))
}

func padLeft(s string, width int) string {
	s = truncateCell(s, width)
	return strings.Repeat(" ", max(0, width-cellWidth(s))) + s
}

func truncateCell(s string, width int) string {
	if width <= 0 || cellWidth(s) <= width {
		return s
	}
	if width <= 3 {
		return prefixRunes(s, width)
	}
	return prefixRunes(s, width-3) + "..."
}

func cellWidth(s string) int {
	return utf8.RuneCountInString(s)
}

func prefixRunes(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if cellWidth(s) <= width {
		return s
	}
	for i := range s {
		if width == 0 {
			return s[:i]
		}
		width--
	}
	return s
}

// runSessionsShow handles the `tau sessions show` subcommand.
func runSessionsShow(ctx context.Context, cmd *cli.Command) error {
	if !cmd.Args().Present() {
		return cli.Exit("usage: tau sessions show <id>", ExitUserError)
	}
	id := cmd.Args().First()

	dir, err := sessionDirForCmd(cmd)
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	sessCLI, err := session.NewCLI(dir)
	if err != nil {
		return cli.Exit(fmt.Sprintf("create sessions CLI: %v", err), ExitSubsystemError)
	}

	if err := sessCLI.Show(ctx, id, os.Stdout); err != nil {
		return cli.Exit(fmt.Sprintf("show session: %v", err), ExitSubsystemError)
	}

	return nil
}

// runSessionsResume handles the `tau sessions resume` subcommand.
// runSessionsResume handles the `tau sessions resume <id> [prompt]` subcommand.
// It creates a runtime and session targeting the given session ID, then executes
// the provided prompts (or reads from stdin if none given), and optionally exports.
func runSessionsResume(ctx context.Context, cmd *cli.Command) error {
	if !cmd.Args().Present() {
		return cli.Exit("usage: tau sessions resume <id> [prompt...]", ExitUserError)
	}
	id := cmd.Args().First()
	recliing := cmd.Args().Slice()[1:]

	// Validate the session exists before doing any work.
	dir, err := sessionDirForCmd(cmd)
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	sessCLI, err := session.NewCLI(dir)
	if err != nil {
		return cli.Exit(fmt.Sprintf("create sessions CLI: %v", err), ExitSubsystemError)
	}
	if err := sessCLI.Show(ctx, id, io.Discard); err != nil {
		return cli.Exit(fmt.Sprintf("session not found: %v", err), ExitUserError)
	}

	// Build prompts: use recliing args, or --message flags, or stdin.
	messages := cmd.StringSlice("message")
	prompts, fileImages, err := buildPromptSequenceWithFiles(recliing, messages, os.Stdin, isTTY(), cmd.Bool("stdin"))
	if err != nil {
		return cli.Exit(fmt.Sprintf("read stdin: %v", err), ExitUserError)
	}
	if len(prompts) == 0 {
		return cli.Exit("no prompt provided", ExitUserError)
	}

	// Set up signal handling.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT)
	defer signal.Stop(sigChan)

	// Set up UI.
	uiImpl := choosePrintModeUI()

	// Build runtime options, wiring the resume ID directly.
	rtOpts, pluginMgr, loader, reg, sessionFlags, err := buildConversationRuntimeOptions(ctx, uiImpl)
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	GlobalCommandRegistry = reg
	GlobalPluginManager = pluginMgr
	// This path never opens the TUI, so the resource loader has no consumer here.
	_ = loader
	if sessionFlags.SelectionError != nil {
		return cli.Exit(fmt.Sprintf("%v", sessionFlags.SelectionError), ExitUserError)
	}

	// Override: target the explicit session ID instead of any auto-detection.
	rtOpts.ResumeSessionID = id

	rt, err := sdk.NewRuntime(rtOpts)
	if err != nil {
		return cli.Exit(fmt.Sprintf("create runtime: %v", err), ExitSubsystemError)
	}
	defer func() { reportShutdownError(rt.Close(ctx)) }()

	// Set up graceful shutdown.
	interruptChan := make(chan struct{}, 1)
	var interrupted atomic.Bool
	doubleSigintWindow := 2 * time.Second
	lastSIGINT := time.Time{}

	go func() {
		for sig := range sigChan {
			if sig == syscall.SIGINT {
				now := time.Now()
				if interrupted.Load() && now.Sub(lastSIGINT) < doubleSigintWindow {
					interruptChan <- struct{}{}
					return
				}
				interrupted.Store(true)
				lastSIGINT = now
				interruptChan <- struct{}{}
				fmt.Fprintln(os.Stderr, "\ninterrupted; ctrl-c again to exit")
			}
		}
	}()

	// Create a session targeting the resume ID.
	sess, err := rt.NewSession(ctx, sdk.SessionOptions{
		SessionID: id,
		Model:     rtOpts.ExplicitModel,
		CWD:       getCWD(),
		UI:        uiImpl,
	})
	if err != nil {
		return cli.Exit(fmt.Sprintf("create session: %v", err), ExitSubsystemError)
	}
	defer func() { reportShutdownError(sess.Close(ctx)) }()

	var printMu sync.Mutex
	printer := &eventPrinter{NoStream: rootCmdFromContext(ctx).Bool("no-stream")}

	for i, prompt := range prompts {
		msg, err := buildCLIMessage(ctx, sess, prompt, i == 0, fileImages)
		if err != nil {
			return cli.Exit(fmt.Sprintf("%v", err), ExitUserError)
		}

		events, err := sess.Send(ctx, msg)
		if err != nil {
			return cli.Exit(fmt.Sprintf("send message: %v", err), ExitSubsystemError)
		}

		for {
			select {
			case <-interruptChan:
				_ = sess.Interrupt()
				return cli.Exit("", ExitSIGINT)
			case <-ctx.Done():
				return cli.Exit("", ExitSIGINT)
			case event, ok := <-events:
				if !ok {
					goto nextPrompt
				}
				processEventWith(&event, &printMu, printer)
			}
		}
	nextPrompt:
		if printer.Err != nil {
			return cli.Exit(printer.Err.Error(), ExitProviderError)
		}
	}

	// Handle export if --export was provided.
	if exportPath := cmd.String("export"); exportPath != "" {
		if err := doExport(sess.ID(), rtOpts.SessionsDir, exportPath, ""); err != nil {
			return cli.Exit(fmt.Sprintf("export failed: %v", err), ExitSubsystemError)
		}
	}

	if err := printer.result(); err != nil {
		return cli.Exit(err.Error(), ExitProviderError)
	}
	return nil
}

// runSessionsRename handles the `tau sessions rename` subcommand.
func runSessionsRename(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 2 {
		return cli.Exit("usage: tau sessions rename <id> <new-name>", ExitUserError)
	}
	id, newName := args[0], args[1]

	dir, err := sessionDirForCmd(cmd)
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	sessCLI, err := session.NewCLI(dir)
	if err != nil {
		return cli.Exit(fmt.Sprintf("create sessions CLI: %v", err), ExitSubsystemError)
	}

	if err := sessCLI.Rename(ctx, id, newName); err != nil {
		return cli.Exit(fmt.Sprintf("rename session: %v", err), ExitSubsystemError)
	}

	fmt.Fprintf(os.Stderr, "Session %.8s renamed to %q\n", id, newName)
	return nil
}

// runSessionsMove handles the `tau sessions move` subcommand.
func runSessionsMove(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 2 {
		return cli.Exit("usage: tau sessions move <id> <cwd>", ExitUserError)
	}
	id, newCWD := args[0], args[1]

	dir, err := sessionDirForCmd(cmd)
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	sessCLI, err := session.NewCLI(dir)
	if err != nil {
		return cli.Exit(fmt.Sprintf("create sessions CLI: %v", err), ExitSubsystemError)
	}

	oldCWD, err := sessCLI.MoveCWD(ctx, id, newCWD)
	if err != nil {
		return cli.Exit(fmt.Sprintf("move session: %v", err), ExitSubsystemError)
	}

	fmt.Fprintf(os.Stderr, "Session %.8s moved from %s to %s\n", id, oldCWD, newCWD)
	return nil
}

// runSessionsDelete handles the `tau sessions delete` subcommand.
func runSessionsDelete(ctx context.Context, cmd *cli.Command) error {
	if !cmd.Args().Present() {
		return cli.Exit("usage: tau sessions delete <id>", ExitUserError)
	}
	id := cmd.Args().First()

	dir, err := sessionDirForCmd(cmd)
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	sessCLI, err := session.NewCLI(dir)
	if err != nil {
		return cli.Exit(fmt.Sprintf("create sessions CLI: %v", err), ExitSubsystemError)
	}

	if err := sessCLI.Delete(ctx, id); err != nil {
		return cli.Exit(fmt.Sprintf("delete session: %v", err), ExitSubsystemError)
	}

	fmt.Fprintf(os.Stderr, "Session %.8s moved to trash\n", id)
	return nil
}

// runSessionsCleanupEmpty handles the `tau sessions cleanup-empty` subcommand.
func runSessionsCleanupEmpty(ctx context.Context, cmd *cli.Command) error {
	dir, err := sessionDirForCmd(cmd)
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	sessCLI, err := session.NewCLI(dir)
	if err != nil {
		return cli.Exit(fmt.Sprintf("create sessions CLI: %v", err), ExitSubsystemError)
	}

	result, err := sessCLI.CleanupEmpty(ctx, session.CleanupEmptyOptions{DryRun: cmd.Bool("dry-run")})
	if err != nil {
		return cli.Exit(fmt.Sprintf("cleanup empty sessions: %v", err), ExitSubsystemError)
	}

	if len(result.Matched) == 0 && len(result.OrphanDataDirs) == 0 {
		fmt.Fprintln(os.Stderr, "No empty sessions found.")
		return nil
	}

	if cmd.Bool("dry-run") {
		for _, s := range result.Matched {
			writeLine(os.Stdout, "%s  %s  %s\n", s.ID[:8], s.Created[:10], s.Filename)
		}
		fmt.Fprintf(os.Stderr, "Found %d empty session(s) and %d orphan data dir(s). Re-run without --dry-run to move/remove them.\n", len(result.Matched), len(result.OrphanDataDirs))
		return nil
	}

	fmt.Fprintf(os.Stderr, "Moved %d empty session(s) to trash and removed %d data dir(s).\n", len(result.Removed), len(result.RemovedDataDirs))
	return nil
}

// runSessionsRestore handles the `tau sessions restore` subcommand.
func runSessionsRestore(ctx context.Context, cmd *cli.Command) error {
	if !cmd.Args().Present() {
		return cli.Exit("usage: tau sessions restore <id>", ExitUserError)
	}
	id := cmd.Args().First()

	dir, err := sessionDirForCmd(cmd)
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	sessCLI, err := session.NewCLI(dir)
	if err != nil {
		return cli.Exit(fmt.Sprintf("create sessions CLI: %v", err), ExitSubsystemError)
	}

	if err := sessCLI.Restore(ctx, id); err != nil {
		return cli.Exit(fmt.Sprintf("restore session: %v", err), ExitSubsystemError)
	}

	fmt.Fprintf(os.Stderr, "Session %.8s restored from trash\n", id)
	return nil
}

// runSessionsCompact handles the `tau sessions compact` subcommand.
func runSessionsCompact(ctx context.Context, cmd *cli.Command) error {
	if !cmd.Args().Present() {
		return cli.Exit("usage: tau sessions compact <id>", ExitUserError)
	}
	id := cmd.Args().First()

	dir, err := sessionDirForCmd(cmd)
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	sessCLI, err := session.NewCLI(dir)
	if err != nil {
		return cli.Exit(fmt.Sprintf("create sessions CLI: %v", err), ExitSubsystemError)
	}

	if err := sessCLI.Compact(ctx, id); err != nil {
		return cli.Exit(fmt.Sprintf("compact session: %v", err), ExitSubsystemError)
	}

	fmt.Fprintf(os.Stderr, "Session %.8s compacted\n", id)
	return nil
}

// runSessionsSummarizeBranch handles the `tau sessions summarize-branch` subcommand.
func runSessionsSummarizeBranch(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 2 {
		return cli.Exit("usage: tau sessions summarize-branch <id> <branch-tip>", ExitUserError)
	}
	id, branchTip := args[0], args[1]

	dir, err := sessionDirForCmd(cmd)
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	sessCLI, err := session.NewCLI(dir)
	if err != nil {
		return cli.Exit(fmt.Sprintf("create sessions CLI: %v", err), ExitSubsystemError)
	}

	if err := sessCLI.SummarizeBranch(ctx, id, branchTip); err != nil {
		return cli.Exit(fmt.Sprintf("summarize branch: %v", err), ExitSubsystemError)
	}

	fmt.Fprintf(os.Stderr, "Branch %.8s summarized\n", branchTip)
	return nil
}

// runSessionsFork handles the `tau sessions fork` subcommand.
func runSessionsFork(ctx context.Context, cmd *cli.Command) error {
	if !cmd.Args().Present() {
		return cli.Exit("usage: tau sessions fork <id>", ExitUserError)
	}
	id := cmd.Args().First()

	var atEntry *string
	if at := cmd.String("at"); at != "" {
		atEntry = &at
	}

	dir, err := sessionDirForCmd(cmd)
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	sessCLI, err := session.NewCLI(dir)
	if err != nil {
		return cli.Exit(fmt.Sprintf("create sessions CLI: %v", err), ExitSubsystemError)
	}

	newID, err := sessCLI.Fork(ctx, id, atEntry)
	if err != nil {
		return cli.Exit(fmt.Sprintf("fork session: %v", err), ExitSubsystemError)
	}

	fmt.Fprintf(os.Stderr, "Forked session %.8s -> %.8s\n", id, newID)
	return nil
}

// runSessionsExport handles the `tau sessions export` subcommand.
func runSessionsExport(ctx context.Context, cmd *cli.Command) error {
	if !cmd.Args().Present() {
		return cli.Exit("usage: tau sessions export <id> [--format html|json|md] [--output FILE]", ExitUserError)
	}
	id := cmd.Args().First()

	opts := session.ExportOptions{
		Format:     cmd.String("format"),
		Output:     cmd.String("output"),
		NoCollapse: cmd.Bool("no-collapse"),
	}

	dir, err := sessionDirForCmd(cmd)
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	sessCLI, err := session.NewCLI(dir)
	if err != nil {
		return cli.Exit(fmt.Sprintf("create sessions CLI: %v", err), ExitSubsystemError)
	}

	if err := sessCLI.Export(ctx, id, opts); err != nil {
		return cli.Exit(fmt.Sprintf("export session: %v", err), ExitSubsystemError)
	}

	if opts.Output != "" {
		fmt.Fprintf(os.Stderr, "Exported session %.8s to %s\n", id, opts.Output)
	}
	return nil
}

// copyToClipboard copies the given text to the system clipboard.
func copyToClipboard(text string) bool {
	return copyToClipboardWithXclip(text) || copyToClipboardWithXsel(text)
}

// copyToClipboardWithXclip uses xclip to copy text to clipboard.
func copyToClipboardWithXclip(text string) bool {
	cmd := exec.Command("xclip", "-selection", "clipboard")
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run() == nil
}

// copyToClipboardWithXsel uses xsel to copy text to clipboard.
func copyToClipboardWithXsel(text string) bool {
	cmd := exec.Command("xsel", "--clipboard")
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run() == nil
}

// --- Other Commands ---

// runRPC handles the `tau rpc` command.
func runRPC(ctx context.Context, cmd *cli.Command) error {
	uiImpl := choosePrintModeUI()

	rtOpts, pluginMgr, loader, reg, err := buildRuntimeOptions(ctx, uiImpl, nil)
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	GlobalCommandRegistry = reg
	GlobalPluginManager = pluginMgr
	if loader != nil {
		sdk.GlobalArtifactLoader = loader
		rtOpts.ArtifactLoader = loader
	}
	rtOpts.PluginManager = pluginMgr

	rt, err := sdk.NewRuntime(rtOpts)
	if err != nil {
		return cli.Exit(fmt.Sprintf("create runtime: %v", err), ExitSubsystemError)
	}
	defer func() { reportShutdownError(rt.Close(ctx)) }()

	srv := server.NewServer(rt, reg, pluginMgr)

	// Reset SIGINT handling so we can manage it ourselves for RPC interrupt events.
	// The default SIGINT from signal.NotifyContext in cli() would cancel our context.
	signal.Reset(os.Interrupt)

	// Signal handler goroutine for SIGINT -> interrupt event translation.
	// Uses its own signal.Notify after resetting the inherited handler.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT)
	defer signal.Stop(sigChan)
	signalCtx, stopSignals := context.WithCancel(context.Background())
	signalsDone := make(chan struct{})
	go func() { defer close(signalsDone); forwardRPCSignals(signalCtx, sigChan, srv.SendInterrupt) }()
	defer func() { stopSignals(); <-signalsDone }()

	// Use a background context for the server so SIGINT is only handled by the
	// goroutine above (which emits interrupt events). The parent ctx is for CLI
	// lifecycle and must not be cancelled before the server finishes.
	if err := srv.Run(context.Background()); err != nil {
		return cli.Exit(fmt.Sprintf("rpc server error: %v", err), ExitSubsystemError)
	}

	return nil
}

func forwardRPCSignals(ctx context.Context, signals <-chan os.Signal, interrupt func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case sig, ok := <-signals:
			if !ok {
				return
			}
			if sig == syscall.SIGINT {
				interrupt()
			}
		}
	}
}

// choosePrintModeUI returns the appropriate UI for print mode.
func choosePrintModeUI() ui.UI {
	if isTTY() {
		return ui.NewStdinUI()
	}
	return ui.NewNoOpUI()
}

func runDaemon(ctx context.Context, cmd *cli.Command) error {
	if cmd.Bool("systemd-user-unit") {
		return printDaemonSystemdUnit(cmd.String("socket"))
	}

	uiImpl := ui.NewNoOpUI()
	rtOpts, pluginMgr, loader, reg, err := buildRuntimeOptions(ctx, uiImpl, nil)
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	GlobalCommandRegistry = reg
	GlobalPluginManager = pluginMgr
	if loader != nil {
		sdk.GlobalArtifactLoader = loader
		rtOpts.ArtifactLoader = loader
	}
	rtOpts.PluginManager = pluginMgr
	rtOpts.Logger = logging.NewText(os.Stderr, slog.LevelInfo)

	rt, err := sdk.NewRuntime(rtOpts)
	if err != nil {
		return cli.Exit(fmt.Sprintf("create runtime: %v", err), ExitSubsystemError)
	}
	defer func() { reportShutdownError(rt.Close(ctx)) }()

	path := cmd.String("socket")
	if path == "" {
		path = server.SocketPath()
	}

	if addr := cmd.String("websocket"); addr != "" {
		bridge, err := server.NewWebSocketBridge(addr, cmd.String("websocket-token"), rt, reg, pluginMgr, server.WithTokenStore(remoteTokenStore()))
		if err != nil {
			return cli.Exit(fmt.Sprintf("websocket bridge: %v", err), ExitUserError)
		}
		go func() {
			if err := bridge.Run(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "tau websocket bridge error: %v\n", err)
			}
		}()
		fmt.Fprintf(os.Stderr, "tau websocket bridge listening on %s/rpc\n", bridge.Addr())
	}

	fmt.Fprintf(os.Stderr, "tau daemon listening on %s\n", path)
	if err := server.RunUnix(ctx, path, rt, reg, pluginMgr); err != nil {
		return cli.Exit(fmt.Sprintf("daemon error: %v", err), ExitSubsystemError)
	}
	return nil
}

func printDaemonSystemdUnit(socketPath string) error {
	if socketPath == "" {
		socketPath = server.SocketPath()
	}
	unit := fmt.Sprintf(`[Unit]
Description=Tau user daemon

[Service]
Type=simple
ExecStart=tau daemon --socket %s
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
`, socketPath)
	return stdoutLine("%s", unit)
}

// remoteTokenStore returns the per-device token store for remote clients.
func remoteTokenStore() *server.TokenStore {
	return server.NewTokenStore(filepath.Join(configDir(), "remote-tokens.json"))
}

func runDaemonTokenAdd(_ context.Context, cmd *cli.Command) error {
	name := cmd.Args().First()
	if name == "" {
		return cli.Exit("usage: tau daemon token add <device-name>", ExitUserError)
	}
	tok, err := remoteTokenStore().Add(name)
	if err != nil {
		return cli.Exit(fmt.Sprintf("add token: %v", err), ExitSubsystemError)
	}
	writeLine(os.Stdout, "Token for %s (paste into the remote client once):\n%s\n", tok.Name, tok.Token)
	return nil
}

func runDaemonTokenList(_ context.Context, _ *cli.Command) error {
	tokens, err := remoteTokenStore().List()
	if err != nil {
		return cli.Exit(fmt.Sprintf("list tokens: %v", err), ExitSubsystemError)
	}
	if len(tokens) == 0 {
		writeLine(os.Stdout, "No device tokens. Create one with: tau daemon token add <device-name>\n")
		return nil
	}
	for _, tok := range tokens {
		prefix := tok.Token
		if len(prefix) > 8 {
			prefix = prefix[:8]
		}
		writeLine(os.Stdout, "%s\t%s…\tcreated %s\n", tok.Name, prefix, tok.Created.Format(time.RFC3339))
	}
	return nil
}

func runDaemonTokenRevoke(_ context.Context, cmd *cli.Command) error {
	name := cmd.Args().First()
	if name == "" {
		return cli.Exit("usage: tau daemon token revoke <device-name>", ExitUserError)
	}
	found, err := remoteTokenStore().Revoke(name)
	if err != nil {
		return cli.Exit(fmt.Sprintf("revoke token: %v", err), ExitSubsystemError)
	}
	if !found {
		return cli.Exit(fmt.Sprintf("no device token named %q", name), ExitUserError)
	}
	writeLine(os.Stdout, "Revoked token for %s\n", name)
	return nil
}

func runDoctor(ctx context.Context, cmd *cli.Command) error {
	socketPath := server.SocketPath()
	writeLine(os.Stdout, "Socket: %s\n", socketPath)
	dialCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "unix", socketPath)
	if err == nil {
		if closeErr := conn.Close(); closeErr != nil {
			return fmt.Errorf("close daemon probe: %w", closeErr)
		}
		writeLine(os.Stdout, "Daemon: reachable\n")
	} else {
		writeLine(os.Stdout, "Daemon: unavailable (%v)\n", err)
	}
	writeLine(os.Stdout, "TERM: %s\n", os.Getenv("TERM"))
	writeLine(os.Stdout, "TMUX: %t\n", os.Getenv("TMUX") != "")
	writeLine(os.Stdout, "SSH: %t\n", os.Getenv("SSH_CONNECTION") != "")
	return nil
}

// runExtensionsList handles the `tau list` and `tau extensions list` commands.
// It lists installed extensions from the merged user + project settings.
func runExtensionsList(_ context.Context, _ *cli.Command) error {
	settings, err := loadSettings()
	if err != nil {
		return cli.Exit(fmt.Sprintf("load settings: %v", err), ExitUserError)
	}
	var extensions []string
	if settings != nil {
		extensions = append(extensions, settings.ResourcePaths("extensions")...)
	}
	userDir, projectDir, err := packagesDirs()
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	piExtensions, err := piExtensionPackageLines(userDir, projectDir)
	if err != nil {
		return cli.Exit(fmt.Sprintf("list pi extensions: %v", err), ExitSubsystemError)
	}
	extensions = append(extensions, piExtensions...)
	if len(extensions) == 0 {
		writeLine(os.Stdout, "(no extensions installed)\n")
		return nil
	}
	for _, ext := range extensions {
		writeLine(os.Stdout, "%s\n", ext)
	}
	return nil
}

func piExtensionPackageLines(userDir, projectDir string) ([]string, error) {
	mgr := packages.NewManager(userDir, projectDir)
	var lines []string
	for _, scope := range []packages.Scope{packages.ScopeUser, packages.ScopeProject} {
		names, err := mgr.ListInstalled(scope)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			path := mgr.InstallPath(name, scope)
			sidecar, err := packages.LoadSidecar(path)
			if err == nil && sidecar.Pi {
				lines = append(lines, name+" (pi-ext)")
			}
		}
	}
	return lines, nil
}

// runReload handles the `tau reload` command.
func runReload(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	var pluginName string
	if len(args) > 0 {
		pluginName = args[0]
	}

	_, pluginMgr, loader, _, err := buildRuntimeOptions(ctx, ui.NewNoOpUI(), nil)
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	defer func() {
		if pluginMgr != nil {
			reportShutdownError(pluginMgr.Shutdown(ctx))
		}
	}()

	if pluginMgr != nil {
		if pluginName != "" {
			// Reload specific plugin
			if err := pluginMgr.ReloadNamed(ctx, pluginName); err != nil {
				return cli.Exit(fmt.Sprintf("reload error: %v", err), ExitSubsystemError)
			}
			fmt.Fprintf(os.Stderr, "Plugin %s reloaded\n", pluginName)
		} else {
			// Reload all plugins
			if err := pluginMgr.Reload(ctx); err != nil {
				return cli.Exit(fmt.Sprintf("reload error: %v", err), ExitSubsystemError)
			}
			fmt.Fprintln(os.Stderr, "Plugins reloaded")
		}
	}

	if loader != nil {
		if err := loader.Reload(ctx); err != nil {
			return cli.Exit(fmt.Sprintf("artifact reload error: %v", err), ExitSubsystemError)
		}
		fmt.Fprintln(os.Stderr, "Artifacts reloaded")
		return nil
	}

	if pluginName != "" {
		fmt.Fprintf(os.Stderr, "Reload complete for %s\n", pluginName)
	} else {
		fmt.Fprintln(os.Stderr, "Reload complete")
	}
	return nil
}

// runSkillsList handles the `tau skills list` subcommand.
func runSkillsList(ctx context.Context, cmd *cli.Command) error {
	_, _, loader, _, err := buildRuntimeOptions(ctx, ui.NewNoOpUI(), nil, withoutPlugins())
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	if loader == nil {
		return cli.Exit("No artifact loader configured", ExitSubsystemError)
	}

	registry := loader.SkillRegistry()
	if registry == nil {
		return cli.Exit("No skill registry available", ExitSubsystemError)
	}

	// Handle tier filter
	tierFilter := cmd.String("tier")
	var skills []*artifacts.Skill
	if tierFilter != "" {
		tier, err := artifacts.ParseTier(tierFilter)
		if err != nil {
			return cli.Exit(fmt.Sprintf("invalid tier %q: %v", tierFilter, err), ExitUserError)
		}
		skills = registry.ListByTier(tier)
	} else {
		skills = registry.List()
	}
	if cmd.Bool("no-builtin") {
		filtered := skills[:0]
		for _, skill := range skills {
			if skill.Tier != artifacts.TierBuiltin {
				filtered = append(filtered, skill)
			}
		}
		skills = filtered
	}

	if len(skills) == 0 {
		writeLine(os.Stdout, "No skills found\n")
		return nil
	}

	if err := stdoutLine("Available skills:\n"); err != nil {
		return err
	}
	if err := stdoutLine("\n"); err != nil {
		return err
	}
	for _, skill := range skills {
		tierLabel := ""
		if tierFilter == "" {
			tierLabel = fmt.Sprintf(" [%s]", skill.Tier)
		}
		writeLine(os.Stdout, "  /skill:%s%s\n", skill.ID, tierLabel)
		if skill.Description != "" {
			writeLine(os.Stdout, "    %s\n", skill.Description)
		}
		writeLine(os.Stdout, "    Source: %s\n", skill.SourcePath)
		writeLine(os.Stdout, "\n")
	}

	return nil
}

// runSkillsShow handles the `tau skills show` subcommand.
func runSkillsShow(ctx context.Context, cmd *cli.Command) error {
	if !cmd.Args().Present() {
		return cli.Exit("usage: tau skills show <skill-id>", ExitUserError)
	}

	_, _, loader, _, err := buildRuntimeOptions(ctx, ui.NewNoOpUI(), nil, withoutPlugins())
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	if loader == nil {
		return cli.Exit("No artifact loader configured", ExitSubsystemError)
	}

	registry := loader.SkillRegistry()
	if registry == nil {
		return cli.Exit("No skill registry available", ExitSubsystemError)
	}

	id := cmd.Args().First()
	skill, ok := registry.Get(id)
	if !ok {
		if matches := registry.Matches(id); len(matches) > 1 {
			var ids []string
			for _, match := range matches {
				ids = append(ids, match.ID)
			}
			return cli.Exit(fmt.Sprintf("skill %q is ambiguous; matches: %s", id, strings.Join(ids, ", ")), ExitUserError)
		}
		return cli.Exit(fmt.Sprintf("skill %q not found", id), ExitUserError)
	}

	writeLine(os.Stdout, "Skill: %s\n", skill.ID)
	writeLine(os.Stdout, "Name: %s\n", skill.Name)
	if skill.Description != "" {
		writeLine(os.Stdout, "Description: %s\n", skill.Description)
	}
	if skill.Version != "" {
		writeLine(os.Stdout, "Version: %s\n", skill.Version)
	}
	if len(skill.Tags) > 0 {
		writeLine(os.Stdout, "Tags: %s\n", strings.Join(skill.Tags, ", "))
	}
	writeLine(os.Stdout, "Tier: %s\n", skill.Tier)
	writeLine(os.Stdout, "Source: %s\n", skill.SourcePath)

	// Print extra fields if any
	if len(skill.ExtraFields) > 0 {
		writeLine(os.Stdout, "\n")
		writeLine(os.Stdout, "Extra fields:\n")
		for k, v := range skill.ExtraFields {
			writeLine(os.Stdout, "  %s: %v\n", k, v)
		}
	}

	// Print full body. Per the 2026-05-01 discoverable-artifacts spec the
	// `tau skills show` contract is "frontmatter + body + source path"; a
	// truncated preview hides the working content of any directory skill.
	if len(skill.Body) > 0 {
		writeLine(os.Stdout, "\n")
		writeLine(os.Stdout, "Body:\n")
		writeLine(os.Stdout, "%s\n", string(skill.Body))
	}

	return nil
}

// runPromptsList handles the `tau prompts list` subcommand.
func runPromptsList(ctx context.Context, cmd *cli.Command) error {
	_, _, loader, _, err := buildRuntimeOptions(ctx, ui.NewNoOpUI(), nil, withoutPlugins())
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	if loader == nil {
		return cli.Exit("No artifact loader configured", ExitSubsystemError)
	}

	registry := loader.PromptRegistry()
	if registry == nil {
		return cli.Exit("No prompt registry available", ExitSubsystemError)
	}

	// Handle tier filter
	tierFilter := cmd.String("tier")
	var prompts []*artifacts.Prompt
	if tierFilter != "" {
		tier, err := artifacts.ParseTier(tierFilter)
		if err != nil {
			return cli.Exit(fmt.Sprintf("invalid tier %q: %v", tierFilter, err), ExitUserError)
		}
		prompts = registry.ListByTier(tier)
	} else {
		prompts = registry.List()
	}

	if len(prompts) == 0 {
		writeLine(os.Stdout, "No prompt templates found\n")
		return nil
	}

	writeLine(os.Stdout, "Available prompts:\n")
	writeLine(os.Stdout, "\n")
	for _, prompt := range prompts {
		tierLabel := ""
		if tierFilter == "" {
			tierLabel = fmt.Sprintf(" [%s]", prompt.Tier)
		}
		writeLine(os.Stdout, "  /%s%s\n", prompt.ID, tierLabel)
		if prompt.Description != "" {
			writeLine(os.Stdout, "    %s\n", prompt.Description)
		}
		if prompt.ArgumentHint != "" {
			writeLine(os.Stdout, "    Args: %s\n", prompt.ArgumentHint)
		}
		writeLine(os.Stdout, "    Source: %s\n", prompt.SourcePath)
		writeLine(os.Stdout, "\n")
	}

	return nil
}

// runPromptsShow handles the `tau prompts show` subcommand.
func runPromptsShow(ctx context.Context, cmd *cli.Command) error {
	if !cmd.Args().Present() {
		return cli.Exit("usage: tau prompts show <prompt-id>", ExitUserError)
	}

	_, _, loader, _, err := buildRuntimeOptions(ctx, ui.NewNoOpUI(), nil, withoutPlugins())
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	if loader == nil {
		return cli.Exit("No artifact loader configured", ExitSubsystemError)
	}

	registry := loader.PromptRegistry()
	if registry == nil {
		return cli.Exit("No prompt registry available", ExitSubsystemError)
	}

	id := cmd.Args().First()
	prompt, ok := registry.Get(id)
	if !ok {
		if matches := registry.Matches(id); len(matches) > 1 {
			var ids []string
			for _, match := range matches {
				ids = append(ids, match.ID)
			}
			return cli.Exit(fmt.Sprintf("prompt %q is ambiguous; matches: %s", id, strings.Join(ids, ", ")), ExitUserError)
		}
		return cli.Exit(fmt.Sprintf("prompt %q not found", id), ExitUserError)
	}

	writeLine(os.Stdout, "Prompt: %s\n", prompt.ID)
	writeLine(os.Stdout, "Name: %s\n", prompt.Name)
	if prompt.Description != "" {
		writeLine(os.Stdout, "Description: %s\n", prompt.Description)
	}
	if prompt.ArgumentHint != "" {
		writeLine(os.Stdout, "Argument Hint: %s\n", prompt.ArgumentHint)
	}
	writeLine(os.Stdout, "Tier: %s\n", prompt.Tier)
	writeLine(os.Stdout, "Source: %s\n", prompt.SourcePath)

	// Print extra fields if any
	if len(prompt.ExtraFields) > 0 {
		writeLine(os.Stdout, "\n")
		writeLine(os.Stdout, "Extra fields:\n")
		for k, v := range prompt.ExtraFields {
			writeLine(os.Stdout, "  %s: %v\n", k, v)
		}
	}

	// Print body
	if len(prompt.Body) > 0 {
		writeLine(os.Stdout, "\n")
		writeLine(os.Stdout, "Body:\n")
		lines := strings.SplitN(string(prompt.Body), "\n", 50)
		for _, line := range lines {
			writeLine(os.Stdout, "  %s\n", line)
		}
	}

	return nil
}

func themeSource(theme *artifacts.Theme) string {
	if theme.ID == "system" && theme.SourcePath == "" {
		return "generated from terminal colors"
	}
	return theme.SourcePath
}

// runThemesList handles the `tau themes list` subcommand.
func runThemesList(ctx context.Context, cmd *cli.Command) error {
	_, _, loader, _, err := buildRuntimeOptions(ctx, ui.NewNoOpUI(), nil, withoutPlugins())
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	if loader == nil {
		return cli.Exit("No artifact loader configured", ExitSubsystemError)
	}

	registry := loader.ThemeRegistry()
	if registry == nil {
		return cli.Exit("No theme registry available", ExitSubsystemError)
	}

	// Handle tier filter
	tierFilter := cmd.String("tier")
	var themes []*artifacts.Theme
	if tierFilter != "" {
		tier, err := artifacts.ParseTier(tierFilter)
		if err != nil {
			return cli.Exit(fmt.Sprintf("invalid tier %q: %v", tierFilter, err), ExitUserError)
		}
		themes = registry.ListByTier(tier)
	} else {
		themes = registry.List()
	}

	if len(themes) == 0 {
		writeLine(os.Stdout, "No themes found\n")
		return nil
	}

	writeLine(os.Stdout, "Available themes:\n")
	writeLine(os.Stdout, "\n")
	for _, theme := range themes {
		tierLabel := ""
		if tierFilter == "" {
			tierLabel = fmt.Sprintf(" [%s]", theme.Tier)
		}
		writeLine(os.Stdout, "  %s%s\n", theme.ID, tierLabel)
		if theme.Name != "" && theme.Name != theme.ID {
			writeLine(os.Stdout, "    %s\n", theme.Name)
		}
		writeLine(os.Stdout, "    Source: %s\n", themeSource(theme))
		writeLine(os.Stdout, "\n")
	}

	return nil
}

// runThemesShow handles the `tau themes show` subcommand.
func runThemesShow(ctx context.Context, cmd *cli.Command) error {
	if !cmd.Args().Present() {
		return cli.Exit("usage: tau themes show <theme-id>", ExitUserError)
	}

	_, _, loader, _, err := buildRuntimeOptions(ctx, ui.NewNoOpUI(), nil, withoutPlugins())
	if err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	if loader == nil {
		return cli.Exit("No artifact loader configured", ExitSubsystemError)
	}

	registry := loader.ThemeRegistry()
	if registry == nil {
		return cli.Exit("No theme registry available", ExitSubsystemError)
	}

	id := cmd.Args().First()
	theme, ok := registry.Get(id)
	if !ok {
		if matches := registry.Matches(id); len(matches) > 1 {
			var ids []string
			for _, match := range matches {
				ids = append(ids, match.ID)
			}
			return cli.Exit(fmt.Sprintf("theme %q is ambiguous; matches: %s", id, strings.Join(ids, ", ")), ExitUserError)
		}
		return cli.Exit(fmt.Sprintf("theme %q not found", id), ExitUserError)
	}

	writeLine(os.Stdout, "Theme: %s\n", theme.ID)
	writeLine(os.Stdout, "Name: %s\n", theme.Name)
	writeLine(os.Stdout, "Tier: %s\n", theme.Tier)
	writeLine(os.Stdout, "Source: %s\n", themeSource(theme))

	// Print extra fields if any
	if len(theme.ExtraFields) > 0 {
		writeLine(os.Stdout, "\n")
		writeLine(os.Stdout, "Extra fields:\n")
		for k, v := range theme.ExtraFields {
			writeLine(os.Stdout, "  %s: %v\n", k, v)
		}
	}

	// Print raw JSON
	if len(theme.Raw) > 0 {
		writeLine(os.Stdout, "\n")
		writeLine(os.Stdout, "Raw:\n")
		writeLine(os.Stdout, "%s\n", string(theme.Raw))
	}

	return nil
}

// runPluginState shows plugin state for a named plugin.
func runPluginState(ctx context.Context, cmd *cli.Command) error {
	if !cmd.Args().Present() {
		return cli.Exit("usage: tau plugin state <plugin>", ExitUserError)
	}
	name := cmd.Args().First()

	// Get the plugin state directory
	stateDir := filepath.Join(configDir(), "plugin-state")
	store, err := state.NewStore(stateDir, name)
	if err != nil {
		return cli.Exit(fmt.Sprintf("get plugin state: %v", err), ExitSubsystemError)
	}

	kvEntries, err := store.KVEntries()
	if err != nil {
		return cli.Exit(fmt.Sprintf("get kv entries: %v", err), ExitSubsystemError)
	}

	logEntries := store.Log()

	writeLine(os.Stdout, "Plugin: %s\n", name)
	writeLine(os.Stdout, "State:\n")
	if len(kvEntries) == 0 {
		writeLine(os.Stdout, "  (no state)\n")
	} else {
		for _, entry := range kvEntries {
			writeLine(os.Stdout, "  %s = %s\n", entry.Key, entry.Value)
		}
	}
	if len(logEntries) > 0 {
		writeLine(os.Stdout, "Log:\n")
		for _, entry := range logEntries {
			writeLine(os.Stdout, "  %s: %s=%s (%s)\n", entry.ID, entry.Key, entry.Value, entry.Timestamp)
		}
	}
	return nil
}

// runPluginDoctor validates a plugin binary at the given path.
func runPluginDoctor(ctx context.Context, cmd *cli.Command) error {
	if !cmd.Args().Present() {
		return cli.Exit("usage: tau plugin doctor <path>", ExitUserError)
	}
	path := cmd.Args().First()

	// Check if file exists
	info, err := os.Stat(path)
	if err != nil {
		return cli.Exit(fmt.Sprintf("stat plugin: %v", err), ExitSubsystemError)
	}
	if info.IsDir() {
		return cli.Exit("path must be a file, not a directory", ExitUserError)
	}

	// Compute SHA256
	data, err := os.ReadFile(path)
	if err != nil {
		return cli.Exit(fmt.Sprintf("read plugin: %v", err), ExitSubsystemError)
	}

	hash := sha256.Sum256(data)

	writeLine(os.Stdout, "Plugin Doctor Report\n")
	writeLine(os.Stdout, "Path: %s\n", path)
	writeLine(os.Stdout, "Size: %d bytes\n", len(data))
	writeLine(os.Stdout, "SHA256: %s\n", hex.EncodeToString(hash[:]))
	writeLine(os.Stdout, "Executable: %v\n", info.Mode().IsRegular())

	// Load tau-package.toml manifest from the plugin's directory.
	manifestPath := filepath.Join(filepath.Dir(path), "tau-package.toml")
	_, err = os.ReadFile(manifestPath)
	if err != nil {
		writeLine(os.Stdout, "Manifest: not found (optional for loose plugin binaries)\n")
	} else {
		writeLine(os.Stdout, "Manifest: found at %s\n", manifestPath)
		// Parse manifest to validate requirements.
		manifest, err := packages.Load(manifestPath)
		if err != nil {
			writeLine(os.Stdout, "Manifest: parse error: %v\n", err)
		} else {
			// Validate runtime requirements.
			writeLine(os.Stdout, "\n--- Requirements ---\n")
			if manifest.Reqs.Runtime == "" {
				writeLine(os.Stdout, "Runtime: none required\n")
			} else {
				writeLine(os.Stdout, "Required: %s\n", manifest.Reqs.Runtime)
				if err := packages.CheckRuntimeRequirementContext(ctx, manifest.Reqs.Runtime); err != nil {
					writeLine(os.Stdout, "Status: FAIL\n")
					writeLine(os.Stdout, "Error: %v\n", err)
				} else {
					writeLine(os.Stdout, "Status: PASS\n")
				}
			}

			// Validate plugin binary identity (SHA256).
			writeLine(os.Stdout, "\n--- Binary Identity ---\n")
			pluginSHA := manifest.Plugin.SHA256
			if pluginSHA == "" {
				writeLine(os.Stdout, "Expected SHA256: not specified in manifest\n")
			} else {
				writeLine(os.Stdout, "Expected SHA256: %s\n", pluginSHA)
				computed := hex.EncodeToString(hash[:])
				if strings.EqualFold(computed, pluginSHA) {
					writeLine(os.Stdout, "Status: PASS\n")
				} else {
					writeLine(os.Stdout, "Status: FAIL\n")
					writeLine(os.Stdout, "Computed SHA256: %s\n", computed)
					writeLine(os.Stdout, "Error: binary hash does not match expected value\n")
				}
			}
		}
	}

	// Create a minimal plugin manager for handshake.
	// We need a UI and command registry even though doctor mode doesn't use them.
	ui := &noopUI{}
	registry := commands.NewInMemoryRegistry()
	configDir := configDir()
	manager := plugins.NewManager(configDir, ui, registry)

	writeLine(os.Stdout, "\n--- Handshake ---\n")

	// Run the plugin handshake.
	result, err := manager.Handshake(ctx, path)
	if err != nil {
		writeLine(os.Stdout, "Handshake: FAILED\n")
		writeLine(os.Stdout, "Error: %v\n", err)
		return nil
	}
	defer func() { reportShutdownError(result.Close()) }()

	writeLine(os.Stdout, "Handshake: OK\n")
	writeLine(os.Stdout, "Name: %s\n", result.Name)
	writeLine(os.Stdout, "API Version: %s\n", result.APIVersion)

	// Validate plugin API version.
	writeLine(os.Stdout, "\n--- API Version ---\n")
	supported := false
	for _, v := range plugins.SupportedAPIVersions {
		if v == result.APIVersion {
			supported = true
			break
		}
	}
	if supported {
		writeLine(os.Stdout, "Status: PASS (%s is supported)\n", result.APIVersion)
	} else {
		writeLine(os.Stdout, "Status: FAIL\n")
		writeLine(os.Stdout, "Error: unsupported api version %q (tau supports: %v)\n", result.APIVersion, plugins.SupportedAPIVersions)
	}

	// Print registrations.
	regs := result.Registrations
	if len(regs.Tools) > 0 {
		writeLine(os.Stdout, "\nTools (%d):\n", len(regs.Tools))
		for _, t := range regs.Tools {
			desc := t.Description
			if desc == "" {
				desc = "(no description)"
			}
			writeLine(os.Stdout, "  - %s: %s\n", t.Name, desc)
		}
	} else {
		writeLine(os.Stdout, "\nTools: none\n")
	}

	if len(regs.Commands) > 0 {
		writeLine(os.Stdout, "\nCommands (%d):\n", len(regs.Commands))
		for _, c := range regs.Commands {
			desc := c.Description
			if desc == "" {
				desc = "(no description)"
			}
			writeLine(os.Stdout, "  - %s: %s\n", c.Name, desc)
		}
	} else {
		writeLine(os.Stdout, "\nCommands: none\n")
	}

	if len(regs.EventSubscriptions) > 0 {
		writeLine(os.Stdout, "\nEvent Subscriptions (%d):\n", len(regs.EventSubscriptions))
		for _, e := range regs.EventSubscriptions {
			writeLine(os.Stdout, "  - %s\n", e)
		}
	} else {
		writeLine(os.Stdout, "\nEvent Subscriptions: none\n")
	}

	if len(regs.CLIFlags) > 0 {
		writeLine(os.Stdout, "\nCLI Flags (%d):\n", len(regs.CLIFlags))
		for _, f := range regs.CLIFlags {
			required := ""
			if f.Required {
				required = " (required)"
			}
			writeLine(os.Stdout, "  - --%s%s: %s\n", f.Name, required, f.Description)
		}
	} else {
		writeLine(os.Stdout, "\nCLI Flags: none\n")
	}

	if regs.ToolPrefix != "" {
		writeLine(os.Stdout, "\nTool Prefix: %s\n", regs.ToolPrefix)
	}

	return nil
}

// noopUI is a no-op UI implementation for plugin doctor mode.
type noopUI struct{}

func (n *noopUI) Confirm(ctx context.Context, title, message string) (bool, error) { return false, nil }

func (n *noopUI) Select(ctx context.Context, title string, options []string) (int, error) {
	return 0, nil
}

func (n *noopUI) Input(ctx context.Context, title, placeholder string) (string, error) {
	return "", nil
}
func (n *noopUI) Notify(level ui.NotifyLevel, message string) {}
func (n *noopUI) SetStatus(key, message string)               {}
func (n *noopUI) SetWidget(key string, lines []string)        {}
func (n *noopUI) SetTitle(title string)                       {}
func (n *noopUI) SetEditorText(text string)                   {}
func (n *noopUI) SetToolsExpanded(expanded bool)              {}

// --- Root Command Support Functions ---

// runListModels lists available built-in and custom models with optional fuzzy search.
func runListModels(ctx context.Context, search string) int {
	_ = ctx

	type modelEntry struct {
		provider string
		model    string
	}

	seen := make(map[string]struct{})
	models := make([]modelEntry, 0)
	appendModel := func(provider, model string) {
		if provider == "" || model == "" {
			return
		}
		key := provider + "/" + model
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		models = append(models, modelEntry{provider: provider, model: model})
	}

	for _, model := range ai.ModelsList() {
		if model == nil {
			continue
		}
		appendModel(model.Provider, model.ID)
	}

	// Surface custom models from models.json when present.
	if reg, err := ai.LoadModelRegistry(filepath.Join(configDir(), "models.json")); err == nil {
		for _, providerID := range reg.ProviderIDs() {
			p, _ := reg.Provider(providerID)
			for _, m := range p.Models {
				appendModel(providerID, m.ID)
			}
		}
	}

	if search != "" {
		searchLower := strings.ToLower(search)
		filtered := make([]modelEntry, 0, len(models))
		for _, m := range models {
			full := strings.ToLower(m.provider + "/" + m.model)
			if strings.Contains(full, searchLower) {
				filtered = append(filtered, m)
			}
		}
		models = filtered
	}

	if len(models) == 0 {
		fmt.Fprintln(os.Stderr, "No models found")
		if search != "" {
			fmt.Fprintf(os.Stderr, "(searched for: %s)\n", search)
		}
		return ExitSuccess
	}

	slices.SortFunc(models, func(a, b modelEntry) int {
		if a.provider != b.provider {
			if a.provider < b.provider {
				return -1
			}
			return 1
		}
		if a.model < b.model {
			return -1
		}
		if a.model > b.model {
			return 1
		}
		return 0
	})

	for _, m := range models {
		writeLine(os.Stdout, "%s/%s\n", m.provider, m.model)
	}

	return ExitSuccess
}

// runConversationWithSignal runs single-shot conversation mode with signal handling.
func runConversationWithSignal(ctx context.Context, sigChan <-chan os.Signal, prompts []string, fileImages []ai.ContentBlock) int {
	if len(prompts) == 0 {
		fmt.Fprintln(os.Stderr, usageText)
		return ExitUserError
	}

	// Set up signal handling
	sigChanLocal := make(chan os.Signal, 1)
	signal.Notify(sigChanLocal, syscall.SIGINT)

	defer signal.Stop(sigChanLocal)

	// Set up runtime
	uiImpl := choosePrintModeUI()

	rtOpts, pluginMgr, loader, reg, sessionFlags, err := buildConversationRuntimeOptions(ctx, uiImpl)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return ExitUserError
	}
	GlobalCommandRegistry = reg
	GlobalPluginManager = pluginMgr
	_ = pluginMgr
	_ = loader
	if sessionFlags.SelectionError != nil {
		fmt.Fprintf(os.Stderr, "%v\n", sessionFlags.SelectionError)
		return ExitUserError
	}

	rt, err := sdk.NewRuntime(rtOpts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create runtime: %v\n", err)
		return ExitSubsystemError
	}
	defer func() { reportShutdownError(rt.Close(ctx)) }()

	// Set up signal handling for graceful shutdown
	interruptChan := make(chan struct{}, 1)
	var interrupted atomic.Bool
	doubleSigintWindow := 2 * time.Second
	lastSIGINT := time.Time{}

	go func() {
		for sig := range sigChanLocal {
			if sig == syscall.SIGINT {
				now := time.Now()
				if interrupted.Load() && now.Sub(lastSIGINT) < doubleSigintWindow {
					interruptChan <- struct{}{}
					return
				}
				interrupted.Store(true)
				lastSIGINT = now
				interruptChan <- struct{}{}
				fmt.Fprintln(os.Stderr, "\ninterrupted; ctrl-c again to exit")
			}
		}
	}()

	// Create a session
	sessionOpts := sdk.SessionOptions{
		Model:               rtOpts.ExplicitModel,
		CWD:                 getCWD(),
		UI:                  uiImpl,
		NoSession:           sessionFlags.NoSession,
		EphemeralExportPath: sessionFlags.EphemeralExportPath,
		DisplayName:         sessionFlags.DisplayName,
		SystemPrompt:        sessionFlags.SystemPrompt,
		AppendSystemPrompts: sessionFlags.AppendSystemPrompts,
		ToolAllowlist:       sessionFlags.ToolAllowlist,
		ToolDenylist:        sessionFlags.ToolDenylist,
		ApproveProjectFiles: sessionFlags.ApproveProjectFiles,
	}

	// Apply session selection flags to session options
	if sessionFlags.ContinueSessionID != "" {
		sessionOpts.SessionID = sessionFlags.ContinueSessionID
	} else if sessionFlags.ResumeSessionID != "" {
		sessionOpts.SessionID = sessionFlags.ResumeSessionID
	}
	if sessionFlags.ForkSourceID != "" {
		sessionOpts.ForkFrom = sessionFlags.ForkSourceID
	}

	sess, err := rt.NewSession(ctx, sessionOpts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create session: %v\n", err)
		return classifyConversationError(err)
	}
	defer func() { reportShutdownError(sess.Close(ctx)) }()

	var printMu sync.Mutex
	printer := &eventPrinter{NoStream: false}
	if rootCmd := rootCmdFromContext(ctx); rootCmd != nil {
		printer.NoStream = rootCmd.Bool("no-stream")
	}
	for i, prompt := range prompts {
		msg, err := buildCLIMessage(ctx, sess, prompt, i == 0, fileImages)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return ExitUserError
		}

		events, err := sess.Send(ctx, msg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "send message: %v\n", err)
			return classifyConversationError(err)
		}

		for {
			select {
			case <-interruptChan:
				_ = sess.Interrupt()
				return ExitSIGINT
			case <-ctx.Done():
				return ExitSIGINT
			case event, ok := <-events:
				if !ok {
					goto nextPrompt
				}
				processEventWith(&event, &printMu, printer)
			}
		}
	nextPrompt:
		if printer.Err != nil {
			fmt.Fprintln(os.Stderr, printer.Err)
			return ExitProviderError
		}
	}

	if sessionFlags.ExportPath != "" {
		if sessionFlags.NoSession && sessionFlags.EphemeralExportPath != "" {
			if err := doExport(sess.ID(), sessionFlags.EphemeralExportPath, sessionFlags.ExportPath, ""); err != nil {
				fmt.Fprintf(os.Stderr, "export failed: %v\n", err)
				return ExitSubsystemError
			}
		} else {
			sessionID := sess.ID()
			if sessionID == "" {
				fmt.Fprintf(os.Stderr, "--export: no session to export\n")
				return ExitSubsystemError
			}
			if err := doExport(sessionID, rtOpts.SessionsDir, sessionFlags.ExportPath, ""); err != nil {
				fmt.Fprintf(os.Stderr, "export failed: %v\n", err)
				return ExitSubsystemError
			}
		}
	}

	if err := printer.result(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return ExitProviderError
	}
	return ExitSuccess
}

// runInteractive launches the interactive terminal UI. It reuses
// the same runtime/session wiring as conversation mode, then drives the
// tui from agent-session events. An optional first prompt is sent on
// startup so `tau "do X"` on a TTY opens the TUI with the run streaming.
func runInteractive(ctx context.Context, cmd *cli.Command, prompts []string) (code int) {
	var exitMessage string
	defer func() {
		if exitMessage != "" {
			if err := printPostTUIExitMessage(os.Stdout, exitMessage); err != nil {
				code = ExitSubsystemError
			}
		}
	}()

	uiImpl := ui.NewNoOpUI()
	var buildOpts []runtimeBuildOption
	if isTTY() {
		buildOpts = append(buildOpts, withTrustPrompt(ui.NewStdinUI()))
	}

	rtOpts, pluginMgr, loader, reg, sessionFlags, err := buildConversationRuntimeOptions(ctx, uiImpl, buildOpts...)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return ExitUserError
	}
	GlobalCommandRegistry = reg
	GlobalPluginManager = pluginMgr
	if sessionFlags.SelectionError != nil {
		fmt.Fprintf(os.Stderr, "%v\n", sessionFlags.SelectionError)
		return ExitUserError
	}

	rt, err := sdk.NewRuntime(rtOpts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create runtime: %v\n", err)
		return ExitSubsystemError
	}
	defer func() { reportShutdownError(rt.Close(ctx)) }()

	if resumeSelectionRequested(cmd) {
		sessions, err := rt.ListSessions(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "list sessions: %v\n", err)
			return ExitSubsystemError
		}
		selectedID := ""
		if len(sessions) > 0 {
			selectedID, err = tui.SelectSession(ctx, sessions)
			if err != nil {
				fmt.Fprintf(os.Stderr, "select session: %v\n", err)
				return ExitSubsystemError
			}
		}
		if selectedID == "" {
			exitMessage = "No session selected"
			return ExitUserError
		}
		rtOpts.ResumeSessionID = selectedID
		sessionFlags.ResumeSessionID = selectedID
	}

	sessOpts := sdk.SessionOptions{
		Model:               rtOpts.ExplicitModel,
		CWD:                 getCWD(),
		UI:                  uiImpl,
		NoSession:           sessionFlags.NoSession,
		EphemeralExportPath: sessionFlags.EphemeralExportPath,
		DisplayName:         sessionFlags.DisplayName,
		SystemPrompt:        sessionFlags.SystemPrompt,
		AppendSystemPrompts: sessionFlags.AppendSystemPrompts,
		ToolAllowlist:       sessionFlags.ToolAllowlist,
		ToolDenylist:        sessionFlags.ToolDenylist,
		ApproveProjectFiles: sessionFlags.ApproveProjectFiles,
	}
	if sessionFlags.ContinueSessionID != "" {
		sessOpts.SessionID = sessionFlags.ContinueSessionID
	} else if sessionFlags.ResumeSessionID != "" {
		sessOpts.SessionID = sessionFlags.ResumeSessionID
	}
	if sessionFlags.ForkSourceID != "" {
		sessOpts.ForkFrom = sessionFlags.ForkSourceID
	}

	sess, err := rt.NewSession(ctx, sessOpts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create session: %v\n", err)
		return classifyConversationError(err)
	}
	defer func() { reportShutdownError(sess.Close(ctx)) }()

	initialPrompt := ""
	if len(prompts) > 0 {
		initialPrompt = prompts[0]
	}

	app, err := tui.New(tui.Options{
		Session:         sess,
		Version:         pluginsVersion(),
		CWD:             getCWD(),
		ModelSpec:       sess.State().Model(),
		InitialPrompt:   initialPrompt,
		CredentialStore: rtOpts.CredentialStore,
		SettingsPath:    filepath.Join(configDir(), "settings.json"),
		ModelScope:      modelScopePatterns(sessionFlags.Models, rtOpts.Settings),
		ThemeLookup:     themeLookup(loader),
		ThemeNames:      themeNames(loader),
		RefreshTheme:    themeRefresher(loader),
		StartupMessages: diagnosticsFromContext(ctx),
		SessionFactory: func(ctx context.Context, opts sdk.SessionOptions) (sdk.Session, error) {
			if opts.CWD == "" {
				opts.CWD = getCWD()
			}
			opts.UI = uiImpl
			opts.Model = rtOpts.ExplicitModel
			opts.NoSession = sessionFlags.NoSession
			opts.EphemeralExportPath = sessionFlags.EphemeralExportPath
			opts.SystemPrompt = sessionFlags.SystemPrompt
			opts.AppendSystemPrompts = sessionFlags.AppendSystemPrompts
			opts.ToolAllowlist = sessionFlags.ToolAllowlist
			opts.ToolDenylist = sessionFlags.ToolDenylist
			opts.ApproveProjectFiles = sessionFlags.ApproveProjectFiles
			return rt.NewSession(ctx, opts)
		},
		ListSessions: rt.ListSessions,
		RenameSession: func(ctx context.Context, id, name string) error {
			storage := rt.GetSessionStorage()
			if storage == nil {
				return fmt.Errorf("session storage unavailable")
			}
			return storage.Rename(id, name)
		},
		PinSession:   rt.PinSession,
		UnpinSession: rt.UnpinSession,
		DeleteSession: func(ctx context.Context, id string) error {
			storage := rt.GetSessionStorage()
			if storage == nil {
				return fmt.Errorf("session storage unavailable")
			}
			return storage.Delete(id)
		},
		ImportSession: func(ctx context.Context, src, cwd string) (string, error) {
			storage := rt.GetSessionStorage()
			if storage == nil {
				return "", fmt.Errorf("session storage unavailable")
			}
			return storage.Import(src, cwd)
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "build tui: %v\n", err)
		return ExitSubsystemError
	}
	if pluginMgr != nil {
		pluginMgr.SetUI(app)
		pluginMgr.SetMessageSender(app.SendPluginUserMessage)
		app.WirePluginTerminalInput(pluginMgr)
	}

	if err := app.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "tui error: %v\n", err)
		return ExitSubsystemError
	}
	if err := printResumeInstructionsIfWritten(os.Stdout, rtOpts.SessionsDir, app.SessionID()); err != nil {
		fmt.Fprintf(os.Stderr, "resume instructions: %v\n", err)
		return ExitSubsystemError
	}
	return ExitSuccess
}

const sessionSelectorExitMessageRow = 25

func printPostTUIExitMessage(w io.Writer, message string) error {
	if message == "" {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\x1b[%d;1H\x1b[0m\x1b[39m\x1b[49m\x1b[J%s\n", sessionSelectorExitMessageRow, message); err != nil {
		return fmt.Errorf("write exit message: %w", err)
	}
	return nil
}

func printResumeInstructions(w io.Writer, sessionID string) {
	if sessionID == "" {
		return
	}
	writeLine(w, "\nSession ID: %s\n", sessionID)
	writeLine(w, "Resume this session with: tau --resume %s\n", sessionID)
}

func printResumeInstructionsIfWritten(w io.Writer, sessionDir, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	storage, err := session.NewStorageAt(sessionDir)
	if err != nil {
		return fmt.Errorf("open session storage: %w", err)
	}
	if _, err := storage.Open(sessionID); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil
		}
		return fmt.Errorf("open session %q: %w", sessionID, err)
	}
	printResumeInstructions(w, sessionID)
	return nil
}

// doExport exports a session to a file with optional format override.
// If sessionDir is non-empty, it uses that directory for session storage.
func doExport(sessionID, sessionDir, outputPath, formatOverride string) error {
	ctx := context.Background()

	// Use the provided sessionDir, or fall back to default
	sessDir := sessionDir
	sessCLI, err := session.NewCLI(sessDir)
	if err != nil {
		return fmt.Errorf("create session CLI: %w", err)
	}

	format := formatOverride
	if format == "" {
		format = "md" // default to markdown
	}

	opts := session.ExportOptions{
		Format: format,
		Output: outputPath,
	}

	if err := sessCLI.Export(ctx, sessionID, opts); err != nil {
		return fmt.Errorf("export session: %w", err)
	}

	return nil
}

func classifyConversationError(err error) int {
	switch {
	case errors.Is(err, sdk.ErrSessionNotFound):
		return ExitUserError
	case errors.Is(err, sdk.ErrVisionNotSupported):
		return ExitUserError
	default:
		return ExitSubsystemError
	}
}

// rootCmdFromContext extracts the root command from context metadata.
func rootCmdFromContext(ctx context.Context) *cli.Command {
	if cmd, ok := ctx.Value(rootCmdKey{}).(*cli.Command); ok {
		return cmd
	}
	return nil
}

// modelScopePatterns returns the --models patterns, falling back to the
// enabledModels setting.
func modelScopePatterns(flags []string, settings *ai.Settings) []string {
	if len(flags) > 0 || settings == nil {
		return flags
	}
	return settings.EnabledModels
}

// SessionFlags holds parsed conversation flags for session creation.
type SessionFlags struct {
	NoTools             bool
	NoBuiltinTools      bool
	NoContextFiles      bool
	ToolAllowlist       []string
	ToolDenylist        []string
	ThinkingLevel       string
	Models              []string
	Verbose             bool
	Offline             bool
	NoSession           bool
	ContinueSessionID   string
	ResumeSessionID     string
	ForkSourceID        string
	ExportPath          string
	EphemeralExportPath string
	DisplayName         string
	SystemPrompt        string
	AppendSystemPrompts []string
	ApproveProjectFiles bool
	SelectionError      error
}

// applyConversationFlags applies CLI flags to runtime options and returns session flags.
// Precedence rules for conflicting session flags:
//   - --no-session is highest priority; if set, all other session flags are ignored
//   - --session has highest priority among session-targeting flags
//   - --continue and --resume are mutually exclusive; last one wins with a warning
//   - --fork can be combined with --session, --continue, or --resume (creates fork of target)
func applyConversationFlags(rtOpts *sdk.RuntimeOptions, cmd *cli.Command) SessionFlags {
	sessionFlags := SessionFlags{}
	if cmd.String("provider") != "" && len(cmd.StringSlice("models")) == 0 {
		sessionFlags.SelectionError = fmt.Errorf("--provider requires --model (or --models)")
		return sessionFlags
	}

	// Collect session flag values first to enforce precedence rules
	noSession := cmd.Bool("no-session")
	sessionID := cmd.String("session")
	sessionIDExact := cmd.String("session-id")
	continueFlag := cmd.Bool("continue")
	resumeID := cmd.String("resume")
	resumeSet := cmd.IsSet("resume")
	forkID := cmd.String("fork")

	// Count how many session-targeting flags are set
	sessionFlagCount := 0
	if sessionID != "" {
		sessionFlagCount++
	}
	if sessionIDExact != "" {
		sessionFlagCount++
	}
	if continueFlag {
		sessionFlagCount++
	}
	if resumeSet {
		sessionFlagCount++
	}

	// --session-id wins over --session when both are set (pi behavior).
	if sessionIDExact != "" {
		sessionID = sessionIDExact
	}

	// Enforce --no-session highest priority
	if noSession {
		rtOpts.NoSession = true
		sessionFlags.NoSession = true
		sessionID = ""
		continueFlag = false
		resumeID = ""
		resumeSet = false
		forkID = ""
		sessionFlagCount = 0
	}

	// Enforce mutual exclusivity for --continue, --resume, --session
	if sessionFlagCount > 1 {
		// Determine which takes priority based on specificity
		if sessionID != "" {
			// --session is most specific, warn about others
			if continueFlag {
				fmt.Fprintln(os.Stderr, "warning: --continue ignored when --session is specified")
			}
			if resumeSet {
				fmt.Fprintln(os.Stderr, "warning: --resume ignored when --session is specified")
			}
		} else if continueFlag && resumeSet {
			// --continue and --resume both set, last wins (continue in this case)
			fmt.Fprintln(os.Stderr, "warning: --continue and --resume both specified; --continue takes precedence")
		}
		// If only --continue and --session, sessionID already handled above
	}

	// Handle extension paths (consumed directly by the plugin manager; the SDK
	// no longer carries a runtime-side mirror).
	extPaths := cmd.StringSlice("extension")
	if len(extPaths) > 0 {
		_ = extPaths // see pluginManager.AddExtensionPath below
	}

	// Handle thinking level
	thinking := cmd.String("thinking")
	if thinking != "" {
		rtOpts.ThinkingLevel = thinking
		sessionFlags.ThinkingLevel = thinking
	}

	// Handle model patterns. The root CLI resolves these into an explicit model
	// choice before session creation so one-shot mode has defined live behavior.
	models := cmd.StringSlice("models")
	if len(models) > 0 {
		rtOpts.Models = models
		sessionFlags.Models = models
		scoped, err := ai.ResolveModelPatterns(models, rtOpts.Settings)
		if err != nil {
			sessionFlags.SelectionError = fmt.Errorf("resolve --models: %w", err)
		} else {
			rtOpts.ExplicitModel = scoped.Model.String()
			if rtOpts.ThinkingLevel == "" && scoped.ThinkingLevel != "" {
				rtOpts.ThinkingLevel = scoped.ThinkingLevel
				sessionFlags.ThinkingLevel = scoped.ThinkingLevel
			}
		}
	}

	// Handle verbose/offline
	if cmd.Bool("verbose") {
		rtOpts.Verbose = true
		sessionFlags.Verbose = true
	}
	if cmd.Bool("offline") {
		rtOpts.Offline = true
		sessionFlags.Offline = true
	}

	// Handle tool control flags: --no-tools and --no-builtin-tools
	// These are wired into RuntimeOptions so the SDK can honor them on the live path.
	if cmd.Bool("no-tools") {
		rtOpts.NoTools = true
		sessionFlags.NoTools = true
	}
	if cmd.Bool("no-builtin-tools") {
		rtOpts.NoBuiltinTools = true
		sessionFlags.NoBuiltinTools = true
	}
	// Positive allowlist / negative denylist (pi-compatible). Normalize
	// comma-separated input to a flat list so callers can pass either form.
	if v := normalizeToolList(cmd.StringSlice("tools")); len(v) > 0 {
		sessionFlags.ToolAllowlist = v
		if err := sdk.ValidateToolSelection(v); err != nil {
			sessionFlags.SelectionError = fmt.Errorf("invalid --tools: %w", err)
		}
	}
	if v := normalizeToolList(cmd.StringSlice("exclude-tools")); len(v) > 0 {
		sessionFlags.ToolDenylist = v
	}

	// Handle artifact discovery control flags.
	// --no-extensions: honored directly at the plugin manager above.
	// --no-skills / --no-prompt-templates / --no-themes: honored via the
	// artifact loader config at startup (DisableSkills/DisablePrompts/
	// DisableThemes on the loader.Walk config). They don't need to flow
	// through RuntimeOptions or SessionFlags.
	if cmd.Bool("no-context-files") {
		rtOpts.NoContextFiles = true
		sessionFlags.NoContextFiles = true
	}

	// Handle --ignore-env
	if cmd.Bool("ignore-env") {
		rtOpts.IgnoreEnv = true
	}

	// Handle --provider / --api-key (pi-compatible auth overrides).
	if v := cmd.String("provider"); v != "" {
		rtOpts.Provider = v
	}
	if v := cmd.String("api-key"); v != "" {
		rtOpts.APIKey = v
	}

	// Handle --name/-n (session display name), --system-prompt,
	// --append-system-prompt (repeatable), and --approve/--no-approve.
	if v := cmd.String("name"); v != "" {
		sessionFlags.DisplayName = v
	}
	if v := cmd.String("system-prompt"); v != "" {
		sessionFlags.SystemPrompt = v
	}
	if v := cmd.StringSlice("append-system-prompt"); len(v) > 0 {
		sessionFlags.AppendSystemPrompts = v
	}
	// --no-approve wins over --approve when both are set (more conservative).
	if cmd.Bool("no-approve") {
		sessionFlags.ApproveProjectFiles = false
	} else if cmd.Bool("approve") {
		sessionFlags.ApproveProjectFiles = true
	}

	// Handle session flags (with precedence already enforced)
	// --no-session clears session targets without discarding runtime flags.
	// --session, --continue, --resume precedence is enforced above

	// Handle --session-dir (override session storage location)
	if sessionDir := cmd.String("session-dir"); sessionDir != "" {
		rtOpts.SessionsDir = sessionDir
	}

	// Handle --session (highest priority among session-targeting flags)
	if sessionID != "" {
		rtOpts.ResumeSessionID = sessionID
		sessionFlags.ResumeSessionID = sessionID
	}

	// Handle --continue (find latest resumable session)
	// Only applies if --session was not specified
	if continueFlag && sessionID == "" {
		sessionID, err := findLatestResumableSession(rtOpts.SessionsDir)
		if err != nil {
			sessionFlags.SelectionError = fmt.Errorf("find resumable session: %w", err)
		} else if sessionID != "" {
			rtOpts.ContinueSessionID = sessionID
			sessionFlags.ContinueSessionID = sessionID
		} else {
			sessionFlags.SelectionError = fmt.Errorf("no resumable session found")
		}
	}

	// Handle --resume (string flag - resume a specific session by ID)
	// Only applies if neither --session nor --continue was specified
	if resumeID != "" && sessionID == "" && !continueFlag {
		rtOpts.ResumeSessionID = resumeID
		sessionFlags.ResumeSessionID = resumeID
	}

	// Handle --fork (can be combined with any session-targeting flag)
	if forkID != "" {
		rtOpts.ForkSourceID = forkID
		sessionFlags.ForkSourceID = forkID
	}

	// Handle export flag
	if exportPath := cmd.String("export"); exportPath != "" {
		sessionFlags.ExportPath = exportPath
		// If --no-session and --export both set, use ephemeral export mode
		// to create a temporary session file just for export purposes
		if noSession {
			absPath, err := filepath.Abs(exportPath)
			if err == nil {
				sessionFlags.EphemeralExportPath = filepath.Dir(absPath)
			}
		}
	}

	return sessionFlags
}

// eventPrinter holds CLI display state across events. When NoStream is true,
// text_delta events accumulate into Buf and print at message_end so the user
// sees a complete response in one block instead of streamed chunks.
type eventPrinter struct {
	Err       error
	NoStream  bool
	Buf       strings.Builder
	lastStop  string
	lastError string
}

var errResponseTruncated = errors.New("Response was truncated: the model reached its output token limit.") //nolint:staticcheck // upstream print-mode wording

// result reports a run failure, or failure of the final assistant reply, as
// upstream print mode does after its last prompt.
func (p *eventPrinter) result() error {
	if p.Err != nil {
		return p.Err
	}
	switch p.lastStop {
	case string(ai.StopReasonError), string(ai.StopReasonAborted):
		if p.lastError != "" {
			return errors.New(p.lastError)
		}
		return errors.New("Request " + p.lastStop)
	case string(ai.StopReasonLength):
		return errResponseTruncated
	}
	return nil
}

// processEventWith handles an SDK event and prints it appropriately.
// p may be nil for streaming-only callers; when set, NoStream buffers
// text_delta events until message_end.
func processEventWith(event *sdk.Event, mu *sync.Mutex, p *eventPrinter) {
	mu.Lock()
	defer mu.Unlock()

	switch event.Type {
	case "text_delta":
		if pl, ok := event.Payload.(map[string]any); ok {
			if delta, ok := pl["delta"].(string); ok {
				if p != nil && p.NoStream {
					p.Buf.WriteString(delta)
				} else {
					fmt.Print(delta)
				}
			}
		}
	case "message_end":
		if p != nil {
			if payload, ok := event.Payload.(map[string]any); ok && payload["role"] == "assistant" {
				p.lastStop, _ = payload["finish_reason"].(string)
				p.lastError, _ = payload["error_message"].(string)
			}
		}
		if p != nil && p.NoStream && p.Buf.Len() > 0 {
			fmt.Print(p.Buf.String())
			p.Buf.Reset()
		}
	case "thinking_delta":
		if p, ok := event.Payload.(map[string]any); ok {
			if delta, ok := p["delta"].(string); ok {
				fmt.Fprintf(os.Stderr, "[thinking: %s]\n", delta)
			}
		}
	case "tool_call":
		if p, ok := event.Payload.(map[string]any); ok {
			name, _ := p["name"].(string)
			args, _ := p["input"].(string)
			fmt.Fprintf(os.Stderr, "\n[tool: %s] %s\n", name, args)
		}
	case "tool_result":
		if p, ok := event.Payload.(map[string]any); ok {
			output, _ := p["output"].(string)
			isError, _ := p["is_error"].(bool)
			prefix := "[tool result]"
			if isError {
				prefix = "[tool error]"
			}
			fmt.Fprintf(os.Stderr, "%s %s\n", prefix, output)
		}
	case "command_result":
		if payload, ok := event.Payload.(map[string]any); ok {
			output, _ := payload["output"].(string)
			isError, _ := payload["is_error"].(bool)
			if isError {
				fmt.Fprintf(os.Stderr, "[command error] %s\n", output)
			} else if output != "" {
				fmt.Fprintf(os.Stderr, "%s\n", output)
			}
		}
	case "error":
		if payload, ok := event.Payload.(map[string]any); ok {
			msg, _ := payload["message"].(string)
			if p != nil {
				p.Err = errors.New("error: " + msg)
			} else {
				fmt.Fprintf(os.Stderr, "error: %s\n", msg)
			}
		}
	case "usage":
		if p, ok := event.Payload.(map[string]any); ok {
			if call, ok := p["this_call"].(map[string]any); ok {
				inputTokens := eventUsageTokenCount(call["input_tokens"])
				outputTokens := eventUsageTokenCount(call["output_tokens"])
				fmt.Fprintf(os.Stderr, "\n[tokens: in=%d out=%d]\n", inputTokens, outputTokens)
			}
		}
	}
}

func eventUsageTokenCount(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case int32:
		return int(n)
	case float64:
		return int(n)
	case float32:
		return int(n)
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return int(i)
		}
		f, err := n.Float64()
		if err == nil {
			return int(f)
		}
	}
	return 0
}

// runConversationJSONWithSignal runs conversation mode with JSON output.
func runConversationJSONWithSignal(ctx context.Context, sigChan <-chan os.Signal, prompts []string, fileImages []ai.ContentBlock) int {
	if len(prompts) == 0 {
		fmt.Fprintln(os.Stderr, usageText)
		return ExitUserError
	}

	// Set up signal handling
	sigChanLocal := make(chan os.Signal, 1)
	signal.Notify(sigChanLocal, syscall.SIGINT)
	defer signal.Stop(sigChanLocal)

	// Create JSON UI
	jsonUI := ui.NewJSONEventUI()
	jsonUI.ReadResponse(ctx)

	// Set up runtime using the same bootstrap path as text mode
	uiImpl := ui.NewNoOpUI()
	rtOpts, pluginMgr, loader, reg, sessionFlags, err := buildConversationRuntimeOptions(ctx, uiImpl)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return ExitUserError
	}
	GlobalCommandRegistry = reg
	GlobalPluginManager = pluginMgr
	rtOpts.UI = jsonUI
	rtOpts.PluginManager = pluginMgr
	if pluginMgr != nil {
		pluginMgr.SetUI(jsonUI)
	}
	_ = loader
	if sessionFlags.SelectionError != nil {
		fmt.Fprintf(os.Stderr, "%v\n", sessionFlags.SelectionError)
		return ExitUserError
	}

	rt, err := sdk.NewRuntime(rtOpts)
	if err != nil {
		return ExitSubsystemError
	}
	defer func() { reportShutdownError(rt.Close(ctx)) }()

	// Set up signal handling for graceful shutdown
	interruptChan := make(chan struct{}, 1)
	var interrupted atomic.Bool
	doubleSigintWindow := 2 * time.Second
	lastSIGINT := time.Time{}

	go func() {
		for sig := range sigChanLocal {
			if sig == syscall.SIGINT {
				now := time.Now()
				if interrupted.Load() && now.Sub(lastSIGINT) < doubleSigintWindow {
					interruptChan <- struct{}{}
					return
				}
				interrupted.Store(true)
				lastSIGINT = now
				interruptChan <- struct{}{}
			}
		}
	}()

	// Create a session
	sessionOpts := sdk.SessionOptions{
		Model:               rtOpts.ExplicitModel,
		CWD:                 getCWD(),
		UI:                  jsonUI,
		NoSession:           sessionFlags.NoSession,
		EphemeralExportPath: sessionFlags.EphemeralExportPath,
		DisplayName:         sessionFlags.DisplayName,
		SystemPrompt:        sessionFlags.SystemPrompt,
		AppendSystemPrompts: sessionFlags.AppendSystemPrompts,
		ToolAllowlist:       sessionFlags.ToolAllowlist,
		ToolDenylist:        sessionFlags.ToolDenylist,
		ApproveProjectFiles: sessionFlags.ApproveProjectFiles,
	}

	// Apply session selection flags to session options
	if sessionFlags.ContinueSessionID != "" {
		sessionOpts.SessionID = sessionFlags.ContinueSessionID
	} else if sessionFlags.ResumeSessionID != "" {
		sessionOpts.SessionID = sessionFlags.ResumeSessionID
	}
	if sessionFlags.ForkSourceID != "" {
		sessionOpts.ForkFrom = sessionFlags.ForkSourceID
	}

	sess, err := rt.NewSession(ctx, sessionOpts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create session: %v\n", err)
		return classifyConversationError(err)
	}
	defer func() { reportShutdownError(sess.Close(ctx)) }()
	defer func() { _ = jsonUI.Close() }()

	// Emit meta event for JSON mode
	printJSONMetaEvent()

	for i, prompt := range prompts {
		msg, err := buildCLIMessage(ctx, sess, prompt, i == 0, fileImages)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return ExitUserError
		}

		events, err := sess.Send(ctx, msg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "send message: %v\n", err)
			return classifyConversationError(err)
		}

		for {
			select {
			case <-interruptChan:
				_ = sess.Interrupt()
				return ExitSIGINT
			case <-ctx.Done():
				return ExitSIGINT
			case event, ok := <-events:
				if !ok {
					goto nextJSONPrompt
				}
				printJSONEvent(&event)
			}
		}
	nextJSONPrompt:
	}

	if sessionFlags.ExportPath != "" {
		if sessionFlags.NoSession && sessionFlags.EphemeralExportPath != "" {
			if err := doExport(sess.ID(), sessionFlags.EphemeralExportPath, sessionFlags.ExportPath, ""); err != nil {
				fmt.Fprintf(os.Stderr, "export failed: %v\n", err)
				return ExitSubsystemError
			}
		} else {
			sessionID := sess.ID()
			if sessionID == "" {
				fmt.Fprintf(os.Stderr, "--export: no session to export\n")
				return ExitSubsystemError
			}
			if err := doExport(sessionID, rtOpts.SessionsDir, sessionFlags.ExportPath, ""); err != nil {
				fmt.Fprintf(os.Stderr, "export failed: %v\n", err)
				return ExitSubsystemError
			}
		}
	}

	return ExitSuccess
}

// printJSONEvent prints an SDK event as JSON to stdout.
func printJSONEvent(event *sdk.Event) {
	data, err := json.Marshal(jsonEventEnvelope(*event))
	if err != nil {
		return
	}
	data = append(data, '\n')
	if err := stdoutBytes(data); err != nil {
		return
	}
}

// printJSONMetaEvent emits the required leading meta event for root --json mode.
func printJSONMetaEvent() {
	meta := map[string]any{
		"type":           "meta",
		"schema_version": 1,
		"tau_version":    "0.1.0",
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return
	}
	data = append(data, '\n')
	if err := stdoutBytes(data); err != nil {
		return
	}
}

func preprocessPluginCLIArgs(ctx context.Context, args []string) ([]string, map[string]map[string]string, error) {
	if !mayContainPluginCLIFlags(args) {
		return args, nil, nil
	}

	pluginFlagDefs, err := discoverPluginCLIFlags(ctx, args)
	if err != nil {
		return nil, nil, err
	}
	if len(pluginFlagDefs) == 0 {
		return args, nil, nil
	}

	knownFlags := knownCLIFlags(append(RootFlags, ConversationFlags...))
	cleaned := []string{args[0]}
	resolved := make(map[string]map[string]string)

	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			cleaned = append(cleaned, args[i:]...)
			break
		}
		if !strings.HasPrefix(arg, "--") {
			cleaned = append(cleaned, arg)
			continue
		}

		nameValue := strings.TrimPrefix(arg, "--")
		name := nameValue
		value := ""
		hasInline := false
		if before, after, found := strings.Cut(nameValue, "="); found {
			name = before
			value = after
			hasInline = true
		}
		if _, ok := knownFlags[name]; ok {
			cleaned = append(cleaned, arg)
			continue
		}

		pluginName, ok := pluginFlagDefs[name]
		if !ok {
			cleaned = append(cleaned, arg)
			continue
		}
		if !hasInline {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				value = args[i]
			} else {
				value = "true"
			}
		}
		if _, ok := resolved[pluginName]; !ok {
			resolved[pluginName] = make(map[string]string)
		}
		resolved[pluginName][name] = value
	}

	return cleaned, resolved, nil
}

func mayContainPluginCLIFlags(args []string) bool {
	for _, arg := range args[1:] {
		if arg == "--" {
			return false
		}
		if arg == "--help" || arg == "-h" || arg == "help" || arg == "--version" || arg == "-v" {
			return false
		}
	}
	if hasCommandToken(args[1:]) {
		return false
	}

	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return false
		}
		if arg == "--extension" || arg == "-e" || strings.HasPrefix(arg, "--extension=") {
			return true
		}
		if strings.HasPrefix(arg, "--") && !isKnownRootFlagToken(arg) {
			return true
		}
	}
	return false
}

func hasCommandToken(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		for _, cmd := range Commands {
			if arg == cmd.Name || slices.Contains(cmd.Aliases, arg) {
				return true
			}
		}
	}
	return false
}

func isKnownRootFlagToken(arg string) bool {
	nameValue := strings.TrimPrefix(arg, "--")
	name, _, _ := strings.Cut(nameValue, "=")
	knownFlags := knownCLIFlags(append(RootFlags, ConversationFlags...))
	_, ok := knownFlags[name]
	return ok
}

func discoverPluginCLIFlags(ctx context.Context, args []string) (owners map[string]string, err error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	var override *bool
	for _, arg := range args[1:] {
		if arg == "--" {
			break
		}
		if arg == "--no-approve" || arg == "-na" {
			override = new(false)
			break
		}
		if arg == "--approve" || arg == "-a" {
			override = new(true)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	mgr := plugins.NewManager(configDir(), ui.NewNoOpUI(), commands.NewInMemoryRegistry())
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), plugins.DefaultDrainTimeout)
		defer cancel()
		err = errors.Join(err, mgr.Shutdown(cleanupCtx))
		if err != nil {
			owners = nil
		}
	}()
	for _, extPath := range extractExtensionPaths(args) {
		mgr.AddExtensionPath(extPath)
	}
	pass := &pluginTrustPass{mgr: mgr}
	settings, err := loadTrustedSettingsCached(ctx, cwd, home, override, nil, pass.decide)
	if err != nil {
		return nil, err
	}
	if err := settings.ApplyHTTPProxySettings(); err != nil {
		return nil, err
	}
	if err := pass.finish(ctx, settings); err != nil {
		return nil, err
	}

	owners = make(map[string]string)
	for pluginName, flags := range mgr.RegisteredCLIFlags() {
		for _, flag := range flags {
			if flag.Name != "" {
				owners[flag.Name] = pluginName
			}
		}
	}
	return owners, nil
}

func extractExtensionPaths(args []string) []string {
	var paths []string
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--extension", "-e":
			if i+1 < len(args) {
				i++
				paths = append(paths, args[i])
			}
		default:
			if strings.HasPrefix(args[i], "--extension=") {
				paths = append(paths, strings.TrimPrefix(args[i], "--extension="))
			}
		}
	}
	return paths
}

func knownCLIFlags(flags []cli.Flag) map[string]struct{} {
	known := make(map[string]struct{})
	for _, flag := range flags {
		named, ok := flag.(interface{ Names() []string })
		if !ok {
			continue
		}
		for _, name := range named.Names() {
			known[name] = struct{}{}
		}
	}
	return known
}

func applyPluginCLIFlags(ctx context.Context, pluginMgr *plugins.Manager) {
	if pluginMgr == nil {
		return
	}
	raw := ctx.Value(pluginCLIFlagsKey{})
	if raw == nil {
		return
	}
	flags, ok := raw.(map[string]map[string]string)
	if !ok {
		return
	}
	for pluginName, values := range flags {
		pluginMgr.ResolveCLIFlags(pluginName, values)
	}
}

func jsonEventEnvelope(event sdk.Event) map[string]any {
	out := map[string]any{
		"type": event.Type,
	}
	if event.Type != "meta" {
		if event.SessionID != "" {
			out["session_id"] = event.SessionID
		}
		out["seq"] = event.Seq
		out["ts"] = event.Timestamp
	}
	if payload, ok := event.Payload.(map[string]any); ok {
		for k, v := range payload {
			if k == "type" {
				continue
			}
			out[k] = v
		}
	} else if event.Payload != nil {
		out["payload"] = event.Payload
	}
	return out
}

// getCWD returns the current working directory.
func getCWD() string {
	cwd, _ := os.Getwd()
	return cwd
}

// findLatestResumableSession finds the most recently created resumable session.
func findLatestResumableSession(sessionDir string) (string, error) {
	if sessionDir == "" {
		sessionDir = filepath.Join(configDir(), "sessions")
	}

	storage, err := session.NewStorageAt(sessionDir)
	if err != nil {
		return "", fmt.Errorf("open session storage: %w", err)
	}

	return storage.LatestResumableCWD(getCWD())
}

// processFileArguments processes @file CLI arguments into text content and image attachments.
// It scans the input text for @<path> references, expands them by reading file contents,
// and returns the processed text with file contents embedded and any images extracted.
//
// @<path> references resolve relative to the current working directory. Absolute
// paths (`@/etc/hosts`, `@~/notes`) and parent-relative paths (`@../sibling`)
// are honored — the prompt is a user-facing surface, the same scope rules as
// shell paths. A directory reference expands to a file list, never to the
// contents of every file in the tree: reading a moderately-sized project into
// the model's context window without explicit consent is exactly the bug the
// directory branch guards against.
func processFileArguments(text string) (string, []ai.ContentBlock, error) {
	var result strings.Builder
	var images []ai.ContentBlock

	// Scan for @file references using a simple state machine
	i := 0
	for i < len(text) {
		if text[i] == '@' && (i == 0 || ((text[i-1] == ' ' || text[i-1] == '\n') && text[i-1] != '\\')) {
			// Found @ at start or after whitespace - collect the file path
			i++
			start := i

			// Read until whitespace or end
			for i < len(text) && text[i] != ' ' && text[i] != '\n' && text[i] != '\t' {
				i++
			}
			filePath := text[start:i]

			if filePath == "" {
				// Empty @ reference, just output @
				result.WriteRune('@')
				continue
			}

			// Expand ~ to home directory
			if strings.HasPrefix(filePath, "~/") {
				home, err := os.UserHomeDir()
				if err == nil {
					filePath = filepath.Join(home, filePath[2:])
				}
			}

			resolved, err := filepath.Abs(filePath)
			if err != nil {
				return "", nil, fmt.Errorf("resolve %s: %w", filePath, err)
			}

			info, err := os.Stat(resolved)
			if err != nil {
				if os.IsNotExist(err) {
					result.WriteString("@" + filePath)
					continue
				}
				return "", nil, fmt.Errorf("@%s: %w", filePath, err)
			}

			if info.IsDir() {
				listing, err := processFileArgumentDirectory(resolved)
				if err != nil {
					return "", nil, err
				}
				result.WriteString(listing)
				continue
			}

			// Detect if it's an image based on file extension
			ext := strings.ToLower(filepath.Ext(resolved))
			isImage := ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".gif" || ext == ".webp"

			if isImage {
				images = append(images, ai.ContentBlock{Type: ai.ContentBlockImage, Name: resolved})

				// Add text reference to image
				fmt.Fprintf(&result, "<file name=\"%s\">[image content]</file>\n", resolved)
			} else {
				// Add as text file reference
				data, err := os.ReadFile(resolved)
				if err != nil {
					return "", nil, fmt.Errorf("read file %s: %w", resolved, err)
				}
				fmt.Fprintf(&result, "<file name=\"%s\">\n%s\n</file>\n", resolved, string(data))
			}
		} else {
			r, size := utf8.DecodeRuneInString(text[i:])
			if r == utf8.RuneError && size == 1 {
				result.WriteByte(text[i])
			} else {
				result.WriteRune(r)
			}
			i += size
		}
	}

	return result.String(), images, nil
}

// processFileArgumentDirectory walks a directory and emits a file list
// (one <file name=...> per regular file, no contents). Dotfile
// directories at any depth are skipped so dependency caches and
// `.git` don't drown the listing; only regular files are included
// (no symlinks to devices, FIFOs, etc).
func processFileArgumentDirectory(root string) (string, error) {
	return fmt.Sprintf("<directory name=\"%s\">\n</directory>", root), nil
}

// usageText is the help text for tau.
const usageText = `Usage: tau [options] [prompt]

One-shot conversation flags:
  --session <id>     Target a specific session
  --continue, -c     Continue the latest resumable session
  --resume, -r       Resume a specific session by ID
  --fork <id>        Fork from an existing session
  --no-session       Run without session persistence (ephemeral)
  --export <file>    Export session to file after execution

Prompt composition:
  <prompt>           Prompt text (positional argument)
  @<file|dir>        Read prompt text/image from file or list files in
                     directory (positional, may repeat; accepts absolute
                     paths and parent-relative paths)
  --message, -m      Additional message (positional may be repeated)
  --image, -i        Include image in conversation (may be repeated)

Runtime options:
  --config-dir <dir> Tau config directory (default ~/.tau/agent; env TAU_CONFIG_DIR)
  --provider         Provider override (requires --model or --models)
  --model, --models  Specify models to use
  --api-key          API key for the provider (overrides credential store)
  --system-prompt    Override the default system prompt
  --append-system-prompt  Append to the system prompt (may be repeated)
  --name, -n         Display name for the session
  --session-id       Target a specific session by exact ID (vs. path)
  --tools, -t        Comma-separated list of tools to enable
  --exclude-tools, -xt  Comma-separated list of tools to disable
  --approve, -a      Auto-approve file edits without confirmation
  --no-approve, -na  Reject all file edit confirmations
  --models, -M       Specify ordered model patterns for explicit model selection
  --thinking         Set thinking level (off, minimal, low, medium, high, xhigh)
  --no-stream        Disable streaming output
  --no-tools, --nt   Disable all tools
  --no-builtin-tools Disable builtin tools (grep, find, ls)
  --no-extensions     Disable extensions
  --no-skills        Disable skills
  --no-prompt-templates  Disable prompt templates
  --no-themes        Disable themes
  --no-context-files Disable automatic context files

Plugin/extension options:
  --extension, -e    Load extension from path (may be repeated)
  --session-dir       Override session directory

Output modes:
  --mode <text|json|rpc>  Run mode (default: interactive for TTY, text for non-TTY)
  --print, -p        Single-shot text output (no TTY needed)
  --json, -j         Alias for --mode json (legacy)
  --list-models      List available models [search]

Commands:
  login/logout/providers     Auth management
  models list/discover      Model management
  set-model/show-model      Default model configuration
  install/uninstall/update   Package management
  packages list/info/search Package inspection
  sessions list/show/resume/compact/export/share  Session management
  daemon                  Start Unix socket RPC daemon
  doctor                    Show local tau status
  reload                    Reload artifacts and plugins
  skills/prompts/themes     List available artifacts`

// themeLookup reads the active registry on each application so reloads replace
// both colors and appearance metadata.
// themeRefresher is nil without artifacts, which disables theme file watching.
func themeRefresher(loader *artifacts.Loader) func(string) (bool, error) {
	if loader == nil {
		return nil
	}
	return func(name string) (bool, error) {
		registry := loader.ThemeRegistry()
		if registry == nil {
			return false, nil
		}
		return registry.Refresh(name)
	}
}

func themeNames(loader *artifacts.Loader) func() []string {
	if loader == nil {
		return nil
	}
	return func() []string {
		registry := loader.ThemeRegistry()
		if registry == nil {
			return nil
		}
		themes := registry.List()
		names := make([]string, 0, len(themes))
		for _, theme := range themes {
			names = append(names, theme.Name)
		}
		return names
	}
}

func themeLookup(loader *artifacts.Loader) tui.ThemeLookup {
	if loader == nil {
		return nil
	}
	return func(name string) (tui.ThemeDefinition, bool) {
		registry := loader.ThemeRegistry()
		if registry == nil {
			return tui.ThemeDefinition{}, false
		}
		theme, ok := registry.Get(name)
		if !ok || theme == nil || len(theme.Raw) == 0 {
			return tui.ThemeDefinition{}, false
		}
		definition, err := tui.DecodeTheme(theme.Raw)
		if err != nil {
			// A malformed theme file is worth reporting, but not worth
			// refusing to start over: fall back to the built-in tokens.
			fmt.Fprintf(os.Stderr, "theme %q: %v\n", name, err)
			return tui.ThemeDefinition{}, false
		}
		return definition, true
	}
}

func reportShutdownError(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "tau shutdown error: %v\n", err)
	}
}
