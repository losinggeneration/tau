package tui

import (
	"bytes"
	"testing"

	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/geom"
)

func TestStreamBackendDecodesBufferedEscapeSequences(t *testing.T) {
	be := newStreamBackend(bytes.NewBufferString("\x1b[1;5D"), geom.Size{W: 20, H: 4})
	if _, err := be.Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	ev := be.ReadEvent()
	ke, ok := ev.(event.KeyEvent)
	if !ok {
		t.Fatalf("event = %T, want KeyEvent", ev)
	}
	if ke.Key != event.KeyLeft || ke.Mod != event.ModCtrl {
		t.Fatalf("key event = %+v, want ctrl+left", ke)
	}
}

func TestStreamBackendDecodesBracketedPaste(t *testing.T) {
	be := newStreamBackend(bytes.NewBufferString("a\x1b[200~hello\nworld\x1b[201~b"), geom.Size{W: 20, H: 4})
	if _, err := be.Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	first, ok := be.ReadEvent().(event.KeyEvent)
	if !ok || first.Key != event.KeyRune || first.Rune != 'a' {
		t.Fatalf("first event = %+v, want rune a", first)
	}

	paste, ok := be.ReadEvent().(event.PasteEvent)
	if !ok {
		t.Fatalf("second event = %T, want PasteEvent", paste)
	}
	if paste.Text != "hello\nworld" {
		t.Fatalf("paste text = %q", paste.Text)
	}

	last, ok := be.ReadEvent().(event.KeyEvent)
	if !ok || last.Key != event.KeyRune || last.Rune != 'b' {
		t.Fatalf("last event = %+v, want rune b", last)
	}
}

func TestStreamBackendDecodesModifyOtherKeysFallback(t *testing.T) {
	be := newStreamBackend(bytes.NewBufferString("\x1b[27;2;13~\x1b[27;5;47~"), geom.Size{W: 20, H: 4})
	if _, err := be.Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	shiftEnter, ok := be.ReadEvent().(event.KeyEvent)
	if !ok {
		t.Fatalf("first event = %T, want KeyEvent", shiftEnter)
	}
	if shiftEnter.Key != event.KeyEnter || shiftEnter.Mod != event.ModShift {
		t.Fatalf("first key event = %+v, want shift+enter", shiftEnter)
	}

	ctrlSlash, ok := be.ReadEvent().(event.KeyEvent)
	if !ok {
		t.Fatalf("second event = %T, want KeyEvent", ctrlSlash)
	}
	if ctrlSlash.Key != event.KeyRune || ctrlSlash.Rune != '/' || ctrlSlash.Mod != event.ModCtrl {
		t.Fatalf("second key event = %+v, want ctrl+/", ctrlSlash)
	}
}

func TestStreamBackendDecodesUTF8AcrossChunkBoundaries(t *testing.T) {
	be := newStreamBackend(bytes.NewBufferString("é"), geom.Size{W: 20, H: 4})
	if _, err := be.Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	ev := be.ReadEvent()
	ke, ok := ev.(event.KeyEvent)
	if !ok {
		t.Fatalf("event = %T, want KeyEvent", ev)
	}
	if ke.Key != event.KeyRune || ke.Rune != 'é' {
		t.Fatalf("key event = %+v, want rune é", ke)
	}
}
