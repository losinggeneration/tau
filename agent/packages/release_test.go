package packages

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCurrentPlatform(t *testing.T) {
	platform := currentPlatform()
	assert.NotEmpty(t, platform)
	assert.Contains(t, platform, "/")
}

func TestFindRelease(t *testing.T) {
	manifest := &Manifest{
		Releases: []ReleaseSection{
			{Platform: "linux/amd64", URL: "https://example.com/linux-amd64.tar.gz", SHA256: "abc123"},
			{Platform: "darwin/arm64", URL: "https://example.com/darwin-arm64.tar.gz", SHA256: "def456"},
		},
	}

	// Current platform should match one of the releases or return nil
	release := findRelease(manifest)
	// We don't know the actual platform in tests, so just check it's valid or nil
	if release != nil {
		assert.NotEmpty(t, release.URL)
		assert.NotEmpty(t, release.SHA256)
	}
}

func TestVerifySHA256(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		expected string
		wantErr  bool
	}{
		{
			name:     "valid hash",
			data:     []byte("hello world"),
			expected: "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9",
			wantErr:  false,
		},
		{
			name:     "invalid hash",
			data:     []byte("hello world"),
			expected: "0000000000000000000000000000000000000000000000000000000000000000",
			wantErr:  true,
		},
		{
			name:     "empty hash",
			data:     []byte("hello world"),
			expected: "",
			wantErr:  true,
		},
		{
			name:     "hash with whitespace",
			data:     []byte("hello world"),
			expected: "  b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9  \n",
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifySHA256(tt.data, tt.expected)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestExtractTarGz(t *testing.T) {
	// Create a tar.gz in memory
	var buf bytes.Buffer
	gzW := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzW)

	// Add a file
	content := []byte("hello world")
	header := &tar.Header{
		Name: "test-file.txt",
		Mode: 0o644,
		Size: int64(len(content)),
	}
	require.NoError(t, tw.WriteHeader(header))
	_, err := tw.Write(content)
	require.NoError(t, err)

	// Add a directory
	dirHeader := &tar.Header{
		Name:     "subdir/",
		Mode:     0o755,
		Typeflag: tar.TypeDir,
	}
	require.NoError(t, tw.WriteHeader(dirHeader))

	// Add a file in subdir
	subContent := []byte("nested content")
	subHeader := &tar.Header{
		Name: "subdir/nested.txt",
		Mode: 0o644,
		Size: int64(len(subContent)),
	}
	require.NoError(t, tw.WriteHeader(subHeader))
	_, err = tw.Write(subContent)
	require.NoError(t, err)

	tw.Close()
	gzW.Close()

	// Extract to temp dir
	tmpDir := t.TempDir()
	err = extractTarGz(bytes.NewReader(buf.Bytes()), tmpDir)
	require.NoError(t, err)

	// Verify file was extracted
	extracted, err := os.ReadFile(filepath.Join(tmpDir, "test-file.txt"))
	require.NoError(t, err)
	assert.Equal(t, content, extracted)

	// Verify subdir was created
	subDirInfo, err := os.Stat(filepath.Join(tmpDir, "subdir"))
	require.NoError(t, err)
	assert.True(t, subDirInfo.IsDir())

	// Verify nested file
	nested, err := os.ReadFile(filepath.Join(tmpDir, "subdir", "nested.txt"))
	require.NoError(t, err)
	assert.Equal(t, subContent, nested)
}

func TestExtractTarGz_StripsLeadingSlashes(t *testing.T) {
	// Create a tar.gz with absolute paths
	var buf bytes.Buffer
	gzW := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzW)

	content := []byte("absolute path test")
	header := &tar.Header{
		Name: "/absolute/file.txt",
		Mode: 0o644,
		Size: int64(len(content)),
	}
	require.NoError(t, tw.WriteHeader(header))
	_, err := tw.Write(content)
	require.NoError(t, err)

	// Also test ./
	header2 := &tar.Header{
		Name: "./dot-slash/file.txt",
		Mode: 0o644,
		Size: int64(len(content)),
	}
	require.NoError(t, tw.WriteHeader(header2))
	_, err = tw.Write(content)
	require.NoError(t, err)

	tw.Close()
	gzW.Close()

	tmpDir := t.TempDir()
	err = extractTarGz(bytes.NewReader(buf.Bytes()), tmpDir)
	require.NoError(t, err)

	// Verify files were extracted without leading slash issues
	_, err = os.Stat(filepath.Join(tmpDir, "absolute", "file.txt"))
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(tmpDir, "dot-slash", "file.txt"))
	require.NoError(t, err)
}

