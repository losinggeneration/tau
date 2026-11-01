package ansi

import (
	"testing"

	"github.com/losinggeneration/rovel/event"
)

func TestTextRuneAdmission(t *testing.T) {
	for _, r := range []rune{'a', '\u0301', '\u200d', '\u200c', '\u00a0', '\u2028', '\ufeff'} {
		got, ok := TextRune(event.KeyEvent{Key: event.KeyRune, Rune: r})
		if !ok || got != r {
			t.Fatalf("U+%04X = U+%04X, %v", r, got, ok)
		}
	}
	for _, r := range []rune{0, 7, 27, 127, 128, 159, -1, 0xd800, 0x110000} {
		if _, ok := TextRune(event.KeyEvent{Key: event.KeyRune, Rune: r}); ok {
			t.Fatalf("accepted U+%04X", r)
		}
	}
	for _, mod := range []event.ModMask{event.ModCtrl, event.ModAlt, event.ModShift | event.ModCtrl} {
		if _, ok := TextRune(event.KeyEvent{Key: event.KeyRune, Rune: 'a', Mod: mod}); ok {
			t.Fatalf("accepted modifier %v", mod)
		}
	}
	got, ok := TextRune(event.KeyEvent{Key: event.KeyRune, Rune: '=', ShiftedRune: '+', Mod: event.ModShift})
	if !ok || got != '+' {
		t.Fatalf("shifted = %q, %v", got, ok)
	}
	if _, ok := TextRune(event.KeyEvent{Key: event.KeyEnter, Rune: 'a'}); ok {
		t.Fatal("accepted nontext key")
	}
}
