package piext

import (
	"os"
	"strings"
)

func isOfflineMode() bool {
	val := os.Getenv("PI_OFFLINE")
	if val == "" {
		return false
	}
	val = strings.ToLower(strings.TrimSpace(val))
	return val == "1" || val == "true" || val == "yes"
}
