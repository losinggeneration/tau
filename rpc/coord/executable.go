package coord

import "os"

// ExecutableBuildID identifies an executable file on this host without reading
// it into memory. An empty ID means its identity could not be determined.
func ExecutableBuildID(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return executableBuildID(info)
}
