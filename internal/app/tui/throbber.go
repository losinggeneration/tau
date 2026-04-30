package tui

import "time"

const throbberInterval = 80 * time.Millisecond

var throbberFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type throbber struct {
	frame int
}

func (t *throbber) Frame() string {
	if len(throbberFrames) == 0 {
		return ""
	}
	return throbberFrames[t.frame%len(throbberFrames)]
}

func (t *throbber) Advance() string {
	if len(throbberFrames) == 0 {
		return ""
	}
	t.frame = (t.frame + 1) % len(throbberFrames)
	return t.Frame()
}

func (t *throbber) Reset() {
	t.frame = 0
}
