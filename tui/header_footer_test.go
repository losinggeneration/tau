package tui

import "testing"

func TestHeaderFooterWidgetPlacement(t *testing.T) {
	layout := HeaderFooter{
		Header:       NewText("head"),
		Chat:         NewText("chat"),
		Pending:      NewText("pend"),
		Status:       NewText("stat"),
		WidgetsAbove: []RowRenderer{NewText("above")},
		Editor:       NewText("edit"),
		WidgetsBelow: []RowRenderer{NewText("below")},
		Footer:       NewText("foot"),
	}
	want := []string{"head ", "chat ", "pend ", "stat ", "above", "edit ", "below", "foot "}
	if got := layout.Render(5); !equalStrings(got, want) {
		t.Fatalf("Render = %#v, want %#v", got, want)
	}
}
