package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
)

func writeManagedMarker(t *testing.T, root, kind string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(root, managedInstallMarker),
		[]byte(`{"kind":"`+kind+`","schemaVersion":1,"layout":"releases-v1"}`), 0o644))
}

func newManagedTestRoot(t *testing.T, versions ...string) string {
	t.Helper()
	root := t.TempDir()
	writeManagedMarker(t, root, "tau-managed-install")
	for _, v := range versions {
		dir := filepath.Join(root, "releases", v)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, managedBinaryName()), []byte("tau "+v), 0o755))
	}
	return root
}

func managedReleaseServer(t *testing.T, latest string, binary []byte, sum string) *httptest.Server {
	t.Helper()
	asset := managedAssetName()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest/download/VERSION":
			_, _ = w.Write([]byte(latest + "\n"))
		case "/download/v" + latest + "/" + asset:
			_, _ = w.Write(binary)
		case "/download/v" + latest + "/" + asset + ".sha256":
			_, _ = w.Write([]byte(sum + "  " + asset + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func newTestUpdater(root, base, running string, verify func(context.Context, string, string) error) *managedUpdater {
	return &managedUpdater{
		root:    root,
		base:    base,
		client:  http.DefaultClient,
		running: running,
		verify:  verify,
		warn:    func(error) {},
	}
}

func TestManagedUpdateKeepsTheSelectedReleaseAndItsPredecessor(t *testing.T) {
	root := newManagedTestRoot(t, "0.9.0", "1.0.0", "1.1.0")
	require.NoError(t, os.Symlink(filepath.Join("releases", "1.1.0"), filepath.Join(root, "current")))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "releases", "notes"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "staging", "update-stale"), 0o755))

	binary := []byte("tau 1.2.0")
	srv := managedReleaseServer(t, "1.2.0", binary, sha256Hex(binary))
	var verified string
	u := newTestUpdater(root, srv.URL+"/", "1.1.0", func(_ context.Context, bin, version string) error {
		verified = version
		got, err := os.ReadFile(bin)
		require.NoError(t, err)
		assert.Equal(t, binary, got)
		return nil
	})

	ctx := context.Background()
	version, err := u.latest(ctx)
	require.NoError(t, err)
	require.Equal(t, "1.2.0", version)
	require.NoError(t, u.update(ctx, version))

	assert.Equal(t, "1.2.0", verified)
	target, err := os.Readlink(filepath.Join(root, "current"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("releases", "1.2.0"), target)
	got, err := os.ReadFile(filepath.Join(root, "current", managedBinaryName()))
	require.NoError(t, err)
	assert.Equal(t, binary, got)

	entries, err := os.ReadDir(filepath.Join(root, "releases"))
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, []string{"1.1.0", "1.2.0", "notes"}, names)
	staging, err := os.ReadDir(filepath.Join(root, "staging"))
	require.NoError(t, err)
	assert.Empty(t, staging)
}

func TestManagedUpdateFailuresLeaveTheInstallationUsable(t *testing.T) {
	binary := []byte("tau 1.2.0")
	errSmoke := errors.New("smoke test failed")
	for name, tc := range map[string]struct {
		sum    string
		verify error
		want   string
	}{
		"checksum mismatch": {sum: sha256Hex([]byte("other")), want: "checksum mismatch"},
		"smoke test":        {sum: sha256Hex(binary), verify: errSmoke},
	} {
		t.Run(name, func(t *testing.T) {
			root := newManagedTestRoot(t, "1.0.0", "1.1.0")
			require.NoError(t, os.Symlink(filepath.Join("releases", "1.1.0"), filepath.Join(root, "current")))
			srv := managedReleaseServer(t, "1.2.0", binary, tc.sum)
			u := newTestUpdater(root, srv.URL, "1.1.0", func(context.Context, string, string) error { return tc.verify })

			err := u.update(context.Background(), "1.2.0")
			require.Error(t, err)
			if tc.verify != nil {
				require.ErrorIs(t, err, errSmoke)
			} else {
				assert.Contains(t, err.Error(), tc.want)
			}

			target, err := os.Readlink(filepath.Join(root, "current"))
			require.NoError(t, err)
			assert.Equal(t, filepath.Join("releases", "1.1.0"), target)
			assert.NoDirExists(t, filepath.Join(root, "releases", "1.2.0"))
			assert.DirExists(t, filepath.Join(root, "releases", "1.0.0"))
			staging, err := os.ReadDir(filepath.Join(root, "staging"))
			require.NoError(t, err)
			assert.Empty(t, staging)
		})
	}

	t.Run("missing release", func(t *testing.T) {
		root := newManagedTestRoot(t, "1.1.0")
		srv := managedReleaseServer(t, "1.2.0", binary, sha256Hex(binary))
		u := newTestUpdater(root, srv.URL, "1.1.0", func(context.Context, string, string) error { return nil })
		err := u.update(context.Background(), "1.3.0")
		require.ErrorContains(t, err, "HTTP 404")
		assert.NoFileExists(t, filepath.Join(root, "current"))
	})

	t.Run("invalid version", func(t *testing.T) {
		u := newTestUpdater(newManagedTestRoot(t), "http://unused", "1.1.0", nil)
		require.ErrorContains(t, u.update(context.Background(), "../1.2.0"), "invalid managed release version")
	})

	t.Run("concurrent update", func(t *testing.T) {
		root := newManagedTestRoot(t)
		lock := flock.New(filepath.Join(root, "update.lock"))
		locked, err := lock.TryLock()
		require.NoError(t, err)
		require.True(t, locked)
		t.Cleanup(func() { require.NoError(t, lock.Unlock()) })
		u := newTestUpdater(root, "http://unused", "1.1.0", nil)
		require.ErrorContains(t, u.update(context.Background(), "1.2.0"), "already running")
	})
}

