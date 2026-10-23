package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/gofrs/flock"
	"github.com/urfave/cli/v3"
)

const (
	defaultInstallerBase = "https://github.com/losinggeneration/tau/releases"
	managedInstallMarker = "managed-install.json"
)

var managedReleaseVersionRE = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

// tauVersion reports the module version stamped into the binary, or "dev"
// for builds without one.
func tauVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "dev"
	}
	return strings.TrimPrefix(info.Main.Version, "v")
}

func managedBinaryName() string {
	if runtime.GOOS == "windows" {
		return "tau.exe"
	}
	return "tau"
}

func managedAssetName() string {
	name := "tau-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// managedInstallRoot returns the managed install root when exe runs from one
// of its releases. The root is inherited by child processes, so a tau started
// from elsewhere under a managed tau is not treated as managed.
func managedInstallRoot(configured, exe string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return "", nil
	}
	root, err := filepath.Abs(configured)
	if err != nil {
		return "", fmt.Errorf("resolve managed install root: %w", err)
	}
	releases, err := filepath.EvalSymlinks(filepath.Join(root, "releases"))
	if err != nil {
		return "", nil //nolint:nilerr // A root without releases does not manage this binary.
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if rel, err := filepath.Rel(releases, exe); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", nil //nolint:nilerr // An executable outside the releases is not managed.
	}

	markerPath := filepath.Join(root, managedInstallMarker)
	var marker struct {
		Kind          string `json:"kind"`
		Layout        string `json:"layout"`
		SchemaVersion int    `json:"schemaVersion"`
	}
	data, err := os.ReadFile(markerPath)
	if err == nil {
		err = json.Unmarshal(data, &marker)
	}
	if err != nil || marker.Kind != "tau-managed-install" || marker.SchemaVersion != 1 || marker.Layout != "releases-v1" {
		return "", fmt.Errorf("managed install marker is missing or invalid: %s", markerPath)
	}
	return root, nil
}

// managedUpdater installs releases into root/releases/<version> and activates
// one by pointing the root/current symlink at it.
type managedUpdater struct {
	root    string
	base    string
	client  *http.Client
	running string
	verify  func(ctx context.Context, binary, version string) error
	warn    func(error)
}

func (u *managedUpdater) url(parts ...string) string {
	return strings.TrimRight(u.base, "/") + "/" + strings.Join(parts, "/")
}

func (u *managedUpdater) get(ctx context.Context, url string) (resp *http.Response, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "tau/"+u.running)
	resp, err = u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		closeErr := resp.Body.Close()
		return nil, errors.Join(fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode), closeErr)
	}
	return resp, nil
}

func (u *managedUpdater) fetchText(ctx context.Context, url string) (text string, err error) {
	resp, err := u.get(ctx, url)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", url, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// latest returns the version published as the newest release.
func (u *managedUpdater) latest(ctx context.Context) (string, error) {
	version, err := u.fetchText(ctx, u.url("latest", "download", "VERSION"))
	if err != nil {
		return "", err
	}
	version = strings.TrimPrefix(version, "v")
	if !managedReleaseVersionRE.MatchString(version) {
		return "", fmt.Errorf("invalid managed release version: %q", version)
	}
	return version, nil
}

func (u *managedUpdater) download(ctx context.Context, version, dest string) (err error) {
	asset := managedAssetName()
	sumText, err := u.fetchText(ctx, u.url("download", "v"+version, asset+".sha256"))
	if err != nil {
		return err
	}
	fields := strings.Fields(sumText)
	if len(fields) == 0 {
		return fmt.Errorf("empty checksum for %s", asset)
	}
	want, err := hex.DecodeString(fields[0])
	if err != nil {
		return fmt.Errorf("parse checksum for %s: %w", asset, err)
	}

	resp, err := u.get(ctx, u.url("download", "v"+version, asset))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, hash), resp.Body); err != nil {
		return fmt.Errorf("download %s: %w", asset, err)
	}
	if !bytes.Equal(hash.Sum(nil), want) {
		return fmt.Errorf("checksum mismatch for %s %s", asset, version)
	}
	return nil
}

