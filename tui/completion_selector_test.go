package tui

import (
	"testing"

	"github.com/losinggeneration/tau/tui/autocomplete"
)

func TestCompletionSelectorMapsAutocompleteItemsToSelectorRows(t *testing.T) {
	selector := NewCompletionSelector(&autocomplete.Suggestions{
		Prefix: "@rea",
		Items: []autocomplete.Item{{
			Value:       "@README.md",
			Description: "project readme",
		}},
	})

	items := selector.SelectorItems()
	if len(items) != 1 {
		t.Fatalf("selector items = %d, want 1", len(items))
	}
	if items[0].Name != "README.md" || items[0].Description != "project readme" {
		t.Fatalf("selector item = %#v, want README.md/project readme", items[0])
	}
}

func TestCompletionSelectorApplyUsesAutocompleteCompletion(t *testing.T) {
	selector := NewCompletionSelector(&autocomplete.Suggestions{
		Prefix: "@rea",
		Items:  []autocomplete.Item{{Value: "@README.md", Label: "README.md"}},
	})

	res, ok := selector.Apply([]string{"look at @rea"}, 0, len("look at @rea"), 0, "")
	if !ok {
		t.Fatal("Apply returned false")
	}
	if len(res.Lines) != 1 || res.Lines[0] != "look at @README.md " {
		t.Fatalf("Apply lines = %#v, want completed @README.md", res.Lines)
	}
}

func TestReplaceActiveAtReference(t *testing.T) {
	if got := ReplaceActiveAtReference("first @old\nsecond @rea", "README.md"); got != "first @old\nsecond @README.md" {
		t.Fatalf("ReplaceActiveAtReference = %q", got)
	}
}

func TestCompletionSelectorAppliesEmptyPrefixBeforeSuffix(t *testing.T) {
	selector := NewCompletionSelector(&autocomplete.Suggestions{Items: []autocomplete.Item{{Value: "文档/说明.md", Label: "说明.md"}}})
	lines := []string{"前文", "查看， 后文"}
	result, ok := selector.Apply(lines, 1, len("查看，"), 0, "")
	if !ok || len(result.Lines) != 2 || result.Lines[0] != "前文" || result.Lines[1] != "查看，文档/说明.md 后文" || result.CursorLine != 1 || result.CursorCol != len("查看，文档/说明.md") {
		t.Fatalf("result=%+v ok=%t", result, ok)
	}
	if lines[1] != "查看， 后文" {
		t.Fatalf("source modified: %q", lines)
	}
	empty := CompletionSelector{}
	if _, ok := empty.Apply(lines, 1, len("查看，"), 0, ""); ok {
		t.Fatal("empty selector applied")
	}
}