func TestExtractTarGz_SkipsSymlinks(t *testing.T) {
	var buf bytes.Buffer
	gzW := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzW)

	// Add a symlink
	symHeader := &tar.Header{
		Name:     "link.txt",
		Mode:     0o644,
		Typeflag: tar.TypeSymlink,
		Linkname: "target.txt",
	}
	require.NoError(t, tw.WriteHeader(symHeader))

	tw.Close()
	gzW.Close()

	tmpDir := t.TempDir()
	err := extractTarGz(bytes.NewReader(buf.Bytes()), tmpDir)
	require.NoError(t, err)

	// Verify symlink was not created
	_, err = os.Lstat(filepath.Join(tmpDir, "link.txt"))
	assert.True(t, os.IsNotExist(err), "symlinks should be skipped")
}

func TestDownloadAndExtract_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network test in short mode")
	}

	// This would require a real server to test properly
	// For unit testing, we test the individual components above
	_ = mustParseURL
}

func TestMustParseURL(t *testing.T) {
	u := mustParseURL("https://example.com/path")
	assert.Equal(t, "https", u.Scheme)
	assert.Equal(t, "example.com", u.Host)
	assert.Equal(t, "/path", u.Path)
}

func TestMustParseURL_Panic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			assert.Contains(t, r.(error).Error(), "invalid")
		}
	}()
	mustParseURL("://invalid")
}

// gitInRepo runs a git command inside dir.
//
// Tests must not os.Chdir: the process working directory is global, t.TempDir
// is deleted when the test ends, and nothing restored it — so every test that
// ran afterwards inherited a cwd pointing at a deleted directory, and
// os.Getwd() failed outright. Setting cmd.Dir keeps the effect local.
func gitInRepo(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// Pin the committer identity so the test does not depend on whatever
	// global git config the host happens to have.
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// serveTarball writes the given files into a gzipped tarball, serves it over
// HTTP, and returns the URL and its SHA256 for a [[releases]] entry.
func serveTarball(t *testing.T, dir string, files map[string]string) (url, sha string, hits *atomic.Int64) {
	t.Helper()

	var buf bytes.Buffer
	gzW := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzW)
	for name, content := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o755,
			Size: int64(len(content)),
		}))
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gzW.Close())

	hash := sha256.Sum256(buf.Bytes())
	tarPath := filepath.Join(dir, "release.tar.gz")
	require.NoError(t, os.WriteFile(tarPath, buf.Bytes(), 0o644))

	hits = &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.ServeFile(w, r, tarPath)
	}))
	t.Cleanup(server.Close)

	return server.URL, hex.EncodeToString(hash[:]), hits
}

// commitRepo writes a manifest into a fresh git repo and commits it, so the
// install path can clone it.
func commitRepo(t *testing.T, repoDir, manifest string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(repoDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "tau-package.toml"), []byte(manifest), 0o644))
	gitInRepo(t, repoDir, "init")
	gitInRepo(t, repoDir, "add", ".")
	gitInRepo(t, repoDir, "commit", "-m", "init")
}

// recordReloaders wires both hooks to record what they were called with.
type recordedReloads struct {
	artifact    int
	plugin      int
	pluginNames []string
}

func recordReloaders(mgr *InstallManager) *recordedReloads {
	rec := &recordedReloads{}
	mgr.ArtifactReloader = func(context.Context) error {
		rec.artifact++
		return nil
	}
	mgr.PluginReloader = func(_ context.Context, name string) error {
		rec.plugin++
		rec.pluginNames = append(rec.pluginNames, name)
		return nil
	}
	return rec
}

