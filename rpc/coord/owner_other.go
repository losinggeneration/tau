//go:build !unix

package coord

import "os"

// fileOwner cannot verify ownership here, so private directories are refused.
func fileOwner(os.FileInfo) (int, bool) {
	return 0, false
}
