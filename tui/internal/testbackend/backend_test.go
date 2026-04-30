package testbackend

import (
	"bytes"
	"testing"

	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/geom"
)

func TestNew_SetsSize(t *testing.T) {
	be := New(geom.Size{W: 80, H: 24}, &bytes.Buffer{})
	if got := be.Size(); got.W != 80 || got.H != 24 {
		t.Errorf("Size() = %v, want {80 24}", got)
	}
}

func TestNew_WriteCapturedToBuffer(t *testing.T) {
	buf := &bytes.Buffer{}
	be := New(geom.Size{W: 80, H: 24}, buf)

	n, err := be.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if n != 5 {
		t.Errorf("Write n = %d, want 5", n)
	}

	if buf.String() != "hello" {
		t.Errorf("buf = %q, want %q", buf.String(), "hello")
	}
}

func TestNew_WriteNilBufferDoesNotPanic(t *testing.T) {
	be := New(geom.Size{W: 80, H: 24}, nil)
	if _, err := be.Write([]byte("hello")); err != nil {
		t.Errorf("Write to nil buffer: %v", err)
	}
}

func TestEnable_RecordsEnabled(t *testing.T) {
	be := New(geom.Size{W: 80, H: 24}, &bytes.Buffer{})

	if be.Enabled() {
		t.Errorf("Enabled before Enable() = true, want false")
	}

	if _, err := be.Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	if !be.Enabled() {
		t.Errorf("Enabled after Enable() = false, want true")
	}
}

func TestRestore_RecordsRestored(t *testing.T) {
	be := New(geom.Size{W: 80, H: 24}, &bytes.Buffer{})

	if be.Restored() {
		t.Errorf("Restored before Restore() = true, want false")
	}

	if err := be.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if !be.Restored() {
		t.Errorf("Restored after Restore() = false, want true")
	}
}

func TestSendEvent_BlocksUntilRead(t *testing.T) {
	be := New(geom.Size{W: 80, H: 24}, &bytes.Buffer{})

	// Send an event in a goroutine; verify it can be read.
	be.SendEvent(event.KeyEvent{Key: event.KeyEsc})

	got := be.ReadEvent()
	if got == nil {
		t.Fatalf("ReadEvent = nil, want event")
	}

	if k, ok := got.(event.KeyEvent); !ok || k.Key != event.KeyEsc {
		t.Errorf("ReadEvent = %v, want KeyEvent{KeyEsc}", got)
	}
}

func TestClose_ShutsDownReadEvents(t *testing.T) {
	be := New(geom.Size{W: 80, H: 24}, &bytes.Buffer{})
	be.Close()

	got := be.ReadEvent()
	if got != nil {
		t.Errorf("ReadEvent after Close = %v, want nil", got)
	}
}

func TestOutput_ReturnsCapturedBytes(t *testing.T) {
	buf := &bytes.Buffer{}
	be := New(geom.Size{W: 80, H: 24}, buf)

	be.Write([]byte("test"))

	out := be.Output()
	if string(out) != "test" {
		t.Errorf("Output() = %q, want %q", out, "test")
	}
}
