package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"github.com/google/uuid"
	sessionpkg "github.com/losinggeneration/tau/agent/session"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/logging"
	"github.com/losinggeneration/tau/paths"
	"github.com/losinggeneration/tau/rpc/coord"
	"github.com/losinggeneration/tau/rpc/server"
	"github.com/losinggeneration/tau/sdk"
	"github.com/losinggeneration/tau/ui"
	"github.com/urfave/cli/v3"
	"golang.org/x/sys/unix"
)

func runCoordinator(ctx context.Context, cmd *cli.Command) error {
	cfgDir, err := filepath.Abs(configDir())
	if err != nil {
		return fmt.Errorf("resolve coordinator configuration directory: %w", err)
	}
	node, err := coordinatorNodeID(cfgDir)
	if err != nil {
		return err
	}
	path := cmd.String("socket")
	if path == "" {
		path = server.SocketPath()
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve coordinator socket: %w", err)
	}
	executable, err := workerExecutable()
	if err != nil {
		return err
	}
	sessionDirectory := func(ctx context.Context, cwd string) (string, error) {
		return daemonSessionDirectory(ctx, cfgDir, cwd)
	}
	launch := func(ctx context.Context, request coord.LaunchRequest) error {
		// Resolve storage in the daemon for fresh and resumed workers, so
		// worker-only plugin trust decisions cannot redirect persistence away
		// from the directory shown by the local picker.
		sessionDir, err := sessionDirectory(ctx, request.CWD)
		if err != nil {
			return err
		}
		if request.SessionID != "" {
			if err := checkSavedSession(sessionDir, request.CWD, request.SessionID); err != nil {
				return err
			}
		}
		return launchWorkerProcess(ctx, executable, path, node, request, sessionDir)
	}
	// Keep recovery state outside the socket's volatile runtime directory.
	receiptDir := filepath.Join(cfgDir, fmt.Sprintf("coordinator-%x.workers", sha256.Sum256([]byte(path))))
	run := func(ctx context.Context, path string, launch coord.LaunchFunc) error {
		return coord.RunRecoverable(ctx, path, receiptDir, launch, coord.WithSessionDirectory(sessionDirectory), coord.WithSessionLocator(func(ctx context.Context, ref, hint string) (string, string, error) {
			return locateDaemonSession(ctx, sessionDirectory, ref, hint)
		}), coord.WithWorkerBuild(func() string { return coord.ExecutableBuildID(executable) }))
	}
	addr := cmd.String("websocket")
	if addr == "" {
		return run(ctx, path, launch)
	}
	bridge, err := server.NewCoordinatedBridge(addr, path, cmd.String("websocket-token"), remoteTokenStore())
	if err != nil {
		return cli.Exit(fmt.Sprintf("websocket bridge: %v", err), ExitUserError)
	}
	writeLine(os.Stderr, "tau websocket bridge listening on %s/rpc\n", bridge.Addr())
	return serveCoordinator(ctx, path, bridge, launch, run)
}

func daemonSessionDirectory(ctx context.Context, cfgDir, cwd string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	settings, err := ai.LoadTrustedSettings(ctx, cfgDir, cwd, home, ai.ProjectTrustOptions{})
	if err != nil {
		return "", err
	}
	dir := paths.Dir("TAU_SESSION_DIR", settings.SessionDir, filepath.Join(cfgDir, "sessions"))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(cwd, dir)
	}
	return filepath.Clean(dir), nil
}

