package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
)

func TestRootRPCLeavesStdinAndFilesForProtocol(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"protocol input", nil, "--fork"},
		{"forced stdin", []string{"--stdin"}, "--fork"},
		{"missing attachment", []string{"@" + filepath.Join(t.TempDir(), "missing.md")}, "--mode rpc does not accept"},
		{"prompt", []string{"hello"}, "--mode rpc does not accept"},
		{"message flag", []string{"--message", "hello"}, "--mode rpc does not accept"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, writer, err := os.Pipe()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, input.Close()) })
			const wire = "{\"type\":\"get_state\"}\n"
			_, err = io.WriteString(writer, wire)
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			original := os.Stdin
			os.Stdin = input
			t.Cleanup(func() { os.Stdin = original })
			cmd := &cli.Command{Flags: cloneFlags(append(append([]cli.Flag{}, RootFlags...), ConversationFlags...)), Action: runRoot, ExitErrHandler: func(context.Context, *cli.Command, error) {}}
			args := append([]string{"tau", "--mode", "rpc", "--fork", "target", "--no-session"}, tc.args...)
			err = cmd.Run(t.Context(), args)
			require.ErrorContains(t, err, tc.want)
			remaining, err := io.ReadAll(input)
			require.NoError(t, err)
			assert.Equal(t, wire, string(remaining))
		})
	}
}

func TestPipedStdinUsesJavaScriptWhitespaceTrimming(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  []string
	}{
		{" \n\t\r\u00a0\u2003\u2028\u2029\ufeff", nil},
		{"\ufeff \n hello\nworld \t\u2029", []string{"hello\nworld"}},
		{" \u0085 \n", []string{"\u0085"}},
	} {
		prompts, err := buildPromptSequence(nil, nil, strings.NewReader(tc.input), false, false)
		require.NoError(t, err)
		assert.Equal(t, tc.want, prompts)
	}
	prompts, err := buildPromptSequence([]string{" positional "}, []string{" message "}, strings.NewReader(" \n stdin \n"), true, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"stdin positional ", " message "}, prompts)
	prompts, err = buildPromptSequence([]string{" positional "}, nil, strings.NewReader("ignored"), true, false)
	require.NoError(t, err)
	assert.Equal(t, []string{" positional "}, prompts)
}

func TestInitialPromptCombinesFirstMessageFlagWithContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.md")
	require.NoError(t, os.WriteFile(path, []byte("context"), 0o600))
	wrapped, _, err := expandFileArg(path)
	require.NoError(t, err)
	for _, tc := range []struct {
		args, messages []string
		stdin          string
		want           []string
	}{
		{nil, []string{"first", "second"}, "stdin", []string{"stdinfirst", "second"}},
		{[]string{"@" + path}, []string{"first", "second"}, "stdin", []string{"stdin" + wrapped + "first", "second"}},
		{[]string{"positional", "next"}, []string{"first", "second"}, "stdin", []string{"stdinpositional", "next", "first", "second"}},
		{nil, []string{"first", "second"}, "", []string{"first", "second"}},
	} {
		prompts, _, err := buildPromptSequenceWithFiles(tc.args, tc.messages, strings.NewReader(tc.stdin), false, false)
		require.NoError(t, err)
		assert.Equal(t, tc.want, prompts)
	}
}
