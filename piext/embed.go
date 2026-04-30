package piext

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed shim/index.js shim/package.json shim/package-lock.json shim/proto/plugin.proto shim/src
var shimFS embed.FS

// EmbeddedPackageJSON returns the embedded shim package.json.
func EmbeddedPackageJSON() []byte {
	data, err := shimFS.ReadFile("shim/package.json")
	if err != nil {
		panic(fmt.Sprintf("embedded shim package.json: %v", err))
	}
	return append([]byte(nil), data...)
}

func EmbeddedShimHash() string {
	h := sha256.New()
	if err := fs.WalkDir(shimFS, "shim", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := shimFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", path, err)
		}
		if _, err := fmt.Fprintf(h, "%s\x00", path); err != nil {
			return fmt.Errorf("hash path %s: %w", path, err)
		}
		if _, err := h.Write(data); err != nil {
			return fmt.Errorf("hash embedded %s: %w", path, err)
		}
		return nil
	}); err != nil {
		panic(fmt.Sprintf("hash embedded shim: %v", err))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// WriteShimFilesystem writes the embedded shim tree to dst.
func WriteShimFilesystem(dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return fmt.Errorf("create shim dir: %w", err)
	}
	if err := fs.WalkDir(shimFS, "shim", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, "shim")
		if rel == "" {
			return nil
		}
		out := filepath.Join(dst, filepath.FromSlash(rel))
		if d.IsDir() {
			if err := os.MkdirAll(out, 0o700); err != nil {
				return fmt.Errorf("create %s: %w", out, err)
			}
			return nil
		}
		data, err := shimFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", path, err)
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
			return fmt.Errorf("create parent for %s: %w", out, err)
		}
		if err := os.WriteFile(out, data, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", out, err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("write shim filesystem: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dst, ".tau-shim-hash"), []byte(EmbeddedShimHash()+"\n"), 0o600); err != nil {
		return fmt.Errorf("write shim hash: %w", err)
	}
	return nil
}
