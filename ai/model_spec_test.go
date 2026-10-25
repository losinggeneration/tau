package ai

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExpandModelScopeMatchesUpstreamPatterns(t *testing.T) {
	keys := []string{"anthropic/claude-sonnet-4", "anthropic/claude-opus-4", "openai/gpt-5", "amazon-bedrock/anthropic.claude-v2:0"}
	for _, tc := range []struct {
		name     string
		patterns []string
		want     []string
	}{
		{name: "exact keys keep pattern order", patterns: []string{"openai/gpt-5", "anthropic/claude-opus-4"}, want: []string{"openai/gpt-5", "anthropic/claude-opus-4"}},
		{name: "bare id and case", patterns: []string{"GPT-5"}, want: []string{"openai/gpt-5"}},
		{name: "thinking suffix", patterns: []string{"openai/gpt-5:high", "anthropic/*:low"}, want: []string{"openai/gpt-5", "anthropic/claude-sonnet-4", "anthropic/claude-opus-4"}},
		{name: "id glob", patterns: []string{"*sonnet*"}, want: []string{"anthropic/claude-sonnet-4"}},
		{name: "colon in id is not a level", patterns: []string{"amazon-bedrock/anthropic.claude-v2:0"}, want: []string{"amazon-bedrock/anthropic.claude-v2:0"}},
		{name: "unique substring", patterns: []string{"opus"}, want: []string{"anthropic/claude-opus-4"}},
		{name: "ambiguous substring and misses drop", patterns: []string{"claude-", "missing/*", ""}, want: nil},
		{name: "duplicates collapse", patterns: []string{"gpt-5", "openai/*"}, want: []string{"openai/gpt-5"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, scoped := range ExpandModelScope(tc.patterns, keys) {
				got = append(got, scoped.Model.String())
			}
			assert.Equal(t, tc.want, got)
		})
	}
	assert.Equal(t, []ScopedModel{
		{Model: ModelSpec{Provider: "openai", Model: "gpt-5"}, ThinkingLevel: "high"},
		{Model: ModelSpec{Provider: "anthropic", Model: "claude-sonnet-4"}, ThinkingLevel: "low"},
		{Model: ModelSpec{Provider: "anthropic", Model: "claude-opus-4"}, ThinkingLevel: "low"},
	}, ExpandModelScope([]string{"GPT-5:high", "anthropic/*:low", "openai/gpt-5:minimal"}, keys), "first match keeps its level")
}

func TestModelCandidatesIncludeAnthropicBearerEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "bearer-secret")
	t.Setenv("ANTHROPIC_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	var patternFound, fallbackFound bool
	for _, candidate := range buildModelPatternCandidates(nil) {
		patternFound = patternFound || candidate.Spec.Provider == ProviderAnthropic
	}
	for _, candidate := range buildAllAvailableModelSpecs() {
		fallbackFound = fallbackFound || candidate.Provider == ProviderAnthropic
	}
	assert.True(t, patternFound)
	assert.True(t, fallbackFound)
}