// These two tests exist to prove notifyReloaders is reached on each install
// path. What the hooks *do* — nil handling, error tolerance, skipping the
// plugin hook for a plugin-less package — is covered by the unit tests in
// reloaders_test.go, which need no git repo, no HTTP server, and no tarball.
//
// There used to be four of these: release/source-build × artifact/plugin, each
// standing up its own repo to assert one bool. Since a single install calls
// both hooks, one test per path covers the same call sites.

func TestInstall_ReleasePathNotifiesReloaders(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "repo")

	url, sha, hits := serveTarball(t, tmpDir, map[string]string{
		"out/artifact.txt": "# Test Artifact",
		"plugin":           "# Test Plugin",
	})

	// The TOML key is [[release]], not [[releases]]. The tests this replaced
	// used the plural, so Releases stayed empty, findRelease returned nil, and
	// the install silently fell through to the clone-as-is path — their tarball
	// and HTTP server were never touched. hits below pins that down.
	commitRepo(t, repoDir, fmt.Sprintf(`[package]
name = "test-release-pkg"
version = "1.0.0"
[plugin]
api_version = "1"
[[release]]
platform = "%s"
url = "%s"
sha256 = "%s"
`, currentPlatform(), url, sha))

	userDir := filepath.Join(tmpDir, "user")
	require.NoError(t, os.MkdirAll(userDir, 0o755))
	mgr := NewInstallManager(userDir, filepath.Join(tmpDir, "project"))
	rec := recordReloaders(mgr)

	require.NoError(t, mgr.Install(context.Background(), Config{
		Source: "file://" + repoDir,
		Yes:    true,
	}))

	assert.Equal(t, 1, rec.artifact, "release install should reload artifacts")
	assert.Equal(t, 1, rec.plugin, "release install of a plugin package should reload the plugin")
	assert.Equal(t, []string{"test-release-pkg"}, rec.pluginNames,
		"PluginReloader should receive the package name")
	assert.Positive(t, hits.Load(), "the release tarball was never downloaded; this is not exercising the release path")
}

func TestInstall_SourceBuildPathNotifiesReloaders(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "repo")

	// No [[releases]] entry, so the install falls back to building from source.
	commitRepo(t, repoDir, `[package]
name = "test-source-build-pkg"
version = "1.0.0"
[plugin]
api_version = "1"
[build]
command = "mkdir -p out && echo 'built' > out/plugin"
`)

	userDir := filepath.Join(tmpDir, "user")
	require.NoError(t, os.MkdirAll(userDir, 0o755))
	mgr := NewInstallManager(userDir, filepath.Join(tmpDir, "project"))
	rec := recordReloaders(mgr)

	require.NoError(t, mgr.Install(context.Background(), Config{
		Source: "file://" + repoDir,
		Yes:    true,
	}))

	assert.Equal(t, 1, rec.artifact, "source-build install should reload artifacts")
	assert.Equal(t, 1, rec.plugin, "source-build install of a plugin package should reload the plugin")
	assert.Equal(t, []string{"test-source-build-pkg"}, rec.pluginNames,
		"PluginReloader should receive the package name")
}

// The local and link install paths need no git, no HTTP server, and no
// tarball — just a directory with a manifest. They were untested before the
// reload block was extracted, because reaching them meant driving a full
// install and nobody wanted a third and fourth copy of that scaffolding.

func TestInstall_LocalPathNotifiesReloaders(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	require.NoError(t, os.MkdirAll(filepath.Join(srcDir, "skills"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "tau-package.toml"), []byte(`[package]
name = "test-local-pkg"
version = "1.0.0"
[plugin]
api_version = "1"
[[skills]]
path = "skills"
`), 0o644))

	userDir := filepath.Join(tmpDir, "user")
	require.NoError(t, os.MkdirAll(userDir, 0o755))
	mgr := NewInstallManager(userDir, filepath.Join(tmpDir, "project"))
	rec := recordReloaders(mgr)

	require.NoError(t, mgr.Install(context.Background(), Config{Source: srcDir, Yes: true}))

	assert.Equal(t, 1, rec.artifact, "local install should reload artifacts")
	assert.Equal(t, []string{"test-local-pkg"}, rec.pluginNames,
		"local install of a plugin package should reload the plugin by name")
}

