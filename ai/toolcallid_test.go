package ai

import (
	"strings"
	"testing"
)

// TestSanitizeToolCallIDDisposal pins the one semantic difference between the
// providers sharing this helper: Google drops disallowed runes, Bedrock and
// the Responses family substitute an underscore. Collapsing the two would
// silently change every ID those providers emit.
func TestSanitizeToolCallIDDisposal(t *testing.T) {
	const id = "call.abc|def+ghi/jkl=mno"

	if got, want := sanitizeToolCallID(id, '_'), "call_abc_def_ghi_jkl_mno"; got != want {
		t.Errorf("substituting: got %q, want %q", got, want)
	}
	if got, want := sanitizeToolCallID(id, 0), "callabcdefghijklmno"; got != want {
		t.Errorf("dropping: got %q, want %q", got, want)
	}
}

// TestSanitizeToolCallIDPreservesAllowed verifies the allowed set is exactly
// [a-zA-Z0-9_-] and that a conforming ID is returned untouched.
func TestSanitizeToolCallIDPreservesAllowed(t *testing.T) {
	const id = "abcXYZ012_-"

	for _, sub := range []rune{'_', 0} {
		if got := sanitizeToolCallID(id, sub); got != id {
			t.Errorf("sanitizeToolCallID(%q, %q) = %q, want it unchanged", id, sub, got)
		}
	}
}

// TestSanitizeToolCallIDTruncates verifies the 64-byte clamp both providers
// require.
func TestSanitizeToolCallIDTruncates(t *testing.T) {
	got := sanitizeToolCallID(strings.Repeat("a", 100), '_')
	if len(got) != toolCallIDMaxLen {
		t.Errorf("length = %d, want %d", len(got), toolCallIDMaxLen)
	}
}

// TestSanitizeToolCallIDEmpty verifies an empty ID stays empty. Bedrock used
// to special-case this with an early return.
func TestSanitizeToolCallIDEmpty(t *testing.T) {
	if got := sanitizeToolCallID("", '_'); got != "" {
		t.Errorf("sanitizeToolCallID(\"\") = %q, want empty", got)
	}
}

// legacyResponsesNormalizePart is the exact algorithm openai_responses.go used
// before it moved onto the shared sanitizer: trim, then truncate, then trim
// again. The refactor reordered this to truncate-then-trim, so this copy
// exists to prove the two agree.
func legacyResponsesNormalizePart(part string) string {
	var b strings.Builder
	for _, r := range part {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	normalized := strings.TrimRight(b.String(), "_")
	if len(normalized) > 64 {
		normalized = normalized[:64]
		normalized = strings.TrimRight(normalized, "_")
	}
	return normalized
}

// TestResponsesNormalizePartMatchesLegacy checks the reordered implementation
// against the original across the cases where trim-order could matter: IDs
// shorter and longer than the cap, with and without trailing underscores, and
// with the underscore run straddling the 64-byte boundary.
func TestResponsesNormalizePartMatchesLegacy(t *testing.T) {
	normalize := openAIResponsesToolCallIDNormalizerWithProviders(
		&Model{Provider: ProviderGroq},
		map[string]struct{}{},
	)
	// A provider outside the allowed set routes straight to normalizePart.
	target := &Model{Provider: ProviderGroq}

	cases := []string{
		"",
		"call_abc123",
		"call.abc",
		"trailing___",
		"a" + strings.Repeat("_", 64),
		"a" + strings.Repeat("_", 63) + "b",
		strings.Repeat("a", 63) + strings.Repeat("_", 10),
		strings.Repeat("a", 64) + strings.Repeat("_", 10),
		strings.Repeat("a", 65) + strings.Repeat("_", 10),
		strings.Repeat("a", 100),
		strings.Repeat("a.", 50),
		strings.Repeat("_", 100),
		strings.Repeat("ab_", 30) + "!!!",
	}

	for _, in := range cases {
		t.Run(shortName(in), func(t *testing.T) {
			got := normalize(in, target, nil)
			if want := legacyResponsesNormalizePart(in); got != want {
				t.Errorf("normalize(%q) = %q, want %q", in, got, want)
			}
			if len(got) > toolCallIDMaxLen {
				t.Errorf("normalize(%q) length = %d, want <= %d", in, len(got), toolCallIDMaxLen)
			}
		})
	}
}

func shortName(s string) string {
	if s == "" {
		return "empty"
	}
	if len(s) > 20 {
		return s[:20] + "..."
	}
	return s
}
