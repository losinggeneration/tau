package fuzzy

import "testing"

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
