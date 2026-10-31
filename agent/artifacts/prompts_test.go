package artifacts

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubstituteArgsIsSinglePassAndUsesDefaultsAndSafeSlices(t *testing.T) {
	for _, tc := range []struct {
		template string
		args     []string
		want     string
	}{
		{"$1", []string{"$ARGUMENTS $@ ${@:9}"}, "$ARGUMENTS $@ ${@:9}"},
		{"${@:2}", []string{"a", "$ARGUMENTS"}, "$ARGUMENTS"},
		{"${1:-default} ${2:-fallback}", []string{"present", ""}, "present fallback"},
		{"${@:-all} ${ARGUMENTS:-all}", nil, "all all"},
		{"${1:-$@}", nil, "$@"},
		{"${@:9} ${@:9:2}", []string{"a"}, " "},
		{"${@:0:2} ${@:2:0}", []string{"a", "b", "c"}, "a b "},
		{"$999999999999999999999999", []string{"a"}, ""},
		{"${@:999999999999999999999999}", []string{"a"}, ""},
		{"${@:1:999999999999999999999999}", []string{"a"}, "a"},
		{"$0|$2|$@|$ARGUMENTS", []string{"one", "two"}, "|two|one two|one two"},
	} {
		t.Run(tc.template, func(t *testing.T) { assert.Equal(t, tc.want, SubstituteArgs(tc.template, tc.args)) })
	}
}

func TestParseCommandArgsSplitsJavaScriptWhitespace(t *testing.T) {
	assert.Equal(t, []string{"one", "two", "three", "four five", "six seven", "eight", "nine"}, ParseCommandArgs("one\ntwo\rthree\v'four five'\f\"six seven\"\u00a0eight\ufeffnine"))
	assert.Equal(t, []string{"日本語", "café"}, ParseCommandArgs("日本語\tcafé"))
	assert.Equal(t, []string{"a", "b"}, ParseCommandArgs(strings.Join([]string{"a", "b"}, "\u2028")))
}

func TestParsePromptUsesYAMLMetadata(t *testing.T) {
	prompt, err := parsePrompt(Entry{Kind: KindPrompt, SourcePath: "review.md", Raw: []byte("---\ndescription: \"Review: carefully\" # comment\nargument-hint: '[path]'\nnested:\n  enabled: true\n---\nbody")})
	require.NoError(t, err)
	assert.Equal(t, "Review: carefully", prompt.Description)
	assert.Equal(t, "[path]", prompt.ArgumentHint)
	assert.Equal(t, map[string]any{"enabled": true}, prompt.ExtraFields["nested"])
	for _, kind := range []ArtifactKind{KindPrompt, KindSkill} {
		entry := Entry{Kind: kind, SourcePath: "test.md", Raw: []byte("---\nname: test\ndescription: [unclosed\n---\nbody")}
		if kind == KindPrompt {
			_, err = parsePrompt(entry)
		} else {
			_, err = parseSkill(entry)
		}
		assert.Error(t, err)
	}
}

func TestParsePromptDescriptionMatchesUpstream(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"\n   first line   \nrest", "   first line   "},
		{strings.Repeat("x", 61), strings.Repeat("x", 60) + "..."},
		{strings.Repeat("x", 60), strings.Repeat("x", 60)},
		{"---\ndescription: " + strings.Repeat("x", 80) + "\n---\nbody", strings.Repeat("x", 80)},
		{"---\ndescription: 123\n---\nfallback", "fallback"},
		{"---\ndescription: null\n---\nfallback", "fallback"},
		{strings.Repeat("😀", 31), strings.Repeat("😀", 30) + "..."},
	} {
		t.Run(tc.body, func(t *testing.T) {
			prompt, err := parsePrompt(Entry{Kind: KindPrompt, SourcePath: "review.md", Raw: []byte(tc.body)})
			require.NoError(t, err)
			assert.Equal(t, tc.want, prompt.Description)
		})
	}
}

func TestParsePromptIgnoresNonStringArgumentHint(t *testing.T) {
	prompt, err := parsePrompt(Entry{Kind: KindPrompt, SourcePath: "review.md", Raw: []byte("---\nargument-hint: 123\n---\nbody")})
	require.NoError(t, err)
	assert.Empty(t, prompt.ArgumentHint)
}
