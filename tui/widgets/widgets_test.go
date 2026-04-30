package widgets

import (
	"testing"

	"github.com/losinggeneration/tau/tui"
)

// TestLabel_ReexportWorks verifies that the re-exported Label type can be
// used as a drop-in for the underlying library's Label.
func TestLabel_ReexportWorks(t *testing.T) {
	l := NewLabel("hello")

	var _ tui.View = l

	if l.ID() == 0 {
		t.Errorf("NewLabel().ID() = 0, want non-zero")
	}
}

func TestButton_ReexportWorks(t *testing.T) {
	b := NewButton("press me")

	var _ tui.View = b

	if b.ID() == 0 {
		t.Errorf("NewButton().ID() = 0, want non-zero")
	}
}

func TestTextInput_ReexportWorks(t *testing.T) {
	ti := NewTextInput()

	var _ tui.View = ti

	if ti.ID() == 0 {
		t.Errorf("NewTextInput().ID() = 0, want non-zero")
	}
}
