package packages

import (
	"path/filepath"
	"strings"
)

// isHidden checks if a path is hidden (starts with . or contains . components).
func isHidden(path string) bool {
	// Normalize path separators
	path = filepath.ToSlash(path)
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}