// locateDaemonSession searches the storage of the client's directory, when it
// exists on this machine, and then the daemon's default storage.
func locateDaemonSession(ctx context.Context, sessionDirectory coord.SessionDirectoryFunc, ref, hint string) (id, project string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("resolve home directory: %w", err)
	}
	if hint != "" {
		if hint, err = filepath.EvalSymlinks(hint); err != nil {
			slog.Debug("locate session: ignore client directory", "error", err)
			hint = ""
		}
	}
	var searched []string
	for _, base := range []string{hint, home} {
		if base == "" {
			continue
		}
		dir, err := sessionDirectory(ctx, base)
		if err != nil {
			if base == hint {
				slog.Debug("locate session: skip client directory storage", "error", err)
				continue
			}
			return "", "", err
		}
		if slices.Contains(searched, dir) {
			continue
		}
		searched = append(searched, dir)
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return "", "", fmt.Errorf("inspect session storage: %w", err)
		}
		storage, err := sessionpkg.NewStorageAt(dir)
		if err != nil {
			return "", "", err
		}
		header, project, err := storage.LocateSession(ref, hint)
		if errors.Is(err, sessionpkg.ErrSessionNotFound) {
			continue
		}
		if err != nil {
			return "", "", err
		}
		return header.ID, project, nil
	}
	return "", "", fmt.Errorf("session %q %w", ref, sessionpkg.ErrSessionNotFound)
}

func checkSavedSession(dir, cwd, id string) error {
	storage, err := sessionpkg.NewStorageAt(dir)
	if err != nil {
		return err
	}
	header, err := storage.ProjectHeader(id, cwd)
	if errors.Is(err, sessionpkg.ErrOtherProject) || (err == nil && header.ID != id) {
		return errors.New("saved session does not belong to the requested project")
	}
	if err != nil {
		return err
	}
	return storage.CheckResumeOwner(id)
}

// serveCoordinator runs the coordinator and its WebSocket bridge together.
// Either one stopping stops the other; the bridge only dials the
// coordinator per connection, so neither waits for the other to start.
func serveCoordinator(ctx context.Context, path string, bridge *server.CoordinatedBridge, launch coord.LaunchFunc, run func(context.Context, string, coord.LaunchFunc) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	bridgeDone := make(chan error, 1)
	go func() {
		err := bridge.Run(ctx)
		cancel()
		bridgeDone <- err
	}()
	err := run(ctx, path, launch)
	cancel()
	return errors.Join(err, <-bridgeDone)
}

func coordinatorNodeID(dir string) (id string, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create node configuration directory: %w", err)
	}
	lock := flock.New(filepath.Join(dir, "node-id.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return "", fmt.Errorf("lock node identity: %w", err)
	}
	if !locked {
		return "", errors.New("node identity is being initialized; retry coordinator startup")
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	path := filepath.Join(dir, "node-id")
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return "", fmt.Errorf("read node identity: %w", err)
	default:
		// An empty file is an interrupted first save; no identity was ever
		// published, so generating one is safe.
		if id = strings.TrimSpace(string(data)); id != "" {
			if _, err := uuid.Parse(id); err != nil {
				return "", fmt.Errorf("invalid saved node identity: %w", err)
			}
			return id, nil
		}
	}
	id = uuid.NewString()
	if err := writeFileAtomic(path, []byte(id+"\n")); err != nil {
		return "", fmt.Errorf("save node identity: %w", err)
	}
	return id, nil
}

// writeFileAtomic replaces path with a fully synced private file, so a crash
// leaves either the old contents or the new ones.
func writeFileAtomic(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if removeErr := os.Remove(tmp.Name()); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, removeErr)
			}
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Sync(); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

var workerLaunchVariables = []string{"TAU_COORD_TOKEN", "TAU_COORD_INSTANCE", "TAU_COORD_NODE", "TAU_COORD_LAUNCH_REQUEST", "TAU_COORD_LIFETIME", "TAU_COORD_SESSION", "TAU_COORD_SESSION_DIR"}

func workerEnvironment(env []string, node string, request coord.LaunchRequest, sessionDir string) []string {
	out := make([]string, 0, len(env)+len(workerLaunchVariables))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		// PWD describes the coordinator's directory, not the worker's.
		if name == "PWD" || name == "TAU_WEBSOCKET_TOKEN" || slices.Contains(workerLaunchVariables, name) {
			continue
		}
		out = append(out, entry)
	}
	out = append(out, "TAU_COORD_TOKEN="+request.Token, "TAU_COORD_INSTANCE="+request.InstanceID, "TAU_COORD_NODE="+node)
	if request.LaunchRequestID != "" {
		out = append(out, "TAU_COORD_LAUNCH_REQUEST="+request.LaunchRequestID)
	}
	if request.SessionID != "" {
		out = append(out, "TAU_COORD_SESSION="+request.SessionID)
	}
	if sessionDir != "" {
		out = append(out, "TAU_COORD_SESSION_DIR="+sessionDir)
	}
	if request.Lifetime != nil {
		out = append(out, "TAU_COORD_LIFETIME=3")
	}
	return out
}

