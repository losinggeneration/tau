// Package atref expands @path references in prompt text. Expansion always
// runs where the files live, so a coordinated worker on another machine
// resolves references against its own filesystem.
package atref

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/losinggeneration/tau/sdk"
)

// IsBoundary reports whether r ends an @path token or may precede one.
func IsBoundary(r rune) bool {
	return r == ' ' || r == '\t' || r == '(' || r == '[' || r == '{' || r == ',' || r == ':'
}

// Replace calls fn with the path of every @path token in text and substitutes
// its result for the token. An @ escaped with a backslash or glued to a word
// is left alone.
func Replace(text string, fn func(path string) (string, error)) (string, error) {
	var out strings.Builder
	for i := 0; i < len(text); {
		if text[i] != '@' || (i > 0 && (text[i-1] == '\\' || !IsBoundary(rune(text[i-1])))) {
			out.WriteByte(text[i])
			i++
			continue
		}
		j := i + 1
		for j < len(text) && !IsBoundary(rune(text[j])) && text[j] != '\n' {
			j++
		}
		if j == i+1 {
			out.WriteByte(text[i])
			i++
			continue
		}
		replacement, err := fn(text[i+1 : j])
		if err != nil {
			return "", err
		}
		out.WriteString(replacement)
		i = j
	}
	return out.String(), nil
}

// Resolve joins path with root. Absolute and ~/ paths bypass root so
// @/etc/hosts and @~/notes resolve the way the user typed them.
func Resolve(path, root string) string {
	if path == "" {
		return root
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, filepath.FromSlash(path[2:]))
		}
	}
	if filepath.IsAbs(path) {
		return filepath.FromSlash(path)
	}
	if root == "" {
		root = "."
	}
	return filepath.Join(root, filepath.FromSlash(path))
}

// Directory names a directory without listing its contents. Dumping a tree
// into the prompt would flood the context window; the model can explore the
// absolute path itself.
func Directory(path, full string) (string, error) {
	abs, err := filepath.Abs(full)
	if err != nil {
		return "", fmt.Errorf("@%s: resolve: %w", path, err)
	}
	return fmt.Sprintf("<directory name=%q>\n</directory>", abs), nil
}

// Attach expands every reference in text, importing files through
// importFile. resolve maps a token to a path on disk. Missing paths stay as
// literal text; files that can't be attached are an error.
func Attach(text string, resolve func(path string) string, importFile func(full string) (sdk.Attachment, error)) (string, []sdk.Attachment, error) {
	var records []sdk.Attachment
	expanded, err := Replace(text, func(path string) (string, error) {
		full := resolve(path)
		info, err := os.Stat(full)
		if os.IsNotExist(err) {
			return "@" + path, nil
		}
		if err != nil {
			return "", fmt.Errorf("@%s: %w", path, err)
		}
		if info.IsDir() {
			return Directory(path, full)
		}
		record, err := importFile(full)
		if err != nil {
			return "", fmt.Errorf("@%s: %w", path, err)
		}
		if record.Kind != "image" && record.ExtractedText == "" {
			return "", fmt.Errorf("@%s: unsupported attachment type %s", path, record.MimeType)
		}
		records = append(records, record)
		return Placeholder(path, record), nil
	})
	if err != nil {
		return "", nil, err
	}
	return expanded, records, nil
}

// Placeholder is the prompt text that stands in for an attached file; the
// attachment itself carries the content.
func Placeholder(path string, record sdk.Attachment) string {
	if record.Kind == "image" {
		return fmt.Sprintf("<file name=%q>[image content]</file>", path)
	}
	return fmt.Sprintf("<file name=%q>[attached]</file>", path)
}