// update installs version unless it is already present, activates it, and
// removes releases other than the selected one and the running one. Failures
// before activation leave the current release in place.
func (u *managedUpdater) update(ctx context.Context, version string) (err error) {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("update managed tau: %w", err)
	}
	if !managedReleaseVersionRE.MatchString(version) {
		return fmt.Errorf("invalid managed release version: %q", version)
	}
	lock := flock.New(filepath.Join(u.root, "update.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return fmt.Errorf("lock managed install: %w", err)
	}
	if !locked {
		return errors.New("another managed tau update is already running")
	}
	defer func() { err = errors.Join(err, lock.Unlock()) }()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("update managed tau: %w", err)
	}

	stagingRoot := filepath.Join(u.root, "staging")
	if err := cleanupManagedStaging(stagingRoot); err != nil {
		return err
	}
	releasesRoot := filepath.Join(u.root, "releases")
	if err := os.MkdirAll(releasesRoot, 0o755); err != nil {
		return fmt.Errorf("create releases: %w", err)
	}
	releaseDir := filepath.Join(releasesRoot, version)
	if _, statErr := os.Stat(releaseDir); statErr == nil {
		if err := u.verify(ctx, filepath.Join(releaseDir, managedBinaryName()), version); err != nil {
			return err
		}
		return u.activate(ctx, version)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect release %s: %w", version, statErr)
	}

	if err := os.MkdirAll(stagingRoot, 0o755); err != nil {
		return fmt.Errorf("create staging: %w", err)
	}
	stageDir, err := os.MkdirTemp(stagingRoot, "update-")
	if err != nil {
		return fmt.Errorf("create staging: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(stageDir); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("remove staging: %w", rmErr))
		}
	}()
	binary := filepath.Join(stageDir, managedBinaryName())
	if err := u.download(ctx, version, binary); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("download managed tau: %w", err)
	}
	if err := u.verify(ctx, binary, version); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("verify managed tau: %w", err)
	}
	if err := os.Rename(stageDir, releaseDir); err != nil {
		return fmt.Errorf("install release %s: %w", version, err)
	}
	return u.activate(ctx, version)
}

func (u *managedUpdater) activate(ctx context.Context, version string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("activate managed tau: %w", err)
	}
	tmp := filepath.Join(u.root, fmt.Sprintf("current.tmp.%d", os.Getpid()))
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("activate %s: %w", version, err)
	}
	if err := os.Symlink(filepath.Join("releases", version), tmp); err != nil {
		return fmt.Errorf("activate %s: %w", version, err)
	}
	if err := os.Rename(tmp, filepath.Join(u.root, "current")); err != nil {
		return errors.Join(fmt.Errorf("activate %s: %w", version, err), os.Remove(tmp))
	}
	u.prune(version)
	return nil
}

// prune keeps the active release and the running one, which open sessions
// likely still use and which allows rolling back by repointing current.
func (u *managedUpdater) prune(active string) {
	releasesRoot := filepath.Join(u.root, "releases")
	entries, err := os.ReadDir(releasesRoot)
	if err != nil {
		u.warn(fmt.Errorf("list releases: %w", err))
		return
	}
	for _, e := range entries {
		name := e.Name()
		if name == active || name == u.running || !managedReleaseVersionRE.MatchString(name) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(releasesRoot, name)); err != nil {
			u.warn(fmt.Errorf("remove release %s (retried on the next update): %w", name, err))
		}
	}
}

func cleanupManagedStaging(stagingRoot string) error {
	entries, err := os.ReadDir(stagingRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list staging: %w", err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "update-") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(stagingRoot, e.Name())); err != nil {
			return fmt.Errorf("remove stale staging: %w", err)
		}
	}
	return nil
}

// smokeTestRelease runs the staged binary and checks the version it reports.
func smokeTestRelease(ctx context.Context, binary, version string) error {
	out, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil {
		return fmt.Errorf("verify managed tau %s: %w", version, err)
	}
	if got := strings.TrimSpace(string(out)); got != "tau version "+version {
		return fmt.Errorf("managed tau smoke test reported %q; expected version %s", got, version)
	}
	return nil
}

// runUpdateSelf handles `tau update self` and `tau update pi`.
func runUpdateSelf(ctx context.Context, cmd *cli.Command) error {
	exe, err := os.Executable()
	if err != nil {
		return cli.Exit(fmt.Sprintf("locate tau executable: %v", err), ExitSubsystemError)
	}
	root, err := managedInstallRoot(os.Getenv("TAU_MANAGED_INSTALL_ROOT"), exe)
	if err != nil {
		return cli.Exit(err.Error(), ExitSubsystemError)
	}
	if root == "" {
		return cli.Exit("tau is not a managed installation; "+
			"update it the way it was installed, such as go install or your package manager", ExitSubsystemError)
	}
	base := strings.TrimSpace(os.Getenv("TAU_INSTALLER_API_BASE"))
	if base == "" {
		base = defaultInstallerBase
	}
	u := &managedUpdater{
		root:    root,
		base:    base,
		client:  http.DefaultClient,
		running: tauVersion(),
		verify:  smokeTestRelease,
		warn: func(err error) {
			writeLine(cmd.Root().ErrWriter, "warning: %v\n", err)
		},
	}
	return runManagedUpdate(ctx, cmd.Root().Writer, u)
}

func runManagedUpdate(ctx context.Context, out io.Writer, u *managedUpdater) error {
	version, err := u.latest(ctx)
	if err != nil {
		return cli.Exit(fmt.Sprintf("Error: %v", err), ExitSubsystemError)
	}
	if version == u.running {
		writeLine(out, "tau %s is already up to date\n", version)
		return nil
	}
	writeLine(out, "Updating managed tau installation...\n")
	if err := u.update(ctx, version); err != nil {
		return cli.Exit(fmt.Sprintf("Error: %v", err), ExitSubsystemError)
	}
	writeLine(out, "Updated tau from %s to %s\n", u.running, version)
	return nil
}
