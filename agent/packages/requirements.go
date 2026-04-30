package packages

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// RequirementCheckError represents an error checking a runtime requirement.
type RequirementCheckError struct {
	Runtime  string
	Found    string
	Required string
	NotFound bool
}

func (e *RequirementCheckError) Error() string {
	if e.NotFound {
		return fmt.Sprintf("runtime %q is required but not found", e.Runtime)
	}
	return fmt.Sprintf("%s version %s does not satisfy requirement %s",
		e.Runtime, e.Found, e.Required)
}

// CheckRuntimeRequirement checks if the runtime requirement is satisfied.
// The requirement string format is: <runtime>>=<version>
// Examples: "python>=3.11", "node>=18.0", "go>=1.21"
//
// This function returns nil if the requirement is empty or satisfied,
// or an error if the runtime is not found or does not meet the version constraint.
func CheckRuntimeRequirement(req string) error {
	if req == "" {
		return nil
	}

	req = strings.TrimSpace(req)
	if req == "" {
		return nil
	}

	// Parse the requirement string
	runtime, constraint, err := parseRequirement(req)
	if err != nil {
		return err
	}

	// Find and execute the runtime to get its version
	version, err := getRuntimeVersion(runtime)
	if err != nil {
		return &RequirementCheckError{
			Runtime:  runtime,
			NotFound: true,
		}
	}

	// Check if version satisfies constraint
	if !satisfiesConstraint(version, constraint) {
		return &RequirementCheckError{
			Runtime:  runtime,
			Found:    version,
			Required: req,
		}
	}

	return nil
}

// parseRequirement parses a requirement string into runtime name and version constraint.
// Format: <runtime>>=<version> or <runtime> <version>
func parseRequirement(req string) (runtime string, constraint string, err error) {
	// Supported operators
	operators := []string{">=", "<=", ">", "<", "=", "=="}

	var op string
	var version string

	// Find the operator
	for _, o := range operators {
		if before, after, found := strings.Cut(req, o); found {
			op = o
			runtime = strings.TrimSpace(before)
			version = strings.TrimSpace(after)
			break
		}
	}

	if op == "" {
		return "", "", fmt.Errorf("no operator found (expected >=, <=, >, <, =, or ==)")
	}

	if runtime == "" {
		return "", "", fmt.Errorf("missing runtime name")
	}

	if version == "" {
		return "", "", fmt.Errorf("missing version")
	}

	return runtime, op + version, nil
}

// runtimeVersionCommand maps runtime names to their version flag.
var runtimeVersionCommand = map[string][]string{
	"python":  {"python3", "--version"},
	"python2": {"python2", "--version"},
	"node":    {"node", "--version"},
	"nodejs":  {"node", "--version"},
	"go":      {"go", "version"},
	"ruby":    {"ruby", "--version"},
	"perl":    {"perl", "--version"},
	"rust":    {"rustc", "--version"},
	"cargo":   {"cargo", "--version"},
	"java":    {"java", "-version"},
	"lua":     {"lua", "-v"},
	"php":     {"php", "--version"},
}

// versionRegex extracts version numbers from command output.
var versionRegex = regexp.MustCompile(`(\d+)(?:\.(\d+))?(?:\.(\d+))?`)

// getRuntimeVersion executes the runtime and returns its version string.
func getRuntimeVersion(runtime string) (string, error) {
	// Build command for the runtime
	var cmdArgs []string
	if args, ok := runtimeVersionCommand[runtime]; ok {
		cmdArgs = args
	} else {
		// Try using the runtime name directly with --version
		cmdArgs = []string{runtime, "--version"}
	}

	if len(cmdArgs) < 2 {
		return "", fmt.Errorf("cannot determine version command for %q", runtime)
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get version: %w", err)
	}

	// Extract version from output
	version := extractVersion(string(output))
	if version == "" {
		return "", fmt.Errorf("could not parse version from output: %s", strings.TrimSpace(string(output)))
	}

	return version, nil
}

// extractVersion extracts the first semantic version from the output.
func extractVersion(output string) string {
	matches := versionRegex.FindStringSubmatch(output)
	if matches == nil {
		return ""
	}

	major := matches[1]
	minor := "0"
	patch := "0"

	if len(matches) > 2 && matches[2] != "" {
		minor = matches[2]
	}
	if len(matches) > 3 && matches[3] != "" {
		patch = matches[3]
	}

	return fmt.Sprintf("%s.%s.%s", major, minor, patch)
}

// satisfiesConstraint checks if the given version satisfies the constraint.
func satisfiesConstraint(version, constraint string) bool {
	// Parse the constraint (e.g., ">=3.11", "==1.21", ">2.0")
	if len(constraint) < 2 {
		return false
	}

	op := constraint[:2]
	required := constraint[2:]

	if len(constraint) == 3 && (constraint[0:1] == "=" || constraint[0:1] == ">" || constraint[0:1] == "<") {
		op = constraint[0:1]
		required = constraint[1:]
	}

	// Parse versions
	vParts := strings.Split(version, ".")
	rParts := strings.Split(required, ".")

	vNums := make([]int, 3)
	rNums := make([]int, 3)

	for i := range min(len(vParts), 3) {
		vNums[i], _ = strconv.Atoi(vParts[i])
	}
	for i := range min(len(rParts), 3) {
		rNums[i], _ = strconv.Atoi(rParts[i])
	}

	switch op {
	case ">=":
		return compareVersions(vNums, rNums) >= 0
	case "<=":
		return compareVersions(vNums, rNums) <= 0
	case ">":
		return compareVersions(vNums, rNums) > 0
	case "<":
		return compareVersions(vNums, rNums) < 0
	case "=", "==":
		return compareVersions(vNums, rNums) == 0
	default:
		return false
	}
}

// compareVersions compares two version number slices.
// Returns -1 if v < r, 0 if v == r, 1 if v > r.
func compareVersions(v, r []int) int {
	maxLen := len(v)
	if len(r) > maxLen {
		maxLen = len(r)
	}

	// Extend slices to same length with zeros
	for i := len(v); i < maxLen; i++ {
		v = append(v, 0)
	}
	for i := len(r); i < maxLen; i++ {
		r = append(r, 0)
	}

	for i := 0; i < maxLen; i++ {
		if v[i] < r[i] {
			return -1
		}
		if v[i] > r[i] {
			return 1
		}
	}
	return 0
}
