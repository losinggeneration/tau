package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/losinggeneration/tau/agent/artifacts"
	"github.com/losinggeneration/tau/agent/packages"
	sessionpkg "github.com/losinggeneration/tau/agent/session"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/internal/app/tui"
	"github.com/losinggeneration/tau/rpc/client"
	"github.com/losinggeneration/tau/rpc/coord"
	"github.com/losinggeneration/tau/sdk"
	"github.com/urfave/cli/v3"
)

const coordinatorStartupTimeout = 2 * time.Second

func findInteractiveCoordinator(ctx context.Context, socket string) (*client.CoordinatedClient, error) {
	if err := checkInteractiveSocket(socket); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect Tau coordinator (use --standalone to bypass it): %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, coordinatorStartupTimeout)
	defer cancel()
	c, err := client.NewCoordinatedClient(ctx, socket)
	if err == nil {
		return c, nil
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return nil, nil
	}
	return nil, fmt.Errorf("connect to Tau coordinator at %s (use --standalone to bypass it): %w", socket, err)
}

func checkInteractiveSocket(path string) error {
	for _, name := range []string{path, filepath.Dir(path)} {
		info, err := os.Lstat(name)
		if err != nil {
			return err
		}
		mode := os.ModeSocket
		if name != path {
			mode = os.ModeDir
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if info.Mode().Type() != mode || info.Mode().Perm()&0o077 != 0 || !ok || stat.Uid != uint32(os.Getuid()) {
			return fmt.Errorf("%s is not a private user-owned %s", name, mode)
		}
	}
	return nil
}

func coordinatedStartupFlags(cmd *cli.Command) error {
	// Worker launch uses settings from the daemon, not the client's flags or
	// credentials. Don't silently ignore a tool, trust, or model override.
	for _, flag := range cmd.Flags {
		name := flag.Names()[0]
		switch name {
		case "help", "version", "standalone", "message", "image", "session", "session-id", "resume", "continue", "mode", "print", "json", "stdin":
			continue
		}
		if cmd.IsSet(name) {
			return fmt.Errorf("--%s applies to standalone Tau; use --standalone to apply it", name)
		}
	}
	return nil
}

func coordinatedStartupAction(cmd *cli.Command, hasPrompt bool) (action, arg string) {
	if id := cmp.Or(cmd.String("session-id"), cmd.String("session"), cmd.String("resume")); id != "" {
		return "resume", id
	}
	if cmd.Bool("continue") {
		return "continue", ""
	}
	if resumeSelectionRequested(cmd) {
		return "resume-picker", ""
	}
	if hasPrompt {
		return "new", ""
	}
	return "", ""
}

// Load presentation and storage settings without starting a runtime or loading plugins.
func coordinatedViewSettings(ctx context.Context, cwd string) (*ai.Settings, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	return loadTrustedSettingsCached(ctx, cwd, home, nil, nil, nil)
}

func coordinatedThemes(cwd string, settings *ai.Settings) (*artifacts.Loader, error) {
	userDir, projectDir := packageDirsFromSettings(settings)
	resolved, err := packages.ResolvePackageSources(settings, userDir, filepath.Join(cwd, projectDir))
	if err != nil {
		return nil, fmt.Errorf("resolve theme packages: %w", err)
	}
	return artifacts.NewLoader(artifacts.LoaderOptions{
		CWD: cwd, ProjectTrusted: new(settings.ProjectResourcesTrusted()), SettingsScopes: getSettingsArtifactScopes(settings),
		UserDirs: artifactUserDirs(settings), Packages: append([]packages.ResolvedPackage{}, resolved...),
		DisableSkills: true, DisablePrompts: true,
		ThemeValidator: func(raw []byte) error { _, err := tui.DecodeThemeColors(raw); return err },
	})
}

// savedSessionDir remembers where the attached project's sessions live so
// quitting never depends on the coordinator.
type savedSessionDir struct {
	project, path string
}

func savedTUISessionDir(ctx context.Context, c *client.CoordinatedClient, cwd string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, coordinatorStartupTimeout)
	defer cancel()
	dir, err := c.SessionDirectory(ctx, cwd)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(dir) {
		return "", errors.New("coordinator did not return an absolute session directory")
	}
	return dir, nil
}

