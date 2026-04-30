package packages

import (
	"os"
	"testing"
)

func TestIsOfflineMode(t *testing.T) {
	tests := []struct {
		name   string
		envVal string
		expect bool
	}{
		{name: "empty", envVal: "", expect: false},
		{name: "1", envVal: "1", expect: true},
		{name: "true lowercase", envVal: "true", expect: true},
		{name: "TRUE uppercase", envVal: "TRUE", expect: true},
		{name: "yes lowercase", envVal: "yes", expect: true},
		{name: "YES uppercase", envVal: "YES", expect: true},
		{name: "false", envVal: "false", expect: false},
		{name: "0", envVal: "0", expect: false},
		{name: "no", envVal: "no", expect: false},
		{name: "random", envVal: "random", expect: false},
		{name: " with spaces", envVal: "  true  ", expect: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envVal == "" {
				os.Unsetenv("PI_OFFLINE")
			} else {
				os.Setenv("PI_OFFLINE", tc.envVal)
			}
			defer os.Unsetenv("PI_OFFLINE")

			got := IsOfflineMode()
			if got != tc.expect {
				t.Errorf("IsOfflineMode() = %v, want %v", got, tc.expect)
			}
		})
	}
}
