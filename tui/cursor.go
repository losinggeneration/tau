package tui

import "strings"

// CursorMarker identifies the insertion point without occupying terminal cells.
const CursorMarker = "\x1b]1337;tau-cursor\x07"

const (
	fakeCursorStart = "\x1b]1337;tau-cursor-start\x07"
	fakeCursorEnd   = "\x1b]1337;tau-cursor-end\x07"
)

func resolveFakeCursors(row string) string {
	for {
		i := strings.Index(row, CursorMarker+fakeCursorStart)
		if i < 0 {
			break
		}
		tail := row[i+len(CursorMarker)+len(fakeCursorStart):]
		tail = strings.Replace(tail, fakeCursorEnd, "", 1)
		row = row[:i] + CursorMarker + tail
	}
	row = strings.ReplaceAll(row, fakeCursorStart, ansiInverseOn)
	return strings.ReplaceAll(row, fakeCursorEnd, ansiInverseOff)
}

func drawCursorRow(d Drawer, pos Point, row string, base Style, focused bool) {
	if !focused {
		row = strings.ReplaceAll(row, CursorMarker, "")
	}
	DrawANSIText(d, pos, row, base)
}
