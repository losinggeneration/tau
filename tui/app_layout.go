package tui

// AppLayout composes the Pi-style interactive application regions in their
// canonical top-to-bottom order:
//
//  1. Header
//  2. Chat
//  3. Pending messages
//  4. Status
//  5. Widgets above the editor
//  6. Editor
//  7. Widgets below the editor
//  8. Footer
//
// The chat region is the only flexible region; every other region sizes to its
// content and may collapse to 0x0 when its backing view is empty. This type is
// intentionally a top-level tui wrapper around the lower-level VStack so
// application code does not need to assemble the shell from rovel/layout pieces
// directly.
type AppLayout struct {
	*VStack

	Header       View
	Chat         View
	Pending      View
	Status       View
	WidgetsAbove View
	Editor       View
	WidgetsBelow View
	Footer       View
}

// AppLayoutOpts provides the eight canonical application regions. Nil regions
// are skipped, which is useful for tests and for hosts that do not support a
// given optional region.
type AppLayoutOpts struct {
	Header       View
	Chat         View
	Pending      View
	Status       View
	WidgetsAbove View
	Editor       View
	WidgetsBelow View
	Footer       View
}

// NewAppLayout returns an AppLayout with regions installed in the canonical
// order. Chat is registered as a GrowY child so it receives all remaining
// vertical space after fixed-height regions have been measured.
func NewAppLayout(opts AppLayoutOpts) *AppLayout {
	stack := NewVStack()
	add := func(v View) {
		if v != nil {
			stack.AddChild(NewLayoutChild(v))
		}
	}
	addGrowChat := func(v View) {
		if v != nil {
			stack.AddChild(GrowYLayoutChild(v, 1))
		}
	}

	add(opts.Header)
	addGrowChat(opts.Chat)
	add(opts.Pending)
	add(opts.Status)
	add(opts.WidgetsAbove)
	add(opts.Editor)
	add(opts.WidgetsBelow)
	add(opts.Footer)

	return &AppLayout{
		VStack:       stack,
		Header:       opts.Header,
		Chat:         opts.Chat,
		Pending:      opts.Pending,
		Status:       opts.Status,
		WidgetsAbove: opts.WidgetsAbove,
		Editor:       opts.Editor,
		WidgetsBelow: opts.WidgetsBelow,
		Footer:       opts.Footer,
	}
}