// The session and the picker use separate links and may have reconnected
// independently, so a dead picker link falls back to a fresh connection.
func lookupSavedSessionDir(ctx context.Context, c *client.CoordinatedClient, socket, project string) (dir string, err error) {
	ctx, cancel := context.WithTimeout(ctx, coordinatorStartupTimeout)
	defer cancel()
	if c == nil || c.Err() != nil {
		c, err = findInteractiveCoordinator(ctx, socket)
		if err != nil {
			return "", err
		}
		if c == nil {
			return "", errors.New("coordinator is not listening")
		}
		defer func() { err = errors.Join(err, c.Close()) }()
	}
	return savedTUISessionDir(ctx, c, project)
}

func savedTUISessions(ctx context.Context, c *client.CoordinatedClient, cwd string) (*sessionpkg.Storage, error) {
	dir, err := savedTUISessionDir(ctx, c, cwd)
	if err != nil {
		return nil, err
	}
	return sessionpkg.NewStorageAt(dir)
}

func listSavedTUISessions(ctx context.Context, c *client.CoordinatedClient, cwd string) ([]sdk.SessionInfo, error) {
	storage, err := savedTUISessions(ctx, c, cwd)
	if err != nil {
		return nil, err
	}
	summaries, err := storage.ListCWD(cwd)
	if err != nil {
		return nil, err
	}
	sessions := make([]sdk.SessionInfo, 0, len(summaries))
	for _, info := range summaries {
		sessions = append(sessions, sdk.SessionInfo{
			ID: info.ID, Name: info.Name, Filename: info.Filename, CWD: info.CWD,
			Created: info.Created, LastModified: info.LastModified, Model: info.Model, Status: info.Status, MessageCount: info.MessageCount,
			FirstPrompt: info.FirstPrompt, Pinned: info.Pinned, PinnedAt: info.PinnedAt, Tokens: info.Tokens, ParentID: info.ParentID,
		})
	}
	return sessions, nil
}

// A session ID names one session wherever the client runs, so the daemon
// decides its project.
func resolveSavedTUISession(ctx context.Context, c *client.CoordinatedClient, cwd, ref string) (id, project string, err error) {
	ctx, cancel := context.WithTimeout(ctx, coordinatorStartupTimeout)
	defer cancel()
	return c.LocateSession(ctx, ref, cwd)
}

func savedSessionHeader(storage *sessionpkg.Storage, cwd, id string) (sessionpkg.Header, error) {
	return storage.ProjectHeader(id, cwd)
}

