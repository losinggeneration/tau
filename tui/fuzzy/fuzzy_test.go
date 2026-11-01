package fuzzy

import (
	"math"
	"testing"
)

func TestMatchStringEmptyQueryMatchesEverything(t *testing.T) {
	result := MatchString("", "anything")
	if !result.Matches || result.Score != 0 {
		t.Fatalf("MatchString empty = %+v, want match score 0", result)
	}
}

func TestMatchStringQueryLongerThanTextDoesNotMatch(t *testing.T) {
	if result := MatchString("longquery", "short"); result.Matches {
		t.Fatalf("MatchString long query = %+v, want no match", result)
	}
}

func TestMatchStringExactMatchHasGoodScore(t *testing.T) {
	result := MatchString("test", "test")
	if !result.Matches {
		t.Fatalf("MatchString exact = %+v, want match", result)
	}
	if result.Score >= 0 {
		t.Fatalf("MatchString exact score = %v, want negative", result.Score)
	}
}

func TestMatchStringCharactersMustAppearInOrder(t *testing.T) {
	if result := MatchString("abc", "aXbXc"); !result.Matches {
		t.Fatalf("MatchString in order = %+v, want match", result)
	}
	if result := MatchString("abc", "cba"); result.Matches {
		t.Fatalf("MatchString out of order = %+v, want no match", result)
	}
}

func TestMatchStringCaseInsensitive(t *testing.T) {
	if result := MatchString("ABC", "abc"); !result.Matches {
		t.Fatalf("MatchString ABC/abc = %+v, want match", result)
	}
	if result := MatchString("abc", "ABC"); !result.Matches {
		t.Fatalf("MatchString abc/ABC = %+v, want match", result)
	}
}

func TestMatchStringConsecutiveScoresBetterThanScattered(t *testing.T) {
	consecutive := MatchString("foo", "foobar")
	scattered := MatchString("foo", "f_o_o_bar")
	if !consecutive.Matches || !scattered.Matches {
		t.Fatalf("matches = %+v %+v, want both match", consecutive, scattered)
	}
	if consecutive.Score >= scattered.Score {
		t.Fatalf("consecutive score = %v, scattered = %v, want consecutive lower", consecutive.Score, scattered.Score)
	}
}

func TestMatchStringWordBoundaryScoresBetter(t *testing.T) {
	atBoundary := MatchString("fb", "foo-bar")
	notAtBoundary := MatchString("fb", "afbx")
	if !atBoundary.Matches || !notAtBoundary.Matches {
		t.Fatalf("matches = %+v %+v, want both match", atBoundary, notAtBoundary)
	}
	if atBoundary.Score >= notAtBoundary.Score {
		t.Fatalf("boundary score = %v, nonboundary = %v, want boundary lower", atBoundary.Score, notAtBoundary.Score)
	}
}

func TestMatchStringSwappedAlphaNumericTokens(t *testing.T) {
	result := MatchString("codex52", "gpt-5.2-codex")
	if !result.Matches {
		t.Fatalf("MatchString swapped = %+v, want match", result)
	}
}

func TestFilterEmptyQueryReturnsAllItemsUnchanged(t *testing.T) {
	items := []string{"apple", "banana", "cherry"}
	result := Filter(items, "", func(x string) string { return x })
	assertStrings(t, result, items)
	if len(result) > 0 {
		result[0] = "changed"
		if items[0] == "changed" {
			t.Fatalf("Filter returned aliased slice")
		}
	}
}

func TestFilterFiltersOutNonMatchingItems(t *testing.T) {
	items := []string{"apple", "banana", "cherry"}
	result := Filter(items, "an", func(x string) string { return x })
	assertStrings(t, result, []string{"banana"})
}

func TestFilterSortsByMatchQuality(t *testing.T) {
	items := []string{"a_p_p", "app", "application"}
	result := Filter(items, "app", func(x string) string { return x })
	if len(result) == 0 || result[0] != "app" {
		t.Fatalf("Filter first = %v, want app", result)
	}
}

