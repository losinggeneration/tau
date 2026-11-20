//go:build unix

package coord

import (
	"fmt"
	"os"
	"syscall"
)

func executableBuildID(info os.FileInfo) string {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d:%d:%d", stat.Dev, stat.Ino, info.ModTime().UnixNano(), info.Size())
	}
	return fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
}
