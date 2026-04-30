package layout

import (
	"testing"

	"github.com/losinggeneration/tau/tui"
)

// TestReexports_AlisesResolve verifies that the re-exported types and
// functions can be used as drop-in replacements for the underlying library.
func TestReexports_AlisesResolve(t *testing.T) {
	// Construct a view using the re-exported type aliases.
	root := NewVStack()

	// Verify the view satisfies the tui.View interface.
	var _ tui.View = root

	if root.ID() == 0 {
		t.Errorf("NewVStack().ID() = 0, want non-zero")
	}
}

func TestAlignConstants(t *testing.T) {
	// The four alignment constants must be distinct.
	all := []Align{AlignStart, AlignCenter, AlignEnd, AlignStretch}
	seen := make(map[Align]bool)
	for _, a := range all {
		if seen[a] {
			t.Errorf("duplicate Align value: %d", a)
		}
		seen[a] = true
	}
}

func TestOrientationConstants(t *testing.T) {
	if Horizontal == Vertical {
		t.Errorf("Horizontal must differ from Vertical")
	}
}