func runCoordinatedInteractive(ctx context.Context, cmd *cli.Command, c *client.CoordinatedClient, socket string, prompts []string, fileImages []ai.ContentBlock) (err error) {
	defer func() {
		if c != nil {
			err = errors.Join(err, c.Close())
		}
	}()
	if err := coordinatedStartupFlags(cmd); err != nil {
		return cli.Exit(err.Error(), ExitUserError)
	}
	if flags, _ := ctx.Value(pluginCLIFlagsKey{}).(map[string]map[string]string); len(flags) != 0 {
		return cli.Exit("plugin CLI options apply to standalone Tau; use --standalone to apply them", ExitUserError)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get launch directory: %w", err)
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return fmt.Errorf("resolve launch directory: %w", err)
	}
	credentials, err := openStore()
	if err != nil {
		return fmt.Errorf("open credentials for coordinated TUI: %w", err)
	}
	var current, notice string
	drafts := make(map[string]string)
	action, arg := coordinatedStartupAction(cmd, len(prompts) > 0)
	var prompt string
	var cliPrompt bool
	if len(prompts) > 0 {
		prompt, cliPrompt = prompts[0], true
	}
	viewCWD := cwd
	var savedDir savedSessionDir
	var pickerState tui.InstancePickerState
	var management *tui.InstancePickerJob
	managementCtx, cancelManagement := context.WithCancel(ctx)
	defer cancelManagement()
	startManagement := func(choice tui.InstanceSelection) *tui.InstancePickerJob {
		management = startTUIInstanceManagement(managementCtx, socket, choice, func(result instanceManagementResult) (string, string, map[string]string) {
			current = result.apply(choice.Action, current, drafts, &pickerState)
			if current == "" {
				viewCWD = cwd
			}
			management = nil
			return current, result.notice, result.changes
		})
		return management
	}
	finishManagement := func() {
		if management != nil {
			select {
			case <-management.Done:
				_, notice, _ = management.Finish()
			default:
			}
		}
	}
	for ctx.Err() == nil {
		finishManagement()
		if action == "instances" || action == "new" || action == "prompt" || action == "resume" || action == "continue" || action == "resume-picker" {
			if c == nil || c.Err() != nil {
				if c != nil {
					if closeErr := c.Close(); closeErr != nil {
						return closeErr
					}
				}
				c, err = waitForInteractiveCoordinator(ctx, socket)
				if err != nil {
					return err
				}
			}
		}
		switch action {
		case "instances":
			choice, pickErr := pickTUIInstance(ctx, c, cwd, current, notice, &pickerState, management, startManagement)
			if pickErr != nil {
				if c.Err() == nil || ctx.Err() != nil {
					return pickErr
				}
				if closeErr := c.Close(); closeErr != nil {
					return errors.Join(pickErr, closeErr)
				}
				c = nil
				notice = pickErr.Error()
				continue
			}
			notice = ""
			finishManagement()
			switch choice.Action {
			case tui.InstanceActionOpen:
				if choice.Value == tui.NewInstanceChoice {
					action, arg = "new", cwd
					continue
				}
				current = choice.Value
			case tui.InstanceActionRename:
				name, confirmed, promptErr := tui.PromptInstanceName(ctx, choice.Targets[0])
				if promptErr != nil {
					return promptErr
				}
				if confirmed {
					renameCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
					if renameErr := c.RenameInstance(renameCtx, choice.Targets[0].ID, name); renameErr != nil {
						notice = "Rename failed: " + renameErr.Error()
					}
					cancel()
				}
				continue
			case tui.InstanceActionStop, tui.InstanceActionRestart, tui.InstanceActionRestartOutdated:
				confirmed, confirmErr := tui.ConfirmInstanceManagement(ctx, choice.Action, choice.Targets)
				if confirmErr != nil {
					return confirmErr
				}
				if confirmed {
					startManagement(choice)
				}
				continue
			default:
				// Escape must not try to attach to a worker that is stopping.
				if management.Includes(current) {
					current, viewCWD = "", cwd
				}
			}
		case "continue", "resume-picker":
			var id string
			if action == "continue" {
				storage, err := savedTUISessions(ctx, c, viewCWD)
				if err == nil {
					id, err = storage.LatestResumableCWD(viewCWD)
				}
				if err != nil {
					notice = err.Error()
				} else if id == "" {
					notice = "No saved session to continue."
				}
			} else {
				sessions, listErr := listSavedTUISessions(ctx, c, viewCWD)
				if listErr != nil {
					notice = listErr.Error()
				} else if len(sessions) == 0 {
					notice = "No saved sessions in this directory."
				} else {
					id, err = tui.SelectSession(ctx, sessions)
					if err != nil {
						return err
					}
				}
			}
			if id != "" {
				action, arg = "resume", id
				continue
			}
			if cliPrompt {
				drafts[current] = prompt
			}
			prompt, cliPrompt = "", false
		case "new", "prompt", "resume":
			launchDir, id := viewCWD, ""
			if action == "prompt" {
				launchDir, prompt, cliPrompt = cwd, arg, false
			}
			if action == "new" && arg != "" {
				launchDir = arg
				if !filepath.IsAbs(launchDir) {
					launchDir = filepath.Join(viewCWD, launchDir)
				}
			}
			if action == "resume" {
				savedID, project, saveErr := resolveSavedTUISession(ctx, c, viewCWD, arg)
				if saveErr != nil {
					notice = "Resume: " + saveErr.Error()
					if cliPrompt {
						drafts[current] = prompt
					} else {
						drafts[current] = "/resume " + arg
					}
					prompt, cliPrompt, action = "", false, ""
					continue
				}
				id, launchDir = savedID, project
			}
			startCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			var instance *coord.Instance
			var startErr error
			if id != "" {
				instance, startErr = c.StartSavedSession(startCtx, launchDir, id)
			} else {
				instance, startErr = c.StartInstance(startCtx, launchDir)
			}
			cancel()
			if startErr != nil {
				notice = "Start instance: " + startErr.Error()
				if errors.Is(startErr, client.ErrOutcomeUnknown) {
					notice += " Check /instances before retrying."
				}
				if prompt != "" {
					drafts[current] = prompt
				} else if id != "" {
					drafts[current] = "/resume " + arg
				}
				prompt, cliPrompt = "", false
			} else {
				current = instance.ID
				viewCWD = launchDir
			}
		case "stop":
			confirmed, stopErr := tui.ConfirmInstanceStop(ctx, coord.Instance{ID: current, CWD: viewCWD})
			if stopErr != nil {
				return stopErr
			}
			if confirmed {
				if stopErr = stopInteractiveInstance(ctx, socket, current); stopErr != nil {
					notice = "Stop instance: " + stopErr.Error()
				} else {
					delete(drafts, current)
				}
				current, viewCWD = "", cwd
				action = "instances"
				continue
			}
		}
		var sess *tui.CoordinatedSession
		var appSession sdk.Session
		startup := "No instance attached · Send a message to start · F8 or /instances to join"
		var model string
		if current != "" {
			sess, err = tui.OpenCoordinatedSession(ctx, socket, current)
			if err != nil {
				notice = fmt.Sprintf("Could not open worker %s: %s. If it is still running, join it with F8.", current, err)
				if prompt != "" {
					drafts[current] = prompt
					delete(drafts, "")
					notice += " Draft kept for that worker."
					if cliPrompt && (len(fileImages) > 0 || len(cmd.StringSlice("image")) > 0) {
						notice += " Reattach the command-line images before retrying."
					}
				}
				current, action, viewCWD = "", "", cwd
				prompt, cliPrompt = "", false
				continue
			}
			appSession = sess
			state := sess.State()
			viewCWD, model = state.CWD(), state.Model()
			dir, dirErr := lookupSavedSessionDir(ctx, c, socket, viewCWD)
			if dirErr != nil {
				slog.Debug("resume hint: look up session directory", "error", dirErr)
			}
			if dir != "" {
				savedDir = savedSessionDir{project: viewCWD, path: dir}
			}
			startup = "Daemon instance " + current[:min(8, len(current))] + " · F8 or /instances to switch · /quit to detach"
			if prompt != "" {
				var sendErr error
				if cliPrompt {
					var msg sdk.Message
					msg, sendErr = buildCLIMessage(ctx, sess, prompt, true, fileImages)
					if sendErr == nil {
						_, sendErr = sess.Send(ctx, msg)
					}
				} else {
					sendErr = sess.SendInitialDraft(ctx, prompt)
				}
				if !cliPrompt {
					delete(drafts, "")
				}
				if sendErr != nil {
					notice = "First message: " + sendErr.Error() + ". Draft text kept."
					if cliPrompt {
						notice += " Reattach any command-line images before retrying."
					}
					if errors.Is(sendErr, client.ErrOutcomeUnknown) {
						notice += " Check the shared transcript and queue before sending again."
					}
					drafts[current] = prompt
				}
				prompt, cliPrompt = "", false
			}
		}
		action, arg = "", ""
		selectedCWD := viewCWD
		settings, settingsErr := coordinatedViewSettings(ctx, selectedCWD)
		var themes *artifacts.Loader
		if settingsErr == nil {
			themes, settingsErr = coordinatedThemes(selectedCWD, settings)
		}
		startupMessages := append([]string{startup}, diagnosticsFromContext(ctx)...)
		if settingsErr != nil {
			startupMessages = append(startupMessages, "Local presentation: "+settingsErr.Error())
		}
		app, appErr := tui.New(tui.Options{
			Session: appSession, Version: pluginsVersion(), CWD: viewCWD, ModelSpec: model, CredentialStore: credentials,
			SettingsPath: filepath.Join(configDir(), "settings.json"), StartupMessages: startupMessages,
			DisplaySettings: settings, ThemeLookup: themeLookup(themes), ThemeNames: themeNames(themes), RefreshTheme: themeRefresher(themes),
			ReloadDisplaySettings: func(ctx context.Context) (*ai.Settings, error) {
				return coordinatedViewSettings(ctx, selectedCWD)
			},
			InitialDraft: drafts[current], InitialNotice: notice,
			ListSessions: func(ctx context.Context) (_ []sdk.SessionInfo, err error) {
				if c.Err() == nil {
					return listSavedTUISessions(ctx, c, selectedCWD)
				}
				fresh, err := findInteractiveCoordinator(ctx, socket)
				if err != nil {
					return nil, err
				}
				if fresh == nil {
					return nil, errors.New("coordinator is not listening")
				}
				defer func() { err = errors.Join(err, fresh.Close()) }()
				return listSavedTUISessions(ctx, fresh, selectedCWD)
			},
			InstanceAction: func(next, value, draft string) {
				// Another client may have restarted this worker under a new ID.
				if sess != nil {
					current = sess.InstanceID()
				}
				action, arg = next, value
				drafts[current] = draft
			},
		})
		if appErr != nil {
			if sess != nil {
				appErr = errors.Join(appErr, sess.Close(ctx))
			}
			return appErr
		}
		notice = ""
		if err := app.Run(ctx); err != nil {
			return fmt.Errorf("coordinated TUI: %w", err)
		}
		if action == "" {
			if sess != nil {
				return printCoordinatedResumeInstructions(os.Stdout, savedDir, cwd, sess.State())
			}
			return nil
		}
	}
	return nil
}

