package packages

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// currentPlatform returns the current platform in the format "os/arch".
func currentPlatform() string {
	return fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
}

// findRelease finds a release matching the current platform from the manifest.
// Returns nil if no matching release is found.
func findRelease(manifest *Manifest) *ReleaseSection {
	platform := currentPlatform()
	for i := range manifest.Releases {
		if manifest.Releases[i].Platform == platform {
			return &manifest.Releases[i]
		}
	}
	return nil
}

// downloadAndExtract downloads a release tarball, verifies its sha256, and extracts it.
func downloadAndExtract(ctx context.Context, release *ReleaseSection, destDir string) (err error) {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	// Download the tarball
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, release.URL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download release: %w", errors.Join(err, context.Cause(ctx)))
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close response body: %w", closeErr))
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	if resp.ContentLength > maxArchiveDownload {
		return errors.New("archive download size limit exceeded")
	}

	archive, err := os.CreateTemp("", "tau-release-*.tar.gz")
	if err != nil {
		return fmt.Errorf("create release archive: %w", err)
	}
	defer func() {
		err = errors.Join(err, archive.Close())
		if removeErr := os.Remove(archive.Name()); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(archive, hash), archiveContextReader{ctx: ctx, reader: &archiveLimitReader{reader: resp.Body, remaining: maxArchiveDownload}}); err != nil {
		return fmt.Errorf("read release archive: %w", errors.Join(err, context.Cause(ctx)))
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if err := verifySHA256Digest(hex.EncodeToString(hash.Sum(nil)), release.SHA256); err != nil {
		return fmt.Errorf("sha256 verification failed: %w", err)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind release archive: %w", err)
	}
	if err := extractTarGzContext(ctx, archive, destDir); err != nil {
		return fmt.Errorf("extract tarball: %w", err)
	}

	return nil
}

// verifySHA256 verifies that the data matches the expected sha256 hash.
// Supports hex-encoded hash format.
func verifySHA256(data []byte, expected string) error {
	hash := sha256.Sum256(data)
	return verifySHA256Digest(hex.EncodeToString(hash[:]), expected)
}

func verifySHA256Digest(actual, expected string) error {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return fmt.Errorf("empty sha256")
	}

	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("hash mismatch: got %s, want %s", actual, expected)
	}
	return nil
}

// extractTarGz extracts a tar.gz archive to destDir.
func extractTarGz(r io.Reader, destDir string) error {
	return extractTarGzContext(context.Background(), r, destDir)
}

func extractTarGzContext(ctx context.Context, r io.Reader, destDir string) (err error) {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return fmt.Errorf("create release root: %w", err)
	}
	root, err := os.OpenRoot(destDir)
	if err != nil {
		return fmt.Errorf("open release root: %w", err)
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	gzR, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("create gzip reader: %w", err)
	}
	defer func() {
		if closeErr := gzR.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close gzip reader: %w", closeErr))
		}
	}()

	expanded := archiveContextReader{ctx: ctx, reader: &archiveLimitReader{reader: gzR, remaining: maxArchiveExpanded + maxArchiveMetadata}}
	tr := tar.NewReader(expanded)
	var budget archiveBudget
	for {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar header: %w", err)
		}

		if err := budget.admit(header); err != nil {
			return err
		}

		target := filepath.FromSlash(strings.TrimPrefix(header.Name, "/"))
		if !filepath.IsLocal(target) {
			return fmt.Errorf("release archive path escapes package: %q", header.Name)
		}
		target = filepath.Clean(target)
		targetPath := target

		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(targetPath, os.FileMode(header.Mode)&0o777); err != nil {
				return fmt.Errorf("create dir %s: %w", targetPath, err)
			}
		case tar.TypeReg:
			// Create parent directories
			if err := root.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
				return fmt.Errorf("create parent dir for %s: %w", targetPath, err)
			}

			f, err := root.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode)&0o777)
			if err != nil {
				return fmt.Errorf("create file %s: %w", targetPath, err)
			}

			if _, err := io.Copy(f, archiveContextReader{ctx: ctx, reader: tr}); err != nil {
				if closeErr := f.Close(); closeErr != nil {
					return fmt.Errorf("write file %s: %w; close file: %w", targetPath, err, closeErr)
				}
				return fmt.Errorf("write file %s: %w", targetPath, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("close file %s: %w", targetPath, err)
			}
		case tar.TypeSymlink:
			// Skip symlinks for safety
			continue
		}
	}

	if _, err := io.Copy(io.Discard, expanded); err != nil {
		return fmt.Errorf("validate archive trailer: %w", err)
	}

	return nil
}

func mustParseURL(rawURL string) *url.URL {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}
	return u
}

type archiveContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r archiveContextReader) Read(data []byte) (int, error) {
	if err := context.Cause(r.ctx); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(data)
	if cause := context.Cause(r.ctx); cause != nil {
		return n, errors.Join(err, cause)
	}
	return n, err
}

const (
	maxArchiveDownload int64 = 256 * 1024 * 1024
	maxArchiveExpanded int64 = 1024 * 1024 * 1024
	maxArchiveMetadata int64 = 64 * 1024 * 1024
	maxArchiveEntries        = 100000
)

type archiveBudget struct {
	entries  int
	expanded int64
}

func (b *archiveBudget) admit(header *tar.Header) error {
	if b.entries >= maxArchiveEntries {
		return errors.New("archive entry limit exceeded")
	}
	b.entries++
	if header.Size < 0 || header.Size > maxArchiveExpanded-b.expanded {
		return errors.New("archive expanded size limit exceeded")
	}
	b.expanded += header.Size
	return nil
}

type archiveLimitReader struct {
	reader    io.Reader
	remaining int64
}

func (r *archiveLimitReader) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		var probe [1]byte
		n, err := r.reader.Read(probe[:])
		if n > 0 {
			return 0, errors.Join(errors.New("archive stream size limit exceeded"), err)
		}
		return 0, err
	}
	if int64(len(data)) > r.remaining {
		data = data[:r.remaining]
	}
	n, err := r.reader.Read(data)
	r.remaining -= int64(n)
	return n, err
}
