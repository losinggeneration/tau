package packages

import (
	"testing"
)

func TestParseNpmSpec(t *testing.T) {
	tests := []struct {
		name     string
		spec     string
		expected NpmSpec
	}{
		{
			name: "simple package name",
			spec: "lodash",
			expected: NpmSpec{
				Name:    "lodash",
				Version: "",
				Pinned:  false,
			},
		},
		{
			name: "package with version",
			spec: "lodash@4.0.0",
			expected: NpmSpec{
				Name:    "lodash",
				Version: "4.0.0",
				Pinned:  true,
			},
		},
		{
			name: "package with latest",
			spec: "lodash@latest",
			expected: NpmSpec{
				Name:    "lodash",
				Version: "latest",
				Pinned:  true,
			},
		},
		{
			name: "scoped package",
			spec: "@types/node",
			expected: NpmSpec{
				Name:    "@types/node",
				Version: "",
				Pinned:  false,
			},
		},
		{
			name: "scoped package with version",
			spec: "@types/node@18.0.0",
			expected: NpmSpec{
				Name:    "@types/node",
				Version: "18.0.0",
				Pinned:  true,
			},
		},
		{
			name: "simple package with whitespace",
			spec: "  lodash  ",
			expected: NpmSpec{
				Name:    "lodash",
				Version: "",
				Pinned:  false,
			},
		},
		{
			name: "monorepo package",
			spec: "@babel/core@7.22.0",
			expected: NpmSpec{
				Name:    "@babel/core",
				Version: "7.22.0",
				Pinned:  true,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := parseNpmSpec(tc.spec)
			if result.Name != tc.expected.Name {
				t.Errorf("Name: got %q, want %q", result.Name, tc.expected.Name)
			}
			if result.Version != tc.expected.Version {
				t.Errorf("Version: got %q, want %q", result.Version, tc.expected.Version)
			}
			if result.Pinned != tc.expected.Pinned {
				t.Errorf("Pinned: got %v, want %v", result.Pinned, tc.expected.Pinned)
			}
		})
	}
}