func printCoordinatedResumeInstructions(w io.Writer, saved savedSessionDir, launchCWD string, state sdk.SessionState) error {
	id, project := state.SessionID(), state.CWD()
	if id == "" || project == "" || saved.project != project {
		return nil
	}
	storage, err := sessionpkg.NewStorageAt(saved.path)
	if err != nil {
		slog.Debug("resume hint: open session storage", "error", err)
		return nil
	}
	// --resume requires a saved session even when its worker is still running.
	if _, err := savedSessionHeader(storage, project, id); err != nil {
		slog.Debug("resume hint: verify saved session", "error", err)
		return nil
	}
	if project == launchCWD {
		project = ""
	}
	return printResumeInstructions(w, id, project)
}

// A picker opened during coordinator replacement must not drop local drafts
// or fall back to a standalone session. Ctrl+C still ends the wait.
func waitForInteractiveCoordinator(ctx context.Context, socket string) (*client.CoordinatedClient, error) {
	for warned := false; ; warned = true {
		c, err := findInteractiveCoordinator(ctx, socket)
		if err != nil || c != nil {
			return c, err
		}
		if !warned {
			writeLine(os.Stderr, "Waiting for the Tau coordinator to return (Ctrl+C to exit)…")
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// The TUI may have reattached after a daemon restart while its picker
// connection stayed disconnected. Confirmed stop uses a fresh connection.
func stopInteractiveInstance(ctx context.Context, socket, instanceID string) (err error) {
	c, err := findInteractiveCoordinator(ctx, socket)
	if err != nil {
		return err
	}
	if c == nil {
		return errors.New("coordinator is not listening; no stop was sent")
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return c.StopInstance(ctx, instanceID)
}

type instanceManagementResult struct {
	changes map[string]string // old instance ID to replacement, or empty if stopped
	notice  string
}

// Apply results on the UI side, which owns the draft map and the current view.
func (r instanceManagementResult) apply(action, current string, drafts map[string]string, picker *tui.InstancePickerState) string {
	for old, replacement := range r.changes {
		if action == tui.InstanceActionStop {
			delete(drafts, old)
		} else if replacement != "" {
			if draft, ok := drafts[old]; ok {
				drafts[replacement] = draft
				delete(drafts, old)
			}
		}
		if current == old {
			current = replacement
		}
		if picker.FocusID == old {
			picker.FocusID = replacement
		}
	}
	return current
}

func startTUIInstanceManagement(ctx context.Context, socket string, choice tui.InstanceSelection, finish func(instanceManagementResult) (string, string, map[string]string)) *tui.InstancePickerJob {
	done := make(chan struct{})
	var result instanceManagementResult
	go func() {
		defer close(done)
		// A legacy save check can stall requests on its connection. Keep it
		// off the picker's link as well as off the UI loop.
		c, err := findInteractiveCoordinator(ctx, socket)
		if err != nil {
			result.notice = err.Error()
			return
		}
		if c == nil {
			result.notice = "Coordinator is not listening; no instance action was sent."
			return
		}
		result = manageTUIInstances(ctx, c, choice)
		if err := c.Close(); err != nil {
			result.notice += " · Close instance action connection: " + err.Error()
		}
	}()
	notice := "Restarting 1 instance…"
	if len(choice.Targets) > 1 {
		notice = fmt.Sprintf("Restarting %d instances…", len(choice.Targets))
	}
	if choice.Action == tui.InstanceActionStop {
		notice = "Stopping instance…"
	}
	return &tui.InstancePickerJob{
		Targets: choice.Targets, Notice: notice, Done: done,
		Finish: func() (string, string, map[string]string) { return finish(result) },
	}
}

func manageTUIInstances(ctx context.Context, c *client.CoordinatedClient, choice tui.InstanceSelection) (result instanceManagementResult) {
	result.changes = make(map[string]string, len(choice.Targets))
	if choice.Action == tui.InstanceActionStop {
		instance := choice.Targets[0]
		stopCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		if err := c.StopInstance(stopCtx, instance.ID); err != nil {
			result.notice = "Stop instance: " + err.Error()
			return result
		}
		result.changes[instance.ID] = ""
		if err := waitForInstanceExit(stopCtx, c, instance.ID); err != nil {
			result.notice = "Waiting for stopped instance: " + err.Error()
			return result
		}
		result.notice = "Stopped instance " + instance.ID[:min(8, len(instance.ID))]
		return result
	}
	var restarted int
	var failures []string
	// Restart one at a time so shutdowns and launches don't contend for slots.
	for _, target := range choice.Targets {
		if ctx.Err() != nil {
			break
		}
		targetCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		replacement, stopped, err := restartTUIInstance(targetCtx, c, target)
		cancel()
		if stopped {
			result.changes[target.ID] = ""
		}
		if err != nil {
			if len(failures) < 3 {
				failures = append(failures, fmt.Sprintf("%s: %v", target.ID[:min(8, len(target.ID))], err))
			}
			if c.Err() != nil || errors.Is(err, client.ErrOutcomeUnknown) {
				break
			}
			continue
		}
		restarted++
		result.changes[target.ID] = replacement.ID
	}
	result.notice = fmt.Sprintf("Restarted %d of %d instances", restarted, len(choice.Targets))
	if len(failures) != 0 {
		result.notice += " · " + strings.Join(failures, " · ")
	}
	return result
}

func restartTUIInstance(ctx context.Context, c *client.CoordinatedClient, target coord.Instance) (*coord.Instance, bool, error) {
	// Check that the daemon can resume sessions before shutting anything down.
	if !slices.Contains(c.Welcome().Operations, coord.OperationSavedSessionLaunch) || target.SessionID == "" || !filepath.IsAbs(target.CWD) {
		return nil, false, errors.New("instance cannot be restarted through this coordinator")
	}
	if err := c.StopInstanceForRestart(ctx, target.ID, target.SessionID); err != nil {
		return nil, false, fmt.Errorf("stop worker: %w", err)
	}
	if err := waitForInstanceExit(ctx, c, target.ID); err != nil {
		return nil, true, fmt.Errorf("wait for worker exit: %w", err)
	}
	startCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	instance, err := c.StartNamedSavedSession(startCtx, target.CWD, target.SessionID, target.Name)
	if err != nil {
		return nil, true, fmt.Errorf("worker stopped but could not resume session %s: %w", target.SessionID, err)
	}
	return instance, true, nil
}

func waitForInstanceExit(ctx context.Context, c *client.CoordinatedClient, id string) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		instances, err := c.ListInstances(ctx)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(instances, func(inst coord.Instance) bool { return inst.ID == id && inst.LinkState != coord.LinkExited }) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func pickTUIInstance(ctx context.Context, c *client.CoordinatedClient, cwd, current, notice string, state *tui.InstancePickerState, job *tui.InstancePickerJob, start func(tui.InstanceSelection) *tui.InstancePickerJob) (choice tui.InstanceSelection, err error) {
	watchCtx, cancel := context.WithTimeout(ctx, coordinatorStartupTimeout)
	instances, err := c.WatchInstances(watchCtx)
	cancel()
	if err != nil {
		return tui.InstanceSelection{}, fmt.Errorf("watch Tau instances: %w", err)
	}
	choice, err = tui.SelectInstance(ctx, instances, c.Events(), cwd, current, notice, state, job, start)
	if c.Err() != nil {
		return tui.InstanceSelection{}, errors.Join(err, c.Err())
	}
	unwatchCtx, cancel := context.WithTimeout(ctx, coordinatorStartupTimeout)
	defer cancel()
	if unwatchErr := c.UnwatchInstances(unwatchCtx); unwatchErr != nil {
		return tui.InstanceSelection{}, errors.Join(err, unwatchErr)
	}
	// Unwatch's acknowledgment follows all updates for this subscription.
	// Drain them before a future WatchInstances takes a fresh baseline.
	for {
		select {
		case _, ok := <-c.Events():
			if !ok {
				return tui.InstanceSelection{}, errors.Join(err, c.Err())
			}
		default:
			return choice, err
		}
	}
}