func TestInstall_LinkPathNotifiesReloaders(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	require.NoError(t, os.MkdirAll(filepath.Join(srcDir, "skills"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "tau-package.toml"), []byte(`[package]
name = "test-link-pkg"
version = "1.0.0"
[plugin]
api_version = "1"
[[skills]]
path = "skills"
`), 0o644))

	userDir := filepath.Join(tmpDir, "user")
	require.NoError(t, os.MkdirAll(userDir, 0o755))
	mgr := NewInstallManager(userDir, filepath.Join(tmpDir, "project"))
	rec := recordReloaders(mgr)

	require.NoError(t, mgr.Install(context.Background(), Config{Source: srcDir, Yes: true, Link: true}))

	// The link path is a separate branch that returns before the copy path's
	// reload call, so it needs its own coverage.
	assert.Equal(t, 1, rec.artifact, "link install should reload artifacts")
	assert.Equal(t, []string{"test-link-pkg"}, rec.pluginNames,
		"link install of a plugin package should reload the plugin by name")

	linkPath := filepath.Join(userDir, "test-link-pkg")
	info, err := os.Lstat(linkPath)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "--link should create a symlink, not a copy")
}

func TestExtractTarGzRejectsPathsOutsidePackage(t *testing.T) {
	for _, name := range []string{"../escape", "nested/../../escape", "./../escape"} {
		t.Run(name, func(t *testing.T) {
			var archive bytes.Buffer
			gz := gzip.NewWriter(&archive)
			tw := tar.NewWriter(gz)
			require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: 1, Typeflag: tar.TypeReg}))
			_, err := tw.Write([]byte("x"))
			require.NoError(t, err)
			require.NoError(t, tw.Close())
			require.NoError(t, gz.Close())
			base := t.TempDir()
			destination := filepath.Join(base, "package")
			require.NoError(t, os.MkdirAll(destination, 0o700))
			require.Error(t, extractTarGz(&archive, destination))
			require.NoFileExists(t, filepath.Join(base, "escape"))
		})
	}
}

func TestExtractTarGzRejectsExistingSymlinkEscape(t *testing.T) {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "redirect/escaped", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg}))
	_, err := tw.Write([]byte("x"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	destination := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(destination, "redirect")))
	require.Error(t, extractTarGz(&archive, destination))
	require.NoFileExists(t, filepath.Join(outside, "escaped"))
}

type releaseTestTransport func(*http.Request) (*http.Response, error)

func (f releaseTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type releaseTestBody struct {
	io.Reader
	closeErr error
}

func (b releaseTestBody) Close() error { return b.closeErr }

func TestDownloadReleaseRetainsBodyCloseFailure(t *testing.T) {
	original := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = original })
	cause := errors.New("release body close failed")
	http.DefaultClient = &http.Client{Transport: releaseTestTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadGateway, Body: releaseTestBody{Reader: strings.NewReader("unavailable"), closeErr: cause}, Header: make(http.Header), Request: req}, nil
	})}
	err := downloadAndExtract(t.Context(), &ReleaseSection{URL: "https://release.invalid/archive"}, t.TempDir())
	require.ErrorContains(t, err, "502")
	require.ErrorIs(t, err, cause)
}