// Keep the path used to launch workers, including the managed install's
// current symlink, so replacing the installed binary does not need a daemon restart.
func workerExecutable() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve worker executable: %w", err)
	}
	return workerLaunchPath(os.Getenv("TAU_MANAGED_INSTALL_ROOT"), executable)
}

func workerLaunchPath(configuredRoot, executable string) (string, error) {
	root, err := managedInstallRoot(configuredRoot, executable)
	if err != nil {
		return "", err
	}
	if root != "" {
		return filepath.Join(root, "current", managedBinaryName()), nil
	}
	return executable, nil
}

func launchWorkerProcess(ctx context.Context, executable, socket, node string, request coord.LaunchRequest, sessionDir string) (err error) {
	instance, cwd, ready := request.InstanceID, request.CWD, request.Ready
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(cwd) {
		return errors.New("worker project directory must be absolute")
	}
	cfg, err := filepath.Abs(configDir())
	if err != nil {
		return fmt.Errorf("resolve worker configuration: %w", err)
	}
	logDir, err := workerLogDirectory(cfg)
	if err != nil {
		return err
	}
	if _, err := uuid.Parse(instance); err != nil {
		return fmt.Errorf("invalid worker identity: %w", err)
	}
	log, err := os.OpenFile(filepath.Join(logDir, instance+".log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("open worker log: %w", err)
	}
	defer func() { err = errors.Join(err, log.Close()) }()
	child := exec.Command(executable, "--config-dir", cfg, "worker", "--socket", socket)
	child.Dir = cwd
	child.Env = workerEnvironment(os.Environ(), node, request, sessionDir)
	if request.Lifetime != nil {
		child.ExtraFiles = []*os.File{request.Lifetime}
	}
	child.Stdout = log
	child.Stderr = log
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := child.Start(); err != nil {
		return fmt.Errorf("start worker: %w", err)
	}
	worker := startedProcess(child, instance)
	if ready == nil {
		go worker.reap()
		return nil
	}
	return superviseWorkerStart(ctx, worker, ready, instance+".log")
}

const workerExitPoll = 25 * time.Millisecond

// startedWorker is a started worker whose session leader has not been
// reaped. Until the supervisor reaps it, or hands it to reap, the leader's
// PID and process group ID cannot be reused, so signaling the group is safe.
type startedWorker struct {
	pid int
	// exited reports an exit without reaping the leader.
	exited func() (bool, error)
	kill   func(int, syscall.Signal) error
	// reap waits for and reaps the leader in the background.
	reap func()
}

func startedProcess(child *exec.Cmd, instance string) startedWorker {
	return startedWorker{
		pid:    child.Process.Pid,
		exited: func() (bool, error) { return workerExited(child.Process.Pid) },
		kill:   syscall.Kill,
		reap: func() {
			if err := child.Wait(); err != nil {
				slog.Warn("worker exited", "instance_id", instance, "error", err)
			}
		},
	}
}

func (w startedWorker) killGroup() error {
	if err := w.kill(-w.pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill unready worker group: %w", err)
	}
	return nil
}

// superviseWorkerStart waits for readiness while it alone reaps the leader.
// The coordinator cancels ctx only after revoking registration, so a
// canceled, unready start can no longer register and its whole process group
// is killed before the leader is reaped. A ready worker is left running.
func superviseWorkerStart(ctx context.Context, w startedWorker, ready <-chan struct{}, logName string) error {
	poll := time.NewTicker(workerExitPoll)
	defer poll.Stop()
	for {
		select {
		case <-ready:
			go w.reap()
			return nil
		case <-ctx.Done():
			select {
			case <-ready:
				go w.reap()
				return nil
			default:
			}
			killErr := w.killGroup()
			go w.reap()
			return errors.Join(fmt.Errorf("worker readiness: %w", ctx.Err()), killErr)
		case <-poll.C:
			exited, err := w.exited()
			if !exited && err == nil {
				continue
			}
			select {
			case <-ready:
				go w.reap()
				return nil
			default:
			}
			if err == nil {
				err = errors.New("worker exited before readiness")
			}
			if !errors.Is(err, syscall.ECHILD) {
				err = errors.Join(err, w.killGroup())
			}
			go w.reap()
			return fmt.Errorf("initialize worker; inspect private log %s: %w", logName, err)
		}
	}
}

func workerLogDirectory(cfg string) (string, error) {
	dir := filepath.Join(cfg, "workers")
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("create worker log directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("inspect worker log directory: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || !ok || stat.Uid != uint32(os.Getuid()) {
		return "", errors.New("worker log directory must be private and owned by the current user")
	}
	return dir, nil
}

func runWorker(ctx context.Context, cmd *cli.Command) (err error) {
	token := os.Getenv("TAU_COORD_TOKEN")
	instance := os.Getenv("TAU_COORD_INSTANCE")
	node := os.Getenv("TAU_COORD_NODE")
	launchRequest := os.Getenv("TAU_COORD_LAUNCH_REQUEST")
	sessionID := os.Getenv("TAU_COORD_SESSION")
	sessionDir := os.Getenv("TAU_COORD_SESSION_DIR")
	if token == "" || instance == "" || node == "" || cmd.String("socket") == "" {
		return errors.New("worker is an internal entry point; launch through the coordinator")
	}
	if os.Getenv("TAU_COORD_LIFETIME") == "3" {
		// Keep the inherited lock held until this process exits. A tool or
		// extension subprocess must not keep it alive after worker exit.
		unix.CloseOnExec(3)
	}
	for _, name := range workerLaunchVariables {
		if err := os.Unsetenv(name); err != nil {
			return fmt.Errorf("clear worker launch environment: %w", err)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve worker directory: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	startupTimer := time.AfterFunc(15*time.Second, cancel)
	defer startupTimer.Stop()
	opts, mgr, loader, reg, err := buildRuntimeOptions(ctx, ui.NewNoOpUI(), nil, func(cfg *runtimeBuildConfig) { cfg.sessionDir = sessionDir })
	if err != nil {
		return fmt.Errorf("initialize worker: %w", err)
	}
	GlobalCommandRegistry = reg
	GlobalPluginManager = mgr
	if loader != nil {
		sdk.GlobalArtifactLoader = loader
		opts.ArtifactLoader = loader
	}
	opts.PluginManager = mgr
	opts.Logger = logging.NewText(os.Stderr, slog.LevelInfo)
	runtime, err := sdk.NewRuntime(opts)
	if err != nil {
		cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()
		if mgr != nil {
			err = errors.Join(err, mgr.Shutdown(cleanupCtx))
		}
		return fmt.Errorf("create worker runtime: %w", err)
	}
	if !startupTimer.Stop() {
		cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()
		return errors.Join(errors.New("worker initialization exceeded deadline"), runtime.Close(cleanupCtx))
	}
	// On Linux this describes the running executable even after the installed
	// path is replaced. Fall back to the process path on other platforms.
	build := coord.ExecutableBuildID("/proc/self/exe")
	if build == "" {
		if exe, err := os.Executable(); err == nil {
			build = coord.ExecutableBuildID(exe)
		}
	}
	return server.NewWorker(runtime, reg, mgr).Run(ctx, cmd.String("socket"), coord.Instance{
		ID: instance, NodeID: node, CWD: cwd, SessionID: sessionID, PID: os.Getpid(), State: coord.StateAwaitingUser, LaunchRequestID: launchRequest, BuildID: build,
	}, token)
}