func TestFilterWorksWithCustomTextFunction(t *testing.T) {
	type item struct {
		name string
		id   int
	}
	items := []item{{name: "foo", id: 1}, {name: "bar", id: 2}, {name: "foobar", id: 3}}
	result := Filter(items, "foo", func(item item) string { return item.name })
	if len(result) != 2 {
		t.Fatalf("Filter len = %d, want 2: %+v", len(result), result)
	}
	if result[0].name != "foo" || result[1].name != "foobar" {
		t.Fatalf("Filter result = %+v, want foo and foobar", result)
	}
}

func TestFilterAllTokensMustMatch(t *testing.T) {
	items := []string{"foo bar", "foo baz", "bar baz", "foo"}
	result := Filter(items, "foo bz", func(x string) string { return x })
	assertStrings(t, result, []string{"foo baz"})
}

func assertStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: got %v want %v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("[%d] = %q, want %q: got %v want %v", i, got[i], want[i], got, want)
		}
	}
}

func TestFilterRanksExactMatchFirstAndSplitsSlashTokens(t *testing.T) {
	id := func(s string) string { return s }
	if got := Filter([]string{"clone", "cl"}, "cl", id); len(got) != 2 || got[0] != "cl" {
		t.Fatalf("exact match not first: %v", got)
	}
	models := []string{"openai/gpt-5.2", "anthropic/claude-opus-4-6"}
	if got := Filter(models, "opus/anthropic", id); len(got) != 1 || got[0] != "anthropic/claude-opus-4-6" {
		t.Fatalf("reordered provider/model query: %v", got)
	}
}

func TestFilterUsesJavaScriptWhitespace(t *testing.T) {
	items := []string{"alpha beta", "beta alpha", "alpha\u0085beta"}
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"\ufeff", items},
		{"beta\ufeffalpha", []string{"alpha beta", "alpha\u0085beta", "beta alpha"}},
		{"\u0085", []string{"alpha\u0085beta"}},
		{"\ufeffalpha\ufeff", []string{"alpha beta", "alpha\u0085beta", "beta alpha"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			assertStrings(t, Filter(items, tc.query, func(s string) string { return s }), tc.want)
		})
	}
}

func TestMatchStringScoresJavaScriptWhitespaceBoundaries(t *testing.T) {
	for _, separator := range []rune{'\u00a0', '\ufeff', '\v', '\f', '\u2028', '\u0085'} {
		want := -9.9
		if separator == '\u0085' {
			want = 0.1
		}
		got := MatchString("b", string(separator)+"b")
		if !got.Matches || math.Abs(got.Score-want) > 1e-9 {
			t.Errorf("separator=%U got=%+v want=%v", separator, got, want)
		}
	}
}

func TestMatchStringScoresUTF16CodeUnits(t *testing.T) {
	for _, tc := range []struct {
		query, text string
		score       float64
	}{{"😀", "😀", -124.9}, {"x", "😀x", 0.2}, {"ab", "a😀b", -10.7}, {"字", "字", -115}} {
		got := MatchString(tc.query, tc.text)
		if !got.Matches || math.Abs(got.Score-tc.score) > 1e-9 {
			t.Errorf("%q/%q got=%+v want=%v", tc.query, tc.text, got, tc.score)
		}
	}
}

func TestMatchStringUsesUnicodeLowercaseMappings(t *testing.T) {
	for _, tc := range []struct {
		query, text string
		matches     bool
		score       float64
	}{
		{"İ", "i", false, 0},
		{"i\u0307", "İ", true, -124.9},
		{"ΟΣ", "ος", true, -124.9},
		{"ΟΣΑ", "οσα", true, -139.7},
		{"Σ", "ς", false, 0},
		{"Σ", "σ", true, -115},
	} {
		got := MatchString(tc.query, tc.text)
		if got.Matches != tc.matches || math.Abs(got.Score-tc.score) > 1e-9 {
			t.Errorf("%q/%q got=%+v want=%t,%v", tc.query, tc.text, got, tc.matches, tc.score)
		}
	}
}
