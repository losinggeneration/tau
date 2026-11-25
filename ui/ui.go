// Package ui defines the UI interface that bare tau satisfies trivially
// and the future TUI module satisfies fully.
package ui // import "github.com/losinggeneration/tau/ui"

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// NotifyLevel represents the severity level of a notification.
type NotifyLevel int

const (
	// NotifyInfo is an informational notification.
	NotifyInfo NotifyLevel = iota
	// NotifySuccess indicates a successful operation.
	NotifySuccess
	// NotifyWarn indicates a warning.
	NotifyWarn
	// NotifyError indicates an error.
	NotifyError
)

// UIMethod identifies a dialog method. Mirrors Pi's discriminated-union
// `method` field on extension_ui_request events.
type UIMethod string

const (
	// UIMethodSelect is the option-list chooser.
	UIMethodSelect UIMethod = "select"
	// UIMethodConfirm is the yes/no confirmation.
	UIMethodConfirm UIMethod = "confirm"
	// UIMethodInput is the freeform text input.
	UIMethodInput UIMethod = "input"
	// UIMethodEditor is the multi-line editor dialog.
	UIMethodEditor UIMethod = "editor"
	// UIMethodNotify is a fire-and-forget notification.
	UIMethodNotify UIMethod = "notify"
	// UIMethodSetStatus is a fire-and-forget status-line update.
	UIMethodSetStatus UIMethod = "setStatus"
	// UIMethodSetWidget is a fire-and-forget widget update.
	UIMethodSetWidget UIMethod = "setWidget"
	// UIMethodSetTitle is a fire-and-forget terminal title update.
	UIMethodSetTitle UIMethod = "setTitle"
	// UIMethodSetEditorText is a fire-and-forget editor buffer update.
	UIMethodSetEditorText UIMethod = "set_editor_text"
)

// UIRequest is the typed payload for a ui_* / extension_ui_request event.
// Mirrors Pi's discriminated-union shape: the Method discriminator picks
// which fields are meaningful. Optional fields use `,omitempty` so the JSON
// encoding for each method contains only the relevant subset.
//
//	select:    Method, ID, Title, Options, [Timeout]
//	confirm:   Method, ID, Title, Message, [Timeout]
//	input:     Method, ID, Title, [Placeholder], [Timeout]
//	editor:    Method, ID, Title, [Prefill]
//	notify:    Method, Message, [NotifyType]
//	setStatus: Method, StatusKey, StatusText (string | undefined)
//	setWidget: Method, WidgetKey, WidgetLines, [WidgetPlacement]
//	setTitle:  Method, Title
//	set_editor_text: Method, Text
type UIRequest struct {
	Method UIMethod `json:"method"`
	ID     string   `json:"id,omitempty"`

	// Common (select / confirm / input / editor).
	Title string `json:"title,omitempty"`

	// select.
	Options []string `json:"options,omitempty"`

	// confirm.
	Message string `json:"message,omitempty"`

	// input.
	Placeholder string `json:"placeholder,omitempty"`

	// editor.
	Prefill string `json:"prefill,omitempty"`

	// Optional timeout (seconds) for select / confirm / input.
	Timeout int `json:"timeout,omitempty"`

	// notify (Level = 0..3 matches ui.NotifyLevel).
	Level *int `json:"level,omitempty"`

	// setStatus.
	StatusKey  string  `json:"statusKey,omitempty"`
	StatusText *string `json:"statusText,omitempty"` // pointer so nil != ""

	// setWidget.
	WidgetKey       string   `json:"widgetKey,omitempty"`
	WidgetLines     []string `json:"widgetLines,omitempty"`
	WidgetPlacement string   `json:"widgetPlacement,omitempty"` // "aboveEditor" | "belowEditor"

	// setTitle / set_editor_text.
	Text string `json:"text,omitempty"`
}

// UIResponse represents a client reply to a ui_* event. Mirrors Pi's
// discriminated union `extension_ui_response` payload with optional fields:
//
//	select  -> Value     (selected option string)
//	confirm -> Confirmed (true / false)
//	input   -> Value     (entered text)
//	editor  -> Value     (entered text) or Cancelled=true
//	any     -> Cancelled=true means user dismissed the dialog
//
// The Method field lets callers (and the dispatcher in JSONEventUI/RPCUI)
// route the response to the right consumer without inspecting which Go
// field is populated. Type is the wire envelope discriminator ("ui_response").
type UIResponse struct {
	Type   string   `json:"type,omitempty"` // wire envelope discriminator ("ui_response")
	ID     string   `json:"id"`
	Method UIMethod `json:"method,omitempty"`
	// Value is the selected option (select) or entered text (input/editor).
	Value string `json:"value,omitempty"`
	// Confirmed is the yes/no answer for confirm dialogs. Pointer so
	// `false` round-trips through JSON and "unset" is distinguishable from
	// "explicitly false".
	Confirmed *bool `json:"confirmed,omitempty"`
	// Cancelled is true if the user dismissed the dialog (any method).
	Cancelled bool `json:"cancelled,omitempty"`
}