func TestDownloadReleaseRejectsBodyCompletionAfterCancellation(t *testing.T) {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "binary", Mode: 0o700, Size: 2}))
	_, err := tw.Write([]byte("ok"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	hash := sha256.Sum256(archive.Bytes())
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	cause := errors.New("release cancelled")
	original := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = original })
	http.DefaultClient = &http.Client{Transport: releaseTestTransport(func(req *http.Request) (*http.Response, error) {
		cancel(cause)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(archive.Bytes())), Header: make(http.Header), Request: req}, nil
	})}
	destination := filepath.Join(t.TempDir(), "rejected")
	err = downloadAndExtract(ctx, &ReleaseSection{URL: "https://release.invalid/archive", SHA256: hex.EncodeToString(hash[:])}, destination)
	require.ErrorIs(t, err, cause)
	require.NoDirExists(t, destination)
}

func TestDownloadReleaseVerifiesBeforeExtracting(t *testing.T) {
	for _, checksumOK := range []bool{false, true} {
		t.Run(fmt.Sprint(checksumOK), func(t *testing.T) {
			var archive bytes.Buffer
			gz := gzip.NewWriter(&archive)
			tw := tar.NewWriter(gz)
			require.NoError(t, tw.WriteHeader(&tar.Header{Name: "binary", Mode: 0o750, Size: 2}))
			_, err := tw.Write([]byte("ok"))
			require.NoError(t, err)
			require.NoError(t, tw.Close())
			require.NoError(t, gz.Close())
			hash := sha256.Sum256(archive.Bytes())
			checksum := hex.EncodeToString(hash[:])
			if !checksumOK {
				checksum = strings.Repeat("0", 64)
			}
			original := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = original })
			http.DefaultClient = &http.Client{Transport: releaseTestTransport(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(archive.Bytes())), Header: make(http.Header), Request: req}, nil
			})}
			temp := t.TempDir()
			t.Setenv("TMPDIR", temp)
			destination := filepath.Join(t.TempDir(), "release")
			err = downloadAndExtract(t.Context(), &ReleaseSection{URL: "https://release.invalid/archive", SHA256: checksum}, destination)
			if checksumOK {
				require.NoError(t, err)
				body, err := os.ReadFile(filepath.Join(destination, "binary"))
				require.NoError(t, err)
				require.Equal(t, "ok", string(body))
			} else {
				require.ErrorContains(t, err, "hash mismatch")
				require.NoDirExists(t, destination)
			}
			entries, err := os.ReadDir(temp)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

func TestReleaseExtractionRejectsCancelledAdmission(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("extraction cancelled")
	cancel(cause)
	destination := filepath.Join(t.TempDir(), "release")
	err := extractTarGzContext(ctx, strings.NewReader("not an archive"), destination)
	require.ErrorIs(t, err, cause)
	require.NoDirExists(t, destination)
}

type releaseCancellingReader struct {
	reader io.Reader
	cancel context.CancelCauseFunc
	cause  error
}

func (r releaseCancellingReader) Read(data []byte) (int, error) {
	n, err := r.reader.Read(data)
	r.cancel(r.cause)
	return n, err
}

func TestArchiveReaderRetainsReadFailureAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	cause, readErr := errors.New("archive cancelled"), errors.New("archive read failed")
	reader := archiveContextReader{ctx: ctx, reader: releaseCancellingReader{reader: releaseFailingReader{err: readErr}, cancel: cancel, cause: cause}}
	_, err := reader.Read(make([]byte, 10))
	require.ErrorIs(t, err, cause)
	require.ErrorIs(t, err, readErr)
}

type releaseFailingReader struct{ err error }

func (r releaseFailingReader) Read([]byte) (int, error) { return 0, r.err }

func TestReleaseExtractionStopsWhenArchiveReadCancels(t *testing.T) {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "binary", Mode: 0o700, Size: 2}))
	_, err := tw.Write([]byte("ok"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	cause := errors.New("archive read cancelled")
	reader := releaseCancellingReader{reader: bytes.NewReader(archive.Bytes()), cancel: cancel, cause: cause}
	destination := filepath.Join(t.TempDir(), "release")
	err = extractTarGzContext(ctx, reader, destination)
	require.ErrorIs(t, err, cause)
	require.NoFileExists(t, filepath.Join(destination, "binary"))
}

func TestReleaseRejectsOversizedEntryBeforeCreatingFile(t *testing.T) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "oversized", Mode: 0o600, Size: 1024*1024*1024 + 1}))
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, err := gz.Write(raw.Bytes())
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	destination := t.TempDir()
	err = extractTarGz(bytes.NewReader(compressed.Bytes()), destination)
	require.ErrorContains(t, err, "archive expanded size limit")
	require.NoFileExists(t, filepath.Join(destination, "oversized"))
}

