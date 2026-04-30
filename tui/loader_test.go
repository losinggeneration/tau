package tui

import (
	"testing"
	"time"
)

func TestLoader_RenderLeadingBlankAndMessage(t *testing.T) {
	loader := NewLoader(WithLoaderMessage("Working"), WithLoaderInterval(time.Hour))
	defer loader.Stop()

	got := loader.Render(20)
	want := []string{"", " ⠋ Working          "}
	if !equalStrings(got, want) {
		t.Fatalf("Render = %#v, want %#v", got, want)
	}
}

func TestLoader_SetMessageInvalidatesAndRenders(t *testing.T) {
	invalidations := 0
	loader := NewLoader(
		WithLoaderMessage("Loading"),
		WithLoaderInterval(time.Hour),
		WithLoaderInvalidate(func() { invalidations++ }),
	)
	defer loader.Stop()

	loader.SetMessage("Done")

	if invalidations == 0 {
		t.Fatalf("SetMessage did not request invalidation")
	}

	got := loader.Render(16)
	want := []string{"", " ⠋ Done         "}
	if !equalStrings(got, want) {
		t.Fatalf("Render = %#v, want %#v", got, want)
	}
}

func TestLoader_AdvanceFrame(t *testing.T) {
	loader := NewLoader(WithLoaderMessage("Loading"), WithLoaderInterval(time.Hour))
	defer loader.Stop()

	loader.AdvanceFrame()

	got := loader.Render(12)
	want := []string{"", " ⠙ Loading  "}
	if !equalStrings(got, want) {
		t.Fatalf("Render = %#v, want %#v", got, want)
	}
}

func TestCancellableLoader_HandleCancelAborts(t *testing.T) {
	loader := NewCancellableLoader(WithLoaderInterval(time.Hour))
	defer loader.Dispose()

	called := 0
	loader.OnAbort = func() { called++ }

	if !loader.Handle(KeyEvent{Key: KeyEsc}, nil) {
		t.Fatalf("Handle(Escape) = false, want true")
	}
	if !loader.Aborted() {
		t.Fatalf("Aborted() = false, want true")
	}
	if called != 1 {
		t.Fatalf("OnAbort calls = %d, want 1", called)
	}

	select {
	case <-loader.Context().Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("Context was not cancelled")
	}
}

func TestCancellableLoader_HandleCancelIsIdempotent(t *testing.T) {
	loader := NewCancellableLoader(WithLoaderInterval(time.Hour))
	defer loader.Dispose()

	called := 0
	loader.OnAbort = func() { called++ }

	loader.Handle(KeyEvent{Key: KeyEsc}, nil)
	loader.Handle(KeyEvent{Key: KeyCtrlC}, nil)

	if called != 1 {
		t.Fatalf("OnAbort calls = %d, want 1", called)
	}
}

func TestLoaderSpinnerFramesAndIntervalParity(t *testing.T) {
	loader := NewLoader(WithLoaderMessage("x"), WithLoaderInterval(time.Hour))
	defer loader.Stop()
	want := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏", "⠋"}
	for i, frame := range want {
		got := loader.Render(4)
		wantRow := " " + frame + "  "
		if len(got) != 2 || got[1] != wantRow {
			t.Fatalf("frame %d render=%#v want %q", i, got, wantRow)
		}
		loader.AdvanceFrame()
	}
}
