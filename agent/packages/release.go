package packages

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// Download the tarball
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, release.URL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download release: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close response body: %w", closeErr)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	// Verify sha256
	if err := verifySHA256(data, release.SHA256); err != nil {
		return fmt.Errorf("sha256 verification failed: %w", err)
	}

	// Extract tarball
	if err := extractTarGz(bytes.NewReader(data), destDir); err != nil {
		return fmt.Errorf("extract tarball: %w", err)
	}

	return nil
}

// verifySHA256 verifies that the data matches the expected sha256 hash.
// Supports hex-encoded hash format.
func verifySHA256(data []byte, expected string) error {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return fmt.Errorf("empty sha256")
	}

	hash := sha256.Sum256(data)
	actual := hex.EncodeToString(hash[:])

	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("hash mismatch: got %s, want %s", actual, expected)
	}
	return nil
}

// extractTarGz extracts a tar.gz archive to destDir.
func extractTarGz(r io.Reader, destDir string) (err error) {
	gzR, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("create gzip reader: %w", err)
	}
	defer func() {
		if closeErr := gzR.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close gzip reader: %w", closeErr)
		}
	}()

	tr := tar.NewReader(gzR)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar header: %w", err)
		}

		target := strings.TrimPrefix(header.Name, "/")
		target = strings.TrimPrefix(target, "./")
		if target == "" {
			continue
		}

		targetPath := filepath.Join(destDir, target)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, os.FileMode(header.Mode)); err != nil {
				return fmt.Errorf("create dir %s: %w", targetPath, err)
			}
		case tar.TypeReg:
			// Create parent directories
			if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
				return fmt.Errorf("create parent dir for %s: %w", targetPath, err)
			}

			f, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return fmt.Errorf("create file %s: %w", targetPath, err)
			}

			if _, err := io.Copy(f, tr); err != nil {
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

	return nil
}

func mustParseURL(rawURL string) *url.URL {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}
	return u
}
