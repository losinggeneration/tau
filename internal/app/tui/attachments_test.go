package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPendingAttachmentsTracksAtFiles(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(file, []byte("body"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	a := &App{widgetsAbove: NewWidgetContainer(widgetPlacementAbove), footer: newFooterForTest()}
	a.footer.data.cwd = dir

	a.updatePendingAttachments("read @note.txt", stubCtx())
	got := a.widgetsAbove.Widgets()[pendingAttachmentsWidgetKey]
	if len(got) != 1 || !strings.Contains(got[0], "note.txt") || !strings.Contains(got[0], "Attached:") {
		t.Fatalf("indicator = %#v", got)
	}

	a.updatePendingAttachments("read note.txt", stubCtx())
	if a.widgetsAbove.MinSize().H != 0 {
		t.Fatal("indicator stayed visible after removing @file")
	}
}
