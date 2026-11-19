package cli

import (
	"context"
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
	"github.com/losinggeneration/tau/logging"
	"github.com/losinggeneration/tau/rpc/coord"
	"github.com/losinggeneration/tau/rpc/server"
	"github.com/losinggeneration/tau/sdk"
	"github.com/losinggeneration/tau/ui"
	"github.com/urfave/cli/v3"
)

func runCoordinator(ctx context.Context, cmd *cli.Command) error {
	node, err := coordinatorNodeID(configDir())
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
	launch := func(ctx context.Context, request coord.LaunchRequest) error {
		return launchWorkerProcess(ctx, path, node, request)
	}
	addr := cmd.String("websocket")
	if addr == "" {
		return coord.Run(ctx, path, launch)
	}
	bridge, err := server.NewCoordinatedBridge(addr, path, cmd.String("websocket-token"), remoteTokenStore())
	if err != nil {
		return cli.Exit(fmt.Sprintf("websocket bridge: %v", err), ExitUserError)
	}
	writeLine(os.Stderr, "tau websocket bridge listening on %s/rpc\n", bridge.Addr())
	return serveCoordinator(ctx, path, bridge, launch, coord.Run)
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

var workerLaunchVariables = []string{"TAU_COORD_TOKEN", "TAU_COORD_INSTANCE", "TAU_COORD_NODE", "TAU_COORD_LAUNCH_REQUEST"}

func workerEnvironment(env []string, node string, request coord.LaunchRequest) []string {
	out := make([]string, 0, len(env)+len(workerLaunchVariables))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		// PWD describes the coordinator's directory, not the worker's.
		if name == "PWD" || slices.Contains(workerLaunchVariables, name) {
			continue
		}
		out = append(out, entry)
	}
	out = append(out, "TAU_COORD_TOKEN="+request.Token, "TAU_COORD_INSTANCE="+request.InstanceID, "TAU_COORD_NODE="+node)
	if request.LaunchRequestID != "" {
		out = append(out, "TAU_COORD_LAUNCH_REQUEST="+request.LaunchRequestID)
	}
	return out
}

func launchWorkerProcess(ctx context.Context, socket, node string, request coord.LaunchRequest) (err error) {
	instance, cwd, ready := request.InstanceID, request.CWD, request.Ready
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(cwd) {
		return errors.New("worker project directory must be absolute")
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve worker executable: %w", err)
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
	child.Env = workerEnvironment(os.Environ(), node, request)
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
	if token == "" || instance == "" || node == "" || cmd.String("socket") == "" {
		return errors.New("worker is an internal entry point; launch through the coordinator")
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
	opts, mgr, loader, reg, err := buildRuntimeOptions(ctx, ui.NewNoOpUI(), nil)
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
	return server.NewWorker(runtime, reg, mgr).Run(ctx, cmd.String("socket"), coord.Instance{
		ID: instance, NodeID: node, CWD: cwd, PID: os.Getpid(), State: "ready", LaunchRequestID: launchRequest,
	}, token)
}