func TestManagedUpdateReactivatesAnInstalledRelease(t *testing.T) {
	root := newManagedTestRoot(t, "1.0.0", "1.1.0", "1.2.0")
	var verified int
	u := newTestUpdater(root, "http://unused", "1.2.0", func(context.Context, string, string) error {
		verified++
		return nil
	})
	require.NoError(t, u.update(context.Background(), "1.1.0"))
	assert.Equal(t, 1, verified)
	target, err := os.Readlink(filepath.Join(root, "current"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("releases", "1.1.0"), target)
	assert.NoDirExists(t, filepath.Join(root, "releases", "1.0.0"))
	assert.DirExists(t, filepath.Join(root, "releases", "1.2.0"))
}

func TestManagedInstallRootRequiresAMarkedReleaseExecutable(t *testing.T) {
	root := newManagedTestRoot(t, "1.1.0")
	exe := filepath.Join(root, "releases", "1.1.0", managedBinaryName())

	got, err := managedInstallRoot("", exe)
	require.NoError(t, err)
	assert.Empty(t, got)

	got, err = managedInstallRoot(root, filepath.Join(t.TempDir(), "tau"))
	require.NoError(t, err)
	assert.Empty(t, got, "an executable outside the releases is not managed")

	got, err = managedInstallRoot(root, exe)
	require.NoError(t, err)
	assert.Equal(t, root, got)

	writeManagedMarker(t, root, "pi-managed-install")
	_, err = managedInstallRoot(root, exe)
	require.ErrorContains(t, err, "marker is missing or invalid")
}

func TestRunManagedUpdateReportsTheOutcome(t *testing.T) {
	binary := []byte("tau 1.2.0")
	srv := managedReleaseServer(t, "1.2.0", binary, sha256Hex(binary))
	noVerify := func(context.Context, string, string) error { return nil }

	var out bytes.Buffer
	root := newManagedTestRoot(t, "1.2.0")
	require.NoError(t, runManagedUpdate(context.Background(), &out, newTestUpdater(root, srv.URL, "1.2.0", noVerify)))
	assert.Equal(t, "tau 1.2.0 is already up to date\n", out.String())
	assert.NoFileExists(t, filepath.Join(root, "current"))

	out.Reset()
	root = newManagedTestRoot(t, "1.1.0")
	require.NoError(t, runManagedUpdate(context.Background(), &out, newTestUpdater(root, srv.URL, "1.1.0", noVerify)))
	assert.Equal(t, "Updating managed tau installation...\nUpdated tau from 1.1.0 to 1.2.0\n", out.String())

	err := runManagedUpdate(context.Background(), &out, newTestUpdater(root, srv.URL+"/missing", "1.1.0", noVerify))
	var exit cli.ExitCoder
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, ExitSubsystemError, exit.ExitCode())
}

type managedReleaseTransport func(*http.Request) (*http.Response, error)

func (f managedReleaseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestManagedUpdateCancellationLeavesInstallationUntouched(t *testing.T) {
	for _, phase := range []string{"before admission", "installed verification", "download", "staged verification"} {
		t.Run(phase, func(t *testing.T) {
			root := newManagedTestRoot(t, "0.9.0", "1.1.0")
			if phase == "before admission" || phase == "installed verification" {
				dir := filepath.Join(root, "releases", "1.2.0")
				require.NoError(t, os.MkdirAll(dir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(dir, managedBinaryName()), []byte("installed"), 0o755))
			}
			require.NoError(t, os.Symlink(filepath.Join("releases", "1.1.0"), filepath.Join(root, "current")))
			stale := filepath.Join(root, "staging", "update-stale")
			require.NoError(t, os.MkdirAll(stale, 0o755))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var verified, requests int
			binary := []byte("downloaded binary")
			u := newTestUpdater(root, "https://releases.example.com", "1.1.0", func(got context.Context, _ string, _ string) error {
				assert.Same(t, ctx, got)
				verified++
				if phase == "installed verification" || phase == "staged verification" {
					cancel()
				}
				return nil
			})
			u.client = &http.Client{Transport: managedReleaseTransport(func(req *http.Request) (*http.Response, error) {
				requests++
				require.NoError(t, req.Context().Err())
				body := string(binary)
				if strings.HasSuffix(req.URL.Path, ".sha256") {
					body = sha256Hex(binary)
				} else if phase == "download" {
					cancel()
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
			})}
			if phase == "before admission" {
				cancel()
			}
			err := u.update(ctx, "1.2.0")
			require.ErrorIs(t, err, context.Canceled)
			target, err := os.Readlink(filepath.Join(root, "current"))
			require.NoError(t, err)
			assert.Equal(t, filepath.Join("releases", "1.1.0"), target)
			assert.DirExists(t, filepath.Join(root, "releases", "0.9.0"))
			if phase == "before admission" {
				assert.Zero(t, verified)
				assert.Zero(t, requests)
				assert.DirExists(t, stale)
				assert.NoFileExists(t, filepath.Join(root, "update.lock"))
			} else {
				if phase == "download" {
					assert.Zero(t, verified)
				} else {
					assert.Equal(t, 1, verified)
				}
				entries, err := os.ReadDir(filepath.Join(root, "staging"))
				require.NoError(t, err)
				assert.Empty(t, entries)
				if phase != "installed verification" {
					assert.NoDirExists(t, filepath.Join(root, "releases", "1.2.0"))
				}
				lock := flock.New(filepath.Join(root, "update.lock"))
				locked, err := lock.TryLock()
				require.NoError(t, err)
				require.True(t, locked)
				require.NoError(t, lock.Unlock())
			}
		})
	}
}
