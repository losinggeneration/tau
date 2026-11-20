//go:build !unix

package coord

import (
	"fmt"
	"os"
)

func executableBuildID(info os.FileInfo) string {
	return fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
}