// NewUIResponseValue builds a response carrying a string value (select/input/editor).
func NewUIResponseValue(id string, method UIMethod, value string) UIResponse {
	return UIResponse{Type: "ui_response", ID: id, Method: method, Value: value}
}

// NewUIResponseConfirmed builds a response carrying a yes/no answer (confirm).
func NewUIResponseConfirmed(id string, method UIMethod, confirmed bool) UIResponse {
	return UIResponse{Type: "ui_response", ID: id, Method: method, Confirmed: &confirmed}
}

// NewUIResponseCancelled builds a response indicating the user dismissed the dialog.
func NewUIResponseCancelled(id string, method UIMethod) UIResponse {
	return UIResponse{Type: "ui_response", ID: id, Method: method, Cancelled: true}
}

// UI is the interface for user interaction. Implementations range from
// headless no-op to full TUI. Plugin Host.UI* calls route through
// whichever UI the runtime was constructed with.
type UI interface {
	// Confirm asks the user a yes/no question.
	Confirm(ctx context.Context, title, message string) (bool, error)
	// Select asks the user to choose from a list of options.
	Select(ctx context.Context, title string, options []string) (int, error)
	// Input asks the user for freeform text input.
	Input(ctx context.Context, title, placeholder string) (string, error)
	// Notify displays a notification to the user.
	Notify(level NotifyLevel, message string)
	// SetStatus updates a status line identified by key.
	SetStatus(key, message string)
	// SetWidget updates a widget identified by key with the given lines.
	SetWidget(key string, lines []string)
	// SetTitle updates the terminal title.
	SetTitle(title string)
	// SetEditorText replaces the prompt editor text.
	SetEditorText(text string)
	// SetToolsExpanded expands or collapses rendered tool details.
	SetToolsExpanded(expanded bool)
}

// NoOpUI is a headless UI implementation. Confirm returns the configured
// default (deny by default), Select returns -1, Input returns an error,
// and Notify, SetStatus, and SetWidget are no-ops.
type NoOpUI struct {
	// ConfirmDefault controls what Confirm returns. Defaults to false (deny).
	ConfirmDefault bool
	// Writer receives Notify output. Defaults to os.Stderr.
	Writer io.Writer
}

// NewNoOpUI creates a NoOpUI with defaults.
func NewNoOpUI() *NoOpUI {
	return &NoOpUI{
		Writer: io.Discard,
	}
}

// Confirm returns the configured default value.
func (u *NoOpUI) Confirm(_ context.Context, _, _ string) (bool, error) {
	return u.ConfirmDefault, nil
}

// Select returns -1 (no selection).
func (u *NoOpUI) Select(_ context.Context, _ string, _ []string) (int, error) {
	return -1, nil
}

// ErrNoInput is returned by headless UI implementations when no input source
// is available. Callers can use errors.Is to detect this condition.
var ErrNoInput = errors.New("no input available in headless mode")

// Input returns an error indicating no input is available.
func (u *NoOpUI) Input(_ context.Context, _, _ string) (string, error) {
	return "", ErrNoInput
}

// Notify writes the notification to the configured writer.
func (u *NoOpUI) Notify(level NotifyLevel, message string) {
	w := u.Writer
	if w == nil {
		w = os.Stderr
	}
	var prefix string
	switch level {
	case NotifyInfo:
		prefix = "INFO"
	case NotifySuccess:
		prefix = "OK"
	case NotifyWarn:
		prefix = "WARN"
	case NotifyError:
		prefix = "ERROR"
	}
	_, _ = fmt.Fprintf(w, "[%s] %s\n", prefix, message)
}

// SetStatus is a no-op.
func (u *NoOpUI) SetStatus(_, _ string) {}

// SetWidget is a no-op.
func (u *NoOpUI) SetWidget(_ string, _ []string) {}
func (u *NoOpUI) SetTitle(_ string)              {}
func (u *NoOpUI) SetEditorText(_ string)         {}
func (u *NoOpUI) SetToolsExpanded(_ bool)        {}

