package plugins

import (
	"crypto/md5"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// computeHash computes the MD5 hash of a file.
func (m *Manager) computeHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		if closeErr := f.Close(); closeErr != nil {
			return "", fmt.Errorf("copy file %s: %w; close file: %w", path, err, closeErr)
		}
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close file %s: %w", path, err)
	}

	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func computeManagedShimHash(manifestPath string, manifest *Manifest) (string, error) {
	if manifestPath == "" || manifest == nil || !manifest.HasRunCommand() {
		return "", fmt.Errorf("missing manifest")
	}
	hash, err := hashFiles(filepath.Dir(manifestPath), []string{manifestPath})
	if err != nil {
		return "", err
	}
	args := manifest.Run.args
	if args == nil {
		args = splitCommand(manifest.Run.Command)
	}
	return fmt.Sprintf("%x", md5.Sum([]byte(hash+"\x00"+strings.Join(args, "\x00")))), nil
}

func hashFiles(root string, paths []string) (string, error) {
	h := md5.New()
	for _, p := range paths {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return "", fmt.Errorf("rel %s: %w", p, err)
		}
		if _, err := h.Write([]byte(rel + "\x00")); err != nil {
			return "", fmt.Errorf("hash path %s: %w", rel, err)
		}
		f, err := os.Open(p)
		if err != nil {
			return "", fmt.Errorf("open %s: %w", p, err)
		}
		if _, err := io.Copy(h, f); err != nil {
			closeErr := f.Close()
			if closeErr != nil {
				return "", fmt.Errorf("copy %s: %w; close: %w", p, err, closeErr)
			}
			return "", fmt.Errorf("copy %s: %w", p, err)
		}
		if err := f.Close(); err != nil {
			return "", fmt.Errorf("close %s: %w", p, err)
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// computeDirHash hashes a directory tree deterministically (sorted relative
// paths + file contents). Used for generic run-command plugins where hashing
// the interpreter binary would miss plugin file changes and defeat reload detection.
func computeDirHash(root string) (string, error) {
	h := md5.New()
	var paths []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip vendored/heavy subtrees that shouldn't affect identity.
			if d.Name() == "node_modules" || d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk %s: %w", root, err)
	}
	sort.Strings(paths)
	for _, p := range paths {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return "", fmt.Errorf("rel %s: %w", p, err)
		}
		if _, err := h.Write([]byte(rel + "\x00")); err != nil {
			return "", fmt.Errorf("hash path %s: %w", rel, err)
		}
		f, err := os.Open(p)
		if err != nil {
			return "", fmt.Errorf("open %s: %w", p, err)
		}
		if _, err := io.Copy(h, f); err != nil {
			closeErr := f.Close()
			if closeErr != nil {
				return "", fmt.Errorf("copy %s: %w; close: %w", p, err, closeErr)
			}
			return "", fmt.Errorf("copy %s: %w", p, err)
		}
		if err := f.Close(); err != nil {
			return "", fmt.Errorf("close %s: %w", p, err)
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
