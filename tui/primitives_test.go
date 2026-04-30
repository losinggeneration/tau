package tui

import (
	"testing"

	"github.com/losinggeneration/rovel/text"
)

func TestText_RenderWrapsAndPads(t *testing.T) {
	tr := NewText("hello world")
	got := tr.Render(5)
	want := []string{"hello", " worl", "d    "}
	if !equalStrings(got, want) {
		t.Fatalf("Render = %#v, want %#v", got, want)
	}
}

func TestRenderRows(t *testing.T) {
	got := RenderRows(NewText("hi"), 4)
	want := []string{"hi  "}
	if !equalStrings(got, want) {
		t.Fatalf("RenderRows = %#v, want %#v", got, want)
	}

	if got := RenderRows(nil, 4); got != nil {
		t.Fatalf("RenderRows nil = %#v, want nil", got)
	}
}

func TestValidateRenderRowsReportsComponentOverwideRows(t *testing.T) {
	diagnostics := ValidateRows([]string{"ok", "too wide"}, 3)
	if len(diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v, want one overwide row", diagnostics)
	}
	got := diagnostics[0]
	if got.Index != 1 || got.Width != 8 || got.Limit != 3 || got.Row != "too wide" {
		t.Fatalf("diagnostic = %+v, want row 1 width 8 limit 3", got)
	}

	if diagnostics := ValidateRenderRows(NewText("hello"), 5); len(diagnostics) != 0 {
		t.Fatalf("wrapped Text diagnostics = %#v, want none", diagnostics)
	}
}

func TestSpacer_Render(t *testing.T) {
	sp := NewSpacer(3)
	got := sp.Render(10)
	want := []string{"", "", ""}
	if !equalStrings(got, want) {
		t.Fatalf("Spacer.Render = %#v, want %#v", got, want)
	}
}

func TestTruncatedText_Render(t *testing.T) {
	tr := NewTruncatedText("hello world")
	got := tr.Render(5)
	if len(got) != 1 {
		t.Fatalf("Render rows = %d, want 1: %#v", len(got), got)
	}
	if text.Width(got[0]) != 5 {
		t.Fatalf("Render width = %d, want 5 for %#v", text.Width(got[0]), got[0])
	}
}

func TestBox_RenderPaddingAndEmpty(t *testing.T) {
	box := NewBox(NewText("hi"))
	box.SetPadding(1, 1)
	got := box.Render(6)
	want := []string{"      ", " hi   ", "      "}
	if !equalStrings(got, want) {
		t.Fatalf("Box.Render = %#v, want %#v", got, want)
	}

	empty := NewBox()
	if got := empty.Render(6); got != nil {
		t.Fatalf("empty Box.Render = %#v, want nil", got)
	}
}

func TestVisualTruncate(t *testing.T) {
	got, skipped := VisualTruncate([]string{"a", "b", "c", "d"}, 2)
	want := []string{"c", "d"}
	if skipped != 2 {
		t.Fatalf("skipped = %d, want 2", skipped)
	}
	if !equalStrings(got, want) {
		t.Fatalf("visible = %#v, want %#v", got, want)
	}

	got, skipped = VisualTruncate([]string{"a"}, 2)
	if skipped != 0 || !equalStrings(got, []string{"a"}) {
		t.Fatalf("untruncated = %#v/%d, want [a]/0", got, skipped)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