// StdinUI prompts on stderr and reads from stdin. Used for subcommand
// modes when stdin is a TTY.
type StdinUI struct {
	// Reader is the input source. Defaults to os.Stdin.
	Reader io.Reader
	// Writer is the output for prompts and notifications. Defaults to os.Stderr.
	Writer io.Writer
}

// NewStdinUI creates a StdinUI with defaults.
func NewStdinUI() *StdinUI {
	return &StdinUI{
		Reader: os.Stdin,
		Writer: os.Stderr,
	}
}

// Confirm prompts the user for a yes/no answer.
func (u *StdinUI) Confirm(_ context.Context, title, message string) (bool, error) {
	w := u.Writer
	if w == nil {
		w = os.Stderr
	}
	_, _ = fmt.Fprintf(w, "%s: %s [y/N] ", title, message)

	r := u.Reader
	if r == nil {
		r = os.Stdin
	}

	var response string
	if _, err := fmt.Fscanln(r, &response); err != nil {
		return false, fmt.Errorf("reading confirm input: %w", err)
	}
	return strings.EqualFold(strings.TrimSpace(response), "y"), nil
}

// Select prompts the user to choose from options.
func (u *StdinUI) Select(_ context.Context, title string, options []string) (int, error) {
	w := u.Writer
	if w == nil {
		w = os.Stderr
	}
	_, _ = fmt.Fprintf(w, "%s:\n", title)
	for i, opt := range options {
		_, _ = fmt.Fprintf(w, "  %d. %s\n", i+1, opt)
	}
	_, _ = fmt.Fprintf(w, "Enter choice [1-%d]: ", len(options))

	r := u.Reader
	if r == nil {
		r = os.Stdin
	}

	var choice int
	if _, err := fmt.Fscanln(r, &choice); err != nil {
		return -1, fmt.Errorf("reading select input: %w", err)
	}
	if choice < 1 || choice > len(options) {
		return -1, fmt.Errorf("choice %d out of range [1-%d]", choice, len(options))
	}
	return choice - 1, nil
}

// Input prompts the user for freeform text.
func (u *StdinUI) Input(_ context.Context, title, placeholder string) (string, error) {
	w := u.Writer
	if w == nil {
		w = os.Stderr
	}
	if placeholder != "" {
		_, _ = fmt.Fprintf(w, "%s [%s]: ", title, placeholder)
	} else {
		_, _ = fmt.Fprintf(w, "%s: ", title)
	}

	r := u.Reader
	if r == nil {
		r = os.Stdin
	}

	var input string
	if _, err := fmt.Fscanln(r, &input); err != nil {
		return "", fmt.Errorf("reading input: %w", err)
	}
	input = strings.TrimSpace(input)
	if input == "" && placeholder != "" {
		return placeholder, nil
	}
	return input, nil
}

// Notify writes the notification to the configured writer.
func (u *StdinUI) Notify(level NotifyLevel, message string) {
	w := u.Writer
	if w == nil {
		w = os.Stderr
	}
	var prefix string
	switch level {
	case NotifyInfo:
		prefix = "INFO"
	case NotifySuccess:
		prefix = "OK"
	case NotifyWarn:
		prefix = "WARN"
	case NotifyError:
		prefix = "ERROR"
	}
	_, _ = fmt.Fprintf(w, "[%s] %s\n", prefix, message)
}

// SetStatus writes a status line to the configured writer.
func (u *StdinUI) SetStatus(key, message string) {
	w := u.Writer
	if w == nil {
		w = os.Stderr
	}
	_, _ = fmt.Fprintf(w, "[%s] %s\n", key, message)
}

// SetWidget writes widget lines to the configured writer.
func (u *StdinUI) SetWidget(key string, lines []string) {
	w := u.Writer
	if w == nil {
		w = os.Stderr
	}
	_, _ = fmt.Fprintf(w, "[%s] %s\n", key, strings.Join(lines, "\n"))
}
func (u *StdinUI) SetTitle(_ string)       {}
func (u *StdinUI) SetEditorText(_ string)  {}
func (u *StdinUI) SetToolsExpanded(_ bool) {}

// JSONEventUI implements --json print mode. Every UI call becomes a ui_*
// event written as LF-delimited JSONL to stdout. Consumer replies via stdin
// ui_response lines. Call ReadResponse in a goroutine to process incoming responses.
type JSONEventUI struct {
	// Writer writes JSON events. Defaults to os.Stdout.
	Writer io.Writer
	// Reader reads ui_response lines. Defaults to os.Stdin.
	Reader io.Reader

	// SessionID is the current session ID, included in all events.
	SessionID string
	// Seq is a monotonic counter for event sequencing.
	Seq int64

	// pending tracks pending UI calls awaiting a response.
	// Key is the event ID; value is a chan that receives a typed UIResponse.
	pending map[string]chan<- UIResponse

	// scanCtx is used to cancel the scanner when context is done.
	scanCtx    context.Context
	scanCancel context.CancelFunc
	mu         sync.Mutex
}

