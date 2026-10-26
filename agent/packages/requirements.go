package packages

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
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
	return CheckRuntimeRequirementContext(context.Background(), req)
}

// CheckRuntimeRequirementContext bounds the runtime probe by its caller and a short deadline.
func CheckRuntimeRequirementContext(ctx context.Context, req string) error {
	return checkRuntimeRequirement(ctx, req, getRuntimeVersion)
}

func checkRuntimeRequirement(ctx context.Context, req string, probe func(context.Context, string) (string, error)) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
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
	for _, operator := range []string{">=", "<=", "==", ">", "<", "="} {
		if value, ok := strings.CutPrefix(constraint, operator); ok {
			if _, valid := parseRequirementVersion(value); !valid {
				return fmt.Errorf("invalid runtime version constraint %q", constraint)
			}
			break
		}
	}

	// Find and execute the runtime to get its version
	version, err := probe(ctx, runtime)
	if cause := context.Cause(ctx); cause != nil {
		return errors.Join(err, cause)
	}
	if err != nil {
		return errors.Join(err, &RequirementCheckError{Runtime: runtime, NotFound: true})
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
	operators := []string{">=", "<=", "==", ">", "<", "="}

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
const runtimeVersionOutputLimit = 64 * 1024

func getRuntimeVersion(ctx context.Context, runtime string) (string, error) {
	return probeRuntimeVersion(ctx, runtime, func(cmd *exec.Cmd) error { return cmd.Run() })
}

func probeRuntimeVersion(ctx context.Context, runtime string, run func(*exec.Cmd) error) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
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

	cmd := exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
	cmd.WaitDelay = time.Second
	var output runtimeVersionOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := run(cmd)
	if cause := context.Cause(ctx); cause != nil {
		return "", errors.Join(err, cause)
	}
	if err != nil {
		return "", fmt.Errorf("failed to get version: %w", err)
	}

	// Extract version from output
	version := extractVersion(output.String())
	if version == "" {
		return "", fmt.Errorf("could not parse runtime version")
	}

	return version, nil
}

type runtimeVersionOutput struct{ buffer strings.Builder }

func (output *runtimeVersionOutput) Len() int       { return output.buffer.Len() }
func (output *runtimeVersionOutput) String() string { return output.buffer.String() }

func (output *runtimeVersionOutput) Write(data []byte) (int, error) {
	if len(data) > runtimeVersionOutputLimit-output.Len() {
		return 0, fmt.Errorf("runtime version output exceeded limit")
	}
	return output.buffer.Write(data)
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
	var op, required string
	for _, candidate := range []string{">=", "<=", "==", ">", "<", "="} {
		if value, ok := strings.CutPrefix(constraint, candidate); ok {
			op = candidate
			required = value
			break
		}
	}

	vNums, valid := parseRequirementVersion(version)
	if !valid {
		return false
	}
	rNums, valid := parseRequirementVersion(required)
	if !valid {
		return false
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

func parseRequirementVersion(value string) ([]int, bool) {
	parts := strings.Split(value, ".")
	if len(parts) > 3 {
		return nil, false
	}
	numbers := make([]int, 3)
	for i, part := range parts {
		if part == "" {
			return nil, false
		}
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return nil, false
			}
		}
		number, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		numbers[i] = number
	}
	return numbers, true
}
