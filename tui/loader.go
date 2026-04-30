package tui

import (
	"context"
	"sync"
	"time"

	"github.com/losinggeneration/rovel/geom"
	"github.com/losinggeneration/rovel/style"
	"github.com/losinggeneration/rovel/text"
)

// LoaderFrameInterval is the default spinner frame interval used by Loader.
const LoaderFrameInterval = 80 * time.Millisecond

var defaultLoaderFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// LoaderOption configures a Loader.
type LoaderOption func(*Loader)

// WithLoaderMessage sets the loader message.
func WithLoaderMessage(message string) LoaderOption {
	return func(l *Loader) {
		l.message = message
	}
}

// WithLoaderFrames replaces the spinner frames. Empty frame lists are ignored.
func WithLoaderFrames(frames []string) LoaderOption {
	return func(l *Loader) {
		if len(frames) == 0 {
			return
		}
		l.frames = append([]string(nil), frames...)
	}
}

// WithLoaderInterval sets the spinner frame interval. Non-positive intervals
// disable the background ticker while still allowing manual AdvanceFrame calls.
func WithLoaderInterval(interval time.Duration) LoaderOption {
	return func(l *Loader) {
		l.interval = interval
	}
}

// WithLoaderInvalidate sets a callback invoked whenever the loader changes its
// rendered display. App integrations should use this to request a render.
func WithLoaderInvalidate(fn func()) LoaderOption {
	return func(l *Loader) {
		l.invalidate = fn
	}
}

// WithLoaderStyle sets the style used when painting the loader text.
func WithLoaderStyle(s style.Style) LoaderOption {
	return func(l *Loader) {
		l.style = s
	}
}

// Loader renders an animated spinner plus message. It mirrors the Pi TUI
// loader behavior: it auto-starts, advances frames on an 80ms interval by
// default, renders a leading blank row before the spinner row, and invalidates
// its owner whenever the message or frame changes.
type Loader struct {
	id ID

	mu         sync.Mutex
	frames     []string
	frame      int
	message    string
	interval   time.Duration
	invalidate func()
	style      style.Style
	rect       Rect

	stopOnce sync.Once
	stopCh   chan struct{}
}

// NewLoader returns and starts a Loader.
func NewLoader(opts ...LoaderOption) *Loader {
	l := &Loader{
		id:       NewID(),
		frames:   append([]string(nil), defaultLoaderFrames...),
		message:  "Loading...",
		interval: LoaderFrameInterval,
		stopCh:   make(chan struct{}),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(l)
		}
	}
	l.Start()

	return l
}

// Start starts the background spinner ticker when enabled. NewLoader calls
// Start automatically; calling Start again after construction is a no-op.
func (l *Loader) Start() {
	if l == nil || l.interval <= 0 {
		l.requestInvalidate()
		return
	}

	go l.run()
	l.requestInvalidate()
}

func (l *Loader) run() {
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			l.AdvanceFrame()
		case <-l.stopCh:
			return
		}
	}
}

// Stop stops the spinner ticker. It is safe to call more than once.
func (l *Loader) Stop() {
	if l == nil {
		return
	}
	l.stopOnce.Do(func() { close(l.stopCh) })
}

// SetMessage changes the loader message and requests a render.
func (l *Loader) SetMessage(message string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.message = message
	l.mu.Unlock()
	l.requestInvalidate()
}

// Message returns the current loader message.
func (l *Loader) Message() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.message
}

// AdvanceFrame advances the spinner one frame and requests a render. Tests and
// deterministic apps can use this with WithLoaderInterval(0).
func (l *Loader) AdvanceFrame() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if len(l.frames) > 0 {
		l.frame = (l.frame + 1) % len(l.frames)
	}
	l.mu.Unlock()
	l.requestInvalidate()
}

// Render returns a leading blank row followed by the padded spinner/message row.
func (l *Loader) Render(width int) []string {
	if l == nil || width <= 0 {
		return nil
	}
	l.mu.Lock()
	frame := ""
	if len(l.frames) > 0 {
		frame = l.frames[l.frame%len(l.frames)]
	}
	message := l.message
	l.mu.Unlock()

	available := width - 2
	if available < 1 {
		available = 1
	}
	line := " " + ANSITruncate(frame+" "+message, available, false) + " "

	return []string{"", padRightNoEllipsis(line, width)}
}

func (l *Loader) requestInvalidate() {
	if l != nil && l.invalidate != nil {
		l.invalidate()
	}
}

// ID implements View.
func (l *Loader) ID() ID {
	if l == nil {
		return 0
	}

	return l.id
}

// Rect implements View.
func (l *Loader) Rect() Rect {
	if l == nil {
		return Rect{}
	}

	return l.rect
}

// MinSize implements View.
func (l *Loader) MinSize() Size {
	if l == nil {
		return Size{}
	}
	l.mu.Lock()
	line := ""
	if len(l.frames) > 0 {
		line = l.frames[l.frame%len(l.frames)] + " " + l.message
	}
	l.mu.Unlock()

	w := text.Width(line) + 2
	if w < 1 {
		w = 1
	}

	return Size{W: w, H: 2}
}

// Layout implements View.
func (l *Loader) Layout(r Rect) {
	if l != nil {
		l.rect = r
	}
}

// Focusable implements View.
func (l *Loader) Focusable() bool { return false }

// Handle implements View.
func (l *Loader) Handle(_ Event, _ *Ctx) bool { return false }

// Paint implements View.
func (l *Loader) Paint(d Drawer, _ *Ctx) {
	if l == nil || d == nil {
		return
	}
	for i, row := range l.Render(l.rect.W) {
		y := l.rect.Y + i
		if y >= l.rect.Y+l.rect.H {
			break
		}
		d.DrawText(geom.Point{X: l.rect.X, Y: y}, row, l.style)
	}
}

// CancellableLoader is a Loader that aborts an associated context when the
// user presses the select/input cancel key (Escape or Ctrl-C in tau's key model).
type CancellableLoader struct {
	*Loader

	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once

	// OnAbort is called once when the loader is cancelled by input or Abort.
	OnAbort func()
}

// NewCancellableLoader returns a started cancellable loader.
func NewCancellableLoader(opts ...LoaderOption) *CancellableLoader {
	ctx, cancel := context.WithCancel(context.Background())

	return &CancellableLoader{
		Loader: NewLoader(opts...),
		ctx:    ctx,
		cancel: cancel,
	}
}

// Context returns a context cancelled when the loader is aborted.
func (l *CancellableLoader) Context() context.Context {
	if l == nil || l.ctx == nil {
		return context.Background()
	}

	return l.ctx
}

// Aborted reports whether the loader has been aborted.
func (l *CancellableLoader) Aborted() bool {
	if l == nil || l.ctx == nil {
		return false
	}

	return l.ctx.Err() != nil
}

// Abort aborts the loader and calls OnAbort once.
func (l *CancellableLoader) Abort() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.cancel()
		if l.OnAbort != nil {
			l.OnAbort()
		}
	})
}

// Handle aborts on cancel keys.
func (l *CancellableLoader) Handle(e Event, _ *Ctx) bool {
	ke, ok := e.(KeyEvent)
	if !ok {
		return false
	}
	if ke.Key != KeyEsc && ke.Key != KeyCtrlC {
		return false
	}

	l.Abort()

	return true
}

// Dispose stops the spinner ticker.
func (l *CancellableLoader) Dispose() {
	if l != nil && l.Loader != nil {
		l.Stop()
	}
}