// NewJSONEventUI creates a JSONEventUI with defaults.
func NewJSONEventUI() *JSONEventUI {
	return &JSONEventUI{
		Writer:  os.Stdout,
		Reader:  os.Stdin,
		pending: make(map[string]chan<- UIResponse),
	}
}

// emit writes a JSON event with LF delimiter and standard envelope fields.
// It blocks until the write completes or the context is cancelled.
func (u *JSONEventUI) emit(ctx context.Context, event map[string]any) error {
	u.mu.Lock()
	event["session_id"] = u.SessionID
	event["seq"] = u.Seq
	event["ts"] = time.Now().Format(time.RFC3339Nano)
	u.Seq++
	u.mu.Unlock()

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	data = append(data, '\n')
	_, err = u.Writer.Write(data)
	return err
}

// Confirm emits a ui_confirm event and waits for a ui_response on stdin.
// Blocks until the user responds or context is cancelled.
func (u *JSONEventUI) Confirm(ctx context.Context, title, message string) (bool, error) {
	id := fmt.Sprintf("ui-%d", time.Now().UnixNano())
	respCh := make(chan UIResponse, 1)

	u.mu.Lock()
	u.pending[id] = respCh
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		delete(u.pending, id)
		u.mu.Unlock()
	}()

	err := u.emit(ctx, map[string]any{
		"type":    "ui_confirm",
		"id":      id,
		"title":   title,
		"message": message,
	})
	if err != nil {
		return false, err
	}

	select {
	case resp := <-respCh:
		return confirmResult(resp)
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// Select emits a ui_select event and waits for a ui_response.
func (u *JSONEventUI) Select(ctx context.Context, title string, options []string) (int, error) {
	id := fmt.Sprintf("ui-%d", time.Now().UnixNano())
	respCh := make(chan UIResponse, 1)

	u.mu.Lock()
	u.pending[id] = respCh
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		delete(u.pending, id)
		u.mu.Unlock()
	}()

	err := u.emit(ctx, map[string]any{
		"type":    "ui_select",
		"id":      id,
		"title":   title,
		"options": options,
	})
	if err != nil {
		return -1, err
	}

	select {
	case resp := <-respCh:
		return selectResult(resp, options)
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

// Input emits a ui_input event and waits for a ui_response.
func (u *JSONEventUI) Input(ctx context.Context, title, placeholder string) (string, error) {
	id := fmt.Sprintf("ui-%d", time.Now().UnixNano())
	respCh := make(chan UIResponse, 1)

	u.mu.Lock()
	u.pending[id] = respCh
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		delete(u.pending, id)
		u.mu.Unlock()
	}()

	err := u.emit(ctx, map[string]any{
		"type":        "ui_input",
		"id":          id,
		"title":       title,
		"placeholder": placeholder,
	})
	if err != nil {
		return "", err
	}

	select {
	case resp := <-respCh:
		if resp.Cancelled {
			return "", ErrNoInput
		}
		return resp.Value, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Notify emits a ui_notify event (no response needed).
func (u *JSONEventUI) Notify(level NotifyLevel, message string) {
	ctx := context.Background()
	_ = u.emit(ctx, map[string]any{
		"type":    "ui_notify",
		"level":   int(level),
		"message": message,
	})
}

// SetStatus emits a ui_set_status event (no response needed).
func (u *JSONEventUI) SetStatus(key, message string) {
	ctx := context.Background()
	_ = u.emit(ctx, map[string]any{
		"type":    "ui_set_status",
		"key":     key,
		"message": message,
	})
}

// SetWidget emits a ui_set_widget event (no response needed).
func (u *JSONEventUI) SetWidget(key string, lines []string) {
	ctx := context.Background()
	_ = u.emit(ctx, map[string]any{
		"type":  "ui_set_widget",
		"key":   key,
		"lines": lines,
	})
}

func (u *JSONEventUI) SetTitle(title string) {
	ctx := context.Background()
	_ = u.emit(ctx, map[string]any{"type": "ui_set_title", "title": title})
}

func (u *JSONEventUI) SetEditorText(text string) {
	ctx := context.Background()
	_ = u.emit(ctx, map[string]any{"type": "ui_set_editor_text", "text": text})
}

func (u *JSONEventUI) SetToolsExpanded(expanded bool) {
	ctx := context.Background()
	_ = u.emit(ctx, map[string]any{"type": "ui_set_tools_expanded", "expanded": expanded})
}

// ReadResponse starts reading ui_response lines from stdin in a background
// goroutine. It returns immediately. The scanning runs until Close is called
// or the context is cancelled.
func (u *JSONEventUI) ReadResponse(ctx context.Context) {
	u.scanCtx, u.scanCancel = context.WithCancel(ctx)
	go u.runScanner()
}

// runScanner reads lines from stdin and dispatches them to pending channels.
func (u *JSONEventUI) runScanner() {
	defer func() {
		if u.scanCancel != nil {
			u.scanCancel()
		}
	}()

	if u.Reader == nil {
		u.Reader = os.Stdin
	}

	scanner := bufio.NewScanner(u.Reader)
	for {
		select {
		case <-u.scanCtx.Done():
			return
		default:
		}

		// Set a timeout for scanning so we can check context periodically
		if !scanner.Scan() {
			return
		}

		line := scanner.Text()
		if line == "" {
			continue
		}

		var msg UIResponse
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue // ignore malformed lines
		}
		if msg.ID == "" {
			continue
		}
		if msg.Type != "ui_response" {
			continue
		}

		u.mu.Lock()
		if ch, ok := u.pending[msg.ID]; ok {
			select {
			case ch <- msg:
			default:
			}
		}
		u.mu.Unlock()
	}
}

// Close cancels the response reader and cleans up resources.
func (u *JSONEventUI) Close() error {
	if u.scanCancel != nil {
		u.scanCancel()
	}
	return nil
}

// RPCUI publishes UI calls as events and accepts typed replies. A coordinated
// worker enables shared requests so any attached client can answer.
type RPCUI struct {
	// Writer writes JSON events. Defaults to os.Stdout.
	Writer io.Writer
	// Reader reads ui_response lines. Defaults to os.Stdin.
	Reader io.Reader

	// SessionID is the current session ID, included in all events.
	SessionID string
	// Seq is a monotonic counter for event sequencing.
	Seq int64

	// pending tracks pending UI calls awaiting a response.
	// Key is the event ID; value is a chan that receives a typed UIResponse.
	pending map[string]chan<- UIResponse

	// scanCtx is used to cancel the scanner when context is done.
	scanCtx    context.Context
	scanCancel context.CancelFunc
	mu         sync.Mutex

	// requests holds shared dialogs once EnableSharedRequests opts in.
	requests map[string]*sharedRequest
	nextID   uint64
}

// SharedRequest is the complete descriptor of a dialog awaiting an answer
// from any attached client.
type SharedRequest struct {
	ID          string   `json:"id"`
	Method      UIMethod `json:"method"`
	Title       string   `json:"title"`
	Message     string   `json:"message,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Options     []string `json:"options,omitempty"`
}

// Outcomes reported by ui_resolved events. They never carry the answer.
const (
	OutcomeAnswered    = "answered"
	OutcomeCancelled   = "cancelled"
	OutcomeInterrupted = "interrupted"
	OutcomeTimeout     = "timeout"
	OutcomeAborted     = "aborted"
	OutcomeFailed      = "failed"
)

var (
	// ErrInvalidResponse reports a reply that cannot resolve its request.
	// The request stays pending.
	ErrInvalidResponse = errors.New("invalid UI response")
	// ErrUIClosed reports a shared request abandoned by Close.
	ErrUIClosed    = errors.New("UI closed")
	errInterrupted = fmt.Errorf("UI request interrupted: %w", context.Canceled)
)

type sharedReply struct {
	resp    UIResponse
	outcome string
	err     error
}

type sharedRequest struct {
	desc  SharedRequest
	order uint64
	// reply has room for the single delivery made by whoever removes the
	// request from RPCUI.requests, so delivery never blocks under mu.
	reply chan sharedReply
}

func (r SharedRequest) event() map[string]any {
	event := map[string]any{"type": "ui_" + string(r.Method), "id": r.ID, "title": r.Title}
	switch r.Method {
	case UIMethodConfirm:
		event["message"] = r.Message
	case UIMethodSelect:
		event["options"] = r.Options
	case UIMethodInput:
		event["placeholder"] = r.Placeholder
	}
	return event
}

func (r SharedRequest) validate(resp UIResponse) error {
	if resp.Method != "" && resp.Method != r.Method {
		return fmt.Errorf("%w: method %q does not match %q", ErrInvalidResponse, resp.Method, r.Method)
	}
	if resp.Cancelled {
		return nil
	}
	switch r.Method {
	case UIMethodConfirm:
		if resp.Confirmed == nil {
			return fmt.Errorf("%w: confirm requires confirmed or cancelled", ErrInvalidResponse)
		}
	case UIMethodSelect:
		if resp.Value == "" || !slices.Contains(r.Options, resp.Value) {
			return fmt.Errorf("%w: select requires one of the options", ErrInvalidResponse)
		}
	}
	return nil
}

func confirmResult(resp UIResponse) (bool, error) {
	switch {
	case resp.Cancelled:
		return false, nil
	case resp.Confirmed == nil:
		return false, fmt.Errorf("%w: confirm requires confirmed or cancelled", ErrInvalidResponse)
	}
	return *resp.Confirmed, nil
}

func selectResult(resp UIResponse, options []string) (int, error) {
	if resp.Cancelled {
		return -1, nil
	}
	if i := slices.Index(options, resp.Value); resp.Value != "" && i >= 0 {
		return i, nil
	}
	return -1, fmt.Errorf("%w: select requires one of the options, got %q", ErrInvalidResponse, resp.Value)
}

func inputResult(resp UIResponse) (string, error) {
	if resp.Cancelled {
		return "", ErrNoInput
	}
	return resp.Value, nil
}

// EnableSharedRequests switches Confirm, Select, and Input to requests that
// any attached client may answer once. Without it, RPCUI keeps its
// connection-owned reply semantics.
func (u *RPCUI) EnableSharedRequests() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.requests == nil {
		u.requests = make(map[string]*sharedRequest)
	}
}

func (u *RPCUI) shared() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests != nil
}

// ask publishes desc and waits until it is resolved. The goroutine that
// removes the request from u.requests decides its outcome, and the waiter
// publishes ui_resolved before returning so later events follow it.
func (u *RPCUI) ask(ctx context.Context, desc SharedRequest) (UIResponse, error) {
	const maxSharedRequests, maxDescriptorBytes = 16, 8 << 10
	if len(desc.Options) > 256 {
		return UIResponse{}, fmt.Errorf("too many shared request options")
	}
	data, err := json.Marshal(desc)
	if err != nil {
		return UIResponse{}, fmt.Errorf("encode shared request: %w", err)
	}
	if len(data) > maxDescriptorBytes {
		return UIResponse{}, fmt.Errorf("shared request exceeds size limit")
	}
	req := &sharedRequest{reply: make(chan sharedReply, 1)}
	u.mu.Lock()
	if len(u.requests) >= maxSharedRequests {
		u.mu.Unlock()
		return UIResponse{}, fmt.Errorf("too many pending shared requests")
	}
	u.nextID++
	req.order = u.nextID
	desc.ID = fmt.Sprintf("ui-%d-%d", time.Now().UnixNano(), u.nextID)
	req.desc = desc
	u.requests[desc.ID] = req
	u.mu.Unlock()

	if err := u.emit(ctx, desc.event()); err != nil {
		if u.release(desc.ID) {
			return u.settle(desc, sharedReply{outcome: OutcomeFailed, err: err})
		}
		return u.settle(desc, <-req.reply)
	}
	var reply sharedReply
	select {
	case reply = <-req.reply:
	case <-ctx.Done():
		if !u.release(desc.ID) {
			// A resolver won the race; honor the answer it was told succeeded.
			reply = <-req.reply
			break
		}
		reply = sharedReply{outcome: OutcomeAborted, err: ctx.Err()}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			reply.outcome = OutcomeTimeout
		}
	}
	return u.settle(desc, reply)
}

func (u *RPCUI) settle(desc SharedRequest, reply sharedReply) (UIResponse, error) {
	// The request is already resolved; a lost notice must not undo that.
	_ = u.emit(context.Background(), map[string]any{
		"type":    "ui_resolved",
		"id":      desc.ID,
		"method":  string(desc.Method),
		"outcome": reply.outcome,
	})
	return reply.resp, reply.err
}

// release removes id and reports whether the caller won the resolution.
func (u *RPCUI) release(id string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, ok := u.requests[id]; !ok {
		return false
	}
	delete(u.requests, id)
	return true
}

// Resolve applies the first valid reply to a shared request. Unknown or
// already-resolved IDs report false without error. An invalid reply returns
// ErrInvalidResponse and leaves the request pending.
func (u *RPCUI) Resolve(resp UIResponse) (bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	req, ok := u.requests[resp.ID]
	if !ok {
		return false, nil
	}
	if err := req.desc.validate(resp); err != nil {
		return false, err
	}
	delete(u.requests, resp.ID)
	outcome := OutcomeAnswered
	if resp.Cancelled {
		outcome = OutcomeCancelled
	}
	req.reply <- sharedReply{resp: resp, outcome: outcome}
	return true, nil
}

// Requests snapshots outstanding shared requests in creation order.
func (u *RPCUI) Requests() []SharedRequest {
	u.mu.Lock()
	pending := make([]*sharedRequest, 0, len(u.requests))
	for _, req := range u.requests {
		pending = append(pending, req)
	}
	u.mu.Unlock()
	slices.SortFunc(pending, func(a, b *sharedRequest) int { return cmp.Compare(a.order, b.order) })
	out := make([]SharedRequest, len(pending))
	for i, req := range pending {
		out[i] = req.desc
		out[i].Options = slices.Clone(req.desc.Options)
	}
	return out
}

// abandonLocked resolves every shared request with reply. Caller holds mu.
func (u *RPCUI) abandonLocked(reply sharedReply) {
	for id, req := range u.requests {
		delete(u.requests, id)
		req.reply <- reply
	}
}

// NewRPCUI creates a RPCUI with defaults.
func NewRPCUI() *RPCUI {
	return &RPCUI{
		Writer:  os.Stdout,
		Reader:  os.Stdin,
		pending: make(map[string]chan<- UIResponse),
	}
}

// emit writes a JSON event with LF delimiter and standard envelope fields.
func (u *RPCUI) emit(ctx context.Context, event map[string]any) error {
	u.mu.Lock()
	event["session_id"] = u.SessionID
	event["seq"] = u.Seq
	event["ts"] = time.Now().Format(time.RFC3339Nano)
	u.Seq++
	u.mu.Unlock()

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	data = append(data, '\n')
	_, err = u.Writer.Write(data)
	return err
}

// Confirm emits a ui_confirm event and waits for a ui_response.
func (u *RPCUI) Confirm(ctx context.Context, title, message string) (bool, error) {
	if u.shared() {
		resp, err := u.ask(ctx, SharedRequest{Method: UIMethodConfirm, Title: title, Message: message})
		if err != nil {
			return false, err
		}
		return confirmResult(resp)
	}
	id := fmt.Sprintf("ui-%d", time.Now().UnixNano())
	respCh := make(chan UIResponse, 1)

	u.mu.Lock()
	u.pending[id] = respCh
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		delete(u.pending, id)
		u.mu.Unlock()
	}()

	err := u.emit(ctx, map[string]any{
		"type":    "ui_confirm",
		"id":      id,
		"title":   title,
		"message": message,
	})
	if err != nil {
		return false, err
	}

	select {
	case resp := <-respCh:
		return confirmResult(resp)
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// Select emits a ui_select event and waits for a ui_response.
func (u *RPCUI) Select(ctx context.Context, title string, options []string) (int, error) {
	if u.shared() {
		resp, err := u.ask(ctx, SharedRequest{Method: UIMethodSelect, Title: title, Options: slices.Clone(options)})
		if err != nil {
			return -1, err
		}
		return selectResult(resp, options)
	}
	id := fmt.Sprintf("ui-%d", time.Now().UnixNano())
	respCh := make(chan UIResponse, 1)

	u.mu.Lock()
	u.pending[id] = respCh
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		delete(u.pending, id)
		u.mu.Unlock()
	}()

	err := u.emit(ctx, map[string]any{
		"type":    "ui_select",
		"id":      id,
		"title":   title,
		"options": options,
	})
	if err != nil {
		return -1, err
	}

	select {
	case resp := <-respCh:
		return selectResult(resp, options)
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

// Input emits a ui_input event and waits for a ui_response.
func (u *RPCUI) Input(ctx context.Context, title, placeholder string) (string, error) {
	if u.shared() {
		resp, err := u.ask(ctx, SharedRequest{Method: UIMethodInput, Title: title, Placeholder: placeholder})
		if err != nil {
			return "", err
		}
		return inputResult(resp)
	}
	id := fmt.Sprintf("ui-%d", time.Now().UnixNano())
	respCh := make(chan UIResponse, 1)

	u.mu.Lock()
	u.pending[id] = respCh
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		delete(u.pending, id)
		u.mu.Unlock()
	}()

	err := u.emit(ctx, map[string]any{
		"type":        "ui_input",
		"id":          id,
		"title":       title,
		"placeholder": placeholder,
	})
	if err != nil {
		return "", err
	}

	select {
	case resp := <-respCh:
		if resp.Cancelled {
			return "", ErrNoInput
		}
		return resp.Value, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Notify emits a ui_notify event (no response needed).
func (u *RPCUI) Notify(level NotifyLevel, message string) {
	ctx := context.Background()
	_ = u.emit(ctx, map[string]any{
		"type":    "ui_notify",
		"level":   int(level),
		"message": message,
	})
}

// SetStatus emits a ui_set_status event (no response needed).
func (u *RPCUI) SetStatus(key, message string) {
	ctx := context.Background()
	_ = u.emit(ctx, map[string]any{
		"type":    "ui_set_status",
		"key":     key,
		"message": message,
	})
}

// SetWidget emits a ui_set_widget event (no response needed).
func (u *RPCUI) SetWidget(key string, lines []string) {
	ctx := context.Background()
	_ = u.emit(ctx, map[string]any{
		"type":  "ui_set_widget",
		"key":   key,
		"lines": lines,
	})
}

func (u *RPCUI) SetTitle(title string) {
	ctx := context.Background()
	_ = u.emit(ctx, map[string]any{"type": "ui_set_title", "title": title})
}

func (u *RPCUI) SetEditorText(text string) {
	ctx := context.Background()
	_ = u.emit(ctx, map[string]any{"type": "ui_set_editor_text", "text": text})
}

func (u *RPCUI) SetToolsExpanded(expanded bool) {
	ctx := context.Background()
	_ = u.emit(ctx, map[string]any{"type": "ui_set_tools_expanded", "expanded": expanded})
}

// ReadResponse starts reading ui_response lines from the Reader in a background
// goroutine. It returns immediately. The scanning runs until Close is called
// or the context is cancelled.
func (u *RPCUI) ReadResponse(ctx context.Context) {
	u.scanCtx, u.scanCancel = context.WithCancel(ctx)
	go u.runScanner()
}

// runScanner reads lines from the Reader and dispatches them to pending channels.
func (u *RPCUI) runScanner() {
	defer func() {
		if u.scanCancel != nil {
			u.scanCancel()
		}
	}()

	if u.Reader == nil {
		u.Reader = os.Stdin
	}

	scanner := bufio.NewScanner(u.Reader)
	for {
		select {
		case <-u.scanCtx.Done():
			return
		default:
		}

		if !scanner.Scan() {
			return
		}

		line := scanner.Text()
		if line == "" {
			continue
		}

		var msg UIResponse
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg.ID == "" {
			continue
		}
		if msg.Type != "ui_response" {
			continue
		}

		u.mu.Lock()
		if ch, ok := u.pending[msg.ID]; ok {
			select {
			case ch <- msg:
			default:
			}
		}
		u.mu.Unlock()
	}
}

// Close cancels the response reader and abandons outstanding shared requests.
func (u *RPCUI) Close() error {
	if u.scanCancel != nil {
		u.scanCancel()
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.abandonLocked(sharedReply{outcome: OutcomeAborted, err: ErrUIClosed})
	return nil
}

// DispatchResponseTyped delivers a typed UIResponse (with discriminator and
// optional Confirmed/Cancelled fields) to the pending channel. Use this when
// the caller knows the original request method or wants to convey
// `confirmed: true|false` / `cancelled: true`.
func (u *RPCUI) DispatchResponseTyped(resp UIResponse) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if ch, ok := u.pending[resp.ID]; ok {
		select {
		case ch <- resp:
		default:
		}
	}
}

// CancelPending dismisses dialogs that are currently waiting for a response.
// Shared requests resolve as interrupted with an error wrapping
// context.Canceled.
func (u *RPCUI) CancelPending() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.abandonLocked(sharedReply{outcome: OutcomeInterrupted, err: errInterrupted})
	for id, ch := range u.pending {
		select {
		case ch <- UIResponse{ID: id, Cancelled: true}:
		default:
		}
	}
}

// WaitForResponseTyped waits for a typed UIResponse for the given ID.
// The response is delivered via DispatchResponseTyped.
func (u *RPCUI) WaitForResponseTyped(id string, timeout time.Duration) (UIResponse, error) {
	respCh := make(chan UIResponse, 1)

	u.mu.Lock()
	u.pending[id] = respCh
	u.mu.Unlock()

	defer func() {
		u.mu.Lock()
		delete(u.pending, id)
		u.mu.Unlock()
	}()

	select {
	case resp := <-respCh:
		return resp, nil
	case <-time.After(timeout):
		return UIResponse{}, fmt.Errorf("timeout waiting for response: %s", id)
	}
}

// PendingRequests reports how many UI requests are currently waiting for a
// client response. The RPC workspace surfaces this as the session-level
// "needs_input" attention status: a blocked ui_confirm/ui_select/ui_input
// is exactly the state where the agent cannot continue without the user.
func (u *RPCUI) PendingRequests() int {
	if u == nil {
		return 0
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.pending) + len(u.requests)
}