func TestArchiveBudgetBoundaries(t *testing.T) {
	budget := archiveBudget{expanded: maxArchiveExpanded - 4, entries: maxArchiveEntries - 2}
	require.NoError(t, budget.admit(&tar.Header{Size: 4}))
	require.ErrorContains(t, budget.admit(&tar.Header{Size: 1}), "expanded size limit")
	require.Equal(t, maxArchiveExpanded, budget.expanded)
	require.ErrorContains(t, budget.admit(&tar.Header{}), "entry limit")
	budget = archiveBudget{}
	require.ErrorContains(t, budget.admit(&tar.Header{Size: -1}), "expanded size limit")
	budget = archiveBudget{entries: maxArchiveEntries - 1}
	require.NoError(t, budget.admit(&tar.Header{Typeflag: tar.TypeDir}))
	require.ErrorContains(t, budget.admit(&tar.Header{Typeflag: tar.TypeDir}), "entry limit")
}

func TestArchiveStreamLimitBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, input string
		limit       int64
		fails       bool
	}{
		{"below", "abc", 4, false}, {"exact", "abcd", 4, false}, {"above", "abcde", 4, true}, {"empty", "", 0, false}, {"zero", "a", 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &archiveLimitReader{reader: strings.NewReader(test.input), remaining: test.limit}
			n, err := reader.Read(nil)
			require.Zero(t, n)
			require.NoError(t, err)
			data, err := io.ReadAll(reader)
			if test.fails {
				require.ErrorContains(t, err, "archive stream size limit")
			} else {
				require.NoError(t, err)
				require.Equal(t, test.input, string(data))
			}
			require.LessOrEqual(t, int64(len(data)), test.limit)
		})
	}
	cause := errors.New("stream failed")
	reader := &archiveLimitReader{reader: releaseFailingReader{err: cause}}
	_, err := io.ReadAll(reader)
	require.ErrorIs(t, err, cause)
}

func TestReleaseRejectsEntryFlood(t *testing.T) {
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gz)
	for range maxArchiveEntries + 1 {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: "directory", Typeflag: tar.TypeDir, Mode: 0o700}))
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	err := extractTarGz(bytes.NewReader(compressed.Bytes()), t.TempDir())
	require.ErrorContains(t, err, "archive entry limit")
}

func TestReleaseRejectsCorruptGzipTrailer(t *testing.T) {
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "file", Mode: 0o600, Size: 2}))
	_, err := tw.Write([]byte("ok"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	data := compressed.Bytes()
	data[len(data)-8] ^= 0xff
	err = extractTarGz(bytes.NewReader(data), t.TempDir())
	require.ErrorIs(t, err, gzip.ErrChecksum)
}

func TestDownloadRejectsDeclaredArchiveSizeBeforeReading(t *testing.T) {
	original := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = original })
	readFailure := errors.New("oversized response was read")
	closeFailure := errors.New("oversized response close failed")
	http.DefaultClient = &http.Client{Transport: releaseTestTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: maxArchiveDownload + 1, Body: releaseTestBody{Reader: releaseFailingReader{err: readFailure}, closeErr: closeFailure}, Header: make(http.Header), Request: req}, nil
	})}
	destination := filepath.Join(t.TempDir(), "rejected")
	err := downloadAndExtract(t.Context(), &ReleaseSection{URL: "https://release.invalid/archive"}, destination)
	require.ErrorContains(t, err, "archive download size limit")
	require.ErrorIs(t, err, closeFailure)
	require.NotErrorIs(t, err, readFailure)
	require.NoDirExists(t, destination)
}
