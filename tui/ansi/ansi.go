package ansi

import (
	"unicode"
	"unicode/utf8"

	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/text"
)

// Strip removes terminal control sequences from s. It is a thin tau adapter
// over rovel's ANSI-aware text primitives.
func Strip(s string) string { return text.StripANSI(s) }

// VisibleWidth returns the visible terminal cell width of s.
func VisibleWidth(s string) int { return text.ANSIWidth(s) }

// Truncate returns s truncated to width visible cells.
func Truncate(s string, width int, ellipsis bool) string {
	return text.ANSITruncate(s, width, ellipsis)
}

// Wrap wraps s to width visible cells.
func Wrap(s string, width int) []string { return text.ANSIWrap(s, width) }

// WrapWords wraps s to width visible cells, preferring breaks at spaces and
// hard-breaking wider words, preserving ANSI styling across lines.
func WrapWords(s string, width int) []string { return text.ANSIWrapWords(s, width) }

// SliceStrict slices s by visible-cell interval, omitting wide graphemes that
// cross either boundary.
func SliceStrict(s string, start, end int) string { return text.ANSISliceStrict(s, start, end) }

// TextRune admits text keys without dropping Unicode joiners and format marks.
func TextRune(key event.KeyEvent) (rune, bool) {
	if key.Key != event.KeyRune || key.Mod & ^event.ModShift != 0 {
		return 0, false
	}
	r := key.Rune
	if key.Mod&event.ModShift != 0 && key.ShiftedRune != 0 {
		r = key.ShiftedRune
	}
	return r, utf8.ValidRune(r) && !unicode.IsControl(r)
}
