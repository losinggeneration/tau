package tui

import (
	"bytes"
	"sync"
)

// counterState is a test-only model used by the headless cycle tests.
// counterState is shared between the app-loop goroutine (which mutates it from
// Handle) and the test goroutine (which polls it from waitFor and asserts on
// it), so every access goes through the mutex.
type counterState struct {
	mu     sync.Mutex
	value  int
	status string
}

func (s *counterState) add(delta int, status string) {
	s.mu.Lock()
	s.value += delta
	s.status = status
	s.mu.Unlock()
}

func (s *counterState) Value() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.value
}

func (s *counterState) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.status
}

// counterView is a test view that lets you increment/decrement a counter
// with + and - keys.
type counterView struct {
	id    ID
	rect  Rect
	state *counterState
}

func newCounterView(s *counterState) *counterView {
	return &counterView{id: NewID(), state: s}
}

func (c *counterView) ID() ID          { return c.id }
func (c *counterView) Rect() Rect      { return c.rect }
func (c *counterView) MinSize() Size   { return Size{W: 20, H: 3} }
func (c *counterView) Layout(r Rect)   { c.rect = r }
func (c *counterView) Focusable() bool { return true }

func (c *counterView) Paint(d Drawer, _ *Ctx) {
	if c.rect.W <= 0 || c.rect.H <= 0 {
		return
	}

	line := "counter = "
	d.DrawText(Point{X: c.rect.X, Y: c.rect.Y}, line, Style{})

	num := []byte{}
	v := c.state.Value()
	if v == 0 {
		num = []byte("0")
	} else {
		neg := false
		if v < 0 {
			neg = true
			v = -v
		}
		for v > 0 {
			num = append([]byte{byte('0' + v%10)}, num...)
			v /= 10
		}
		if neg {
			num = append([]byte{'-'}, num...)
		}
	}
	d.DrawText(Point{X: c.rect.X + len(line), Y: c.rect.Y}, string(num), Style{})
}

func (c *counterView) Handle(e Event, ctx *Ctx) bool {
	ke, ok := e.(KeyEvent)
	if !ok {
		return false
	}

	if ke.Key != KeyRune {
		return false
	}

	switch ke.Rune {
	case '+', '=':
		c.state.add(1, "incremented")
		if ctx != nil && ctx.Invalidate != nil {
			ctx.Invalidate(c.rect)
		}

		return true
	case '-', '_':
		c.state.add(-1, "decremented")
		if ctx != nil && ctx.Invalidate != nil {
			ctx.Invalidate(c.rect)
		}

		return true
	}

	return false
}

// syncBuffer is a bytes.Buffer that is safe to write from the app loop while
// the test goroutine reads it. Tests that hand the app a mirror writer and then
// assert on its contents mid-run need this; a bare bytes.Buffer races.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}
