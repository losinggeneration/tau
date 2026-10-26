package packages

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRuntimeRequirementOperatorsAndInvalidVersions(t *testing.T) {
	for _, test := range []struct {
		version, constraint string
		want                bool
	}{
		{"3.11.2", ">3.10", true},
		{"3.11.2", "<3.12", true},
		{"3.11.2", "=3.11.2", true},
		{"3.11.2", "==3.11.2", true},
		{"3.11.2", ">=3.11", true},
		{"3.11.2", "<=3.11.2", true},
		{"3.11.2", ">3.11.2", false},
		{"3.11.2", ">=banana", false},
		{"garbage", ">=0", false},
		{"3.11.2", ">=3.bad", false},
		{"3.11.2", ">=3.11.2.9", false},
		{"3.11.2", ">=9999999999999999999999999999999", false},
	} {
		t.Run(test.version+test.constraint, func(t *testing.T) { require.Equal(t, test.want, satisfiesConstraint(test.version, test.constraint)) })
	}
	runtime, constraint, err := parseRequirement("node==18.0")
	require.NoError(t, err)
	require.Equal(t, "node", runtime)
	require.Equal(t, "==18.0", constraint)
}

func TestRuntimeRequirementProbeHonorsCancellationAndCauses(t *testing.T) {
	cause := errors.New("cancel runtime probe")
	failure := errors.New("runtime lookup failed")
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(cause)
	calls := 0
	probe := func(context.Context, string) (string, error) { calls++; return "18.0.0", nil }
	require.ErrorIs(t, checkRuntimeRequirement(ctx, "node>=18", probe), cause)
	require.Zero(t, calls)
	for _, outcome := range []error{nil, failure} {
		ctx, cancel := context.WithCancelCause(t.Context())
		err := checkRuntimeRequirement(ctx, "node>=18", func(context.Context, string) (string, error) { cancel(cause); return "18.0.0", outcome })
		require.ErrorIs(t, err, cause)
		if outcome != nil {
			require.ErrorIs(t, err, outcome)
		}
	}
	err := checkRuntimeRequirement(t.Context(), "node>=18", func(context.Context, string) (string, error) { return "", failure })
	require.ErrorIs(t, err, failure)
	var requirement *RequirementCheckError
	require.ErrorAs(t, err, &requirement)
	require.True(t, requirement.NotFound)
}

func TestRuntimeRequirementProbeValidatesReturnedVersion(t *testing.T) {
	for _, test := range []struct {
		req, version string
		wantError    bool
	}{
		{"node>=18", "20.0.0", false},
		{"node>18.0", "18.0.0", true},
		{"node==18.0", "18.0.0", false},
		{"node>=18", "invalid", true},
		{"node>=18", "17.9.0", true},
	} {
		t.Run(test.req+test.version, func(t *testing.T) {
			err := checkRuntimeRequirement(t.Context(), test.req, func(ctx context.Context, name string) (string, error) {
				require.Equal(t, "node", name)
				return test.version, nil
			})
			if test.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestInvalidRequirementRejectsBeforeRuntimeProbe(t *testing.T) {
	for _, req := range []string{"node>=banana", "node>=18.bad", "node==18.0.0.1", "node>=999999999999999999999999999999"} {
		t.Run(req, func(t *testing.T) {
			calls := 0
			err := checkRuntimeRequirement(t.Context(), req, func(context.Context, string) (string, error) { calls++; return "20.0.0", nil })
			require.Error(t, err)
			require.Zero(t, calls)
		})
	}
}

func TestRuntimeProbeCapturesStderrAndBoundsOutput(t *testing.T) {
	for _, test := range []struct {
		runtime, output   string
		stderr, wantError bool
	}{
		{"java", "openjdk version \"21.0.2\"", true, false},
		{"node", "v20.1.0", false, false},
		{"node", strings.Repeat("x", runtimeVersionOutputLimit+1), false, true},
		{"node", "private credential-shaped output", false, true},
	} {
		t.Run(test.runtime+fmt.Sprint(test.stderr, test.wantError), func(t *testing.T) {
			version, err := probeRuntimeVersion(t.Context(), test.runtime, func(cmd *exec.Cmd) error {
				require.NotNil(t, cmd.Cancel)
				require.LessOrEqual(t, cmd.WaitDelay, time.Second)
				writer := cmd.Stdout
				if test.stderr {
					writer = cmd.Stderr
				}
				require.NotNil(t, writer)
				_, err := io.WriteString(writer, test.output)
				return err
			})
			if test.wantError {
				require.Error(t, err)
				require.Empty(t, version)
				require.NotContains(t, err.Error(), test.output)
			} else {
				require.NoError(t, err)
				require.NotEmpty(t, version)
			}
		})
	}
}

func TestRuntimeVersionOutputLimitSurvivesStreamingCopy(t *testing.T) {
	var output runtimeVersionOutput
	_, err := io.Copy(&output, io.LimitReader(strings.NewReader(strings.Repeat("x", runtimeVersionOutputLimit+1)), runtimeVersionOutputLimit+1))
	require.Error(t, err)
	require.LessOrEqual(t, output.Len(), runtimeVersionOutputLimit)
}

func TestRuntimeOutputBoundsStringWriters(t *testing.T) {
	var output runtimeVersionOutput
	_, err := io.WriteString(&output, strings.Repeat("x", runtimeVersionOutputLimit+1))
	require.Error(t, err)
	require.LessOrEqual(t, output.Len(), runtimeVersionOutputLimit)
}
