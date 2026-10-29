// Package codemode runs model-written JavaScript in a fresh goja VM that can
// only reach the host through injected tools and globals.
package codemode

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// DefaultTimeout bounds a run, including time spent inside tools.
const DefaultTimeout = 300 * time.Second

const maxCallStackSize = 10000

//go:embed prelude.js
var preludeSource string

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
	reservedGlobals   = map[string]bool{"tools": true, "ALL_TOOLS": true, "console": true, "text": true, "image": true, "exit": true, "globalThis": true, "store": true, "load": true}
	errTimeout        = errors.New("codemode timeout")
	errFinished       = errors.New("codemode finished")
)

// Tool is a host function a script can call. Args is the JSON of the
// script's argument (an array for spread globals), nil for undefined. The
// returned value crosses back as JSON.
type Tool struct {
	Name        string
	Description string
	// Spread passes all arguments as an array. Only meaningful for globals.
	Spread  bool
	Execute func(ctx context.Context, args json.RawMessage) (any, error)
}

// Options configures one run.
type Options struct {
	Tools []Tool
	// Globals follow the call protocol of tools but stay out of the call log.
	// Names are identifiers or "namespace.member".
	Globals []Tool
	// Timeout overrides DefaultTimeout; negative disables the deadline.
	Timeout time.Duration
	Store   map[string]json.RawMessage
	// Now and Rand replace the VM's clock and Math.random, the hook for
	// deterministic replay.
	Now  func() time.Time
	Rand func() float64
}

type OutputItem struct {
	Type     string // "text" or "image"
	Text     string
	Console  bool
	Data     string
	MIMEType string
}

type CallStatus string

const (
	CallOK        CallStatus = "ok"
	CallError     CallStatus = "error"
	CallCancelled CallStatus = "cancelled"
)

type Call struct {
	Name     string
	Status   CallStatus
	Duration time.Duration
}

type ErrorKind string

const (
	ErrorScript  ErrorKind = "script"
	ErrorTimeout ErrorKind = "timeout"
	ErrorAborted ErrorKind = "aborted"
	ErrorSandbox ErrorKind = "sandbox"
)

type Error struct {
	Kind    ErrorKind
	Name    string
	Message string
	Stack   string
}

// StoreWrites are a successful run's store() changes, in the shape of
// upstream's codemode-store entry data.
type StoreWrites struct {
	Set    map[string]json.RawMessage `json:"set"`
	Delete []string                   `json:"delete"`
}

// Result is the outcome of a run. Error is nil on success; Value and
// StoreWrites are only set then.
type Result struct {
	Error       *Error
	Value       json.RawMessage
	Output      []OutputItem
	Calls       []Call
	StoreWrites StoreWrites
	// During is set for Start runs.
	During *During
}

type callDone struct {
	id      int64
	ok      bool
	payload string
	hasData bool
}

type pendingCall struct {
	record  int // index into calls, -1 for globals
	started time.Time
	cancel  context.CancelFunc
}

type execution struct {
	host       *hostControl
	toolList   []byte
	globalList []byte
	storeJSON  string
	timeout    time.Duration
	vm         *goja.Runtime
	tools      map[string]Tool
	globals    map[string]Tool
	ctx        context.Context
	done       chan callDone
	closed     chan struct{}
	pending    map[int64]*pendingCall
	result     Result
	finished   bool
}

// Execute runs code as the body of an async function. Script failures are
// reported in the Result; the error is only for invalid options.
func Execute(ctx context.Context, code string, opts Options) (Result, error) {
	e, cleanup, err := newExecution(ctx, opts, nil)
	if err != nil {
		return Result{}, err
	}
	defer cleanup()
	e.run(code)
	return e.result, nil
}

// newExecution prepares a VM for one run. cleanup cancels calls still pending
// and releases the run context.
func newExecution(ctx context.Context, opts Options, host *hostControl) (e *execution, cleanup func(), err error) {
	tools, toolList, err := declareTools(opts.Tools)
	if err != nil {
		return nil, nil, err
	}
	globals, globalList, err := declareGlobals(opts.Globals)
	if err != nil {
		return nil, nil, err
	}
	store := map[string]string{}
	for key, value := range opts.Store {
		store[key] = string(value)
	}
	storeJSON, err := json.Marshal(store)
	if err != nil {
		return nil, nil, fmt.Errorf("encode store: %w", err)
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	cancelDeadline := func() {}
	if timeout > 0 {
		runCtx, cancelDeadline = context.WithTimeoutCause(runCtx, timeout, errTimeout)
	}

	e = &execution{
		vm: goja.New(), tools: tools, globals: globals, ctx: runCtx, host: host,
		done: make(chan callDone), closed: make(chan struct{}), pending: map[int64]*pendingCall{},
		toolList: toolList, globalList: globalList, storeJSON: string(storeJSON), timeout: timeout,
	}
	e.result.StoreWrites.Set = map[string]json.RawMessage{}
	e.vm.SetMaxCallStackSize(maxCallStackSize)
	if opts.Now != nil {
		e.vm.SetTimeSource(opts.Now)
	}
	if opts.Rand != nil {
		e.vm.SetRandSource(opts.Rand)
	}
	stop := context.AfterFunc(runCtx, func() { e.vm.Interrupt(context.Cause(runCtx)) })
	return e, func() {
		for _, p := range e.pending {
			p.cancel()
		}
		stop()
		close(e.closed)
		cancelDeadline()
		cancel(nil)
	}, nil
}

func (e *execution) run(code string) {
	timeout := e.timeout
	api, err := e.start(e.toolList, e.globalList, e.storeJSON)
	if err != nil {
		e.fail(err, timeout)
		return
	}
	settle, _ := goja.AssertFunction(api.Get("settle"))
	run, _ := goja.AssertFunction(api.Get("run"))
	stalled, _ := goja.AssertFunction(api.Get("stalled"))

	if e.host != nil {
		e.host.started()
	}
	// The prefix shares the first line with the script so line numbers match.
	fn, err := e.vm.RunScript("codemode.js", "(async (tools, console) => {"+code+"\n})")
	if err != nil {
		var exception *goja.Exception
		if errors.As(err, &exception) {
			message := strings.TrimPrefix(exception.Value().String(), "SyntaxError: ")
			e.finish(&Error{Kind: ErrorScript, Name: "SyntaxError", Message: message, Stack: "SyntaxError: " + message})
			return
		}
		e.fail(err, timeout)
		return
	}
	if _, err := run(api, fn); err != nil {
		e.fail(err, timeout)
		return
	}
	if e.host != nil {
		e.runHosted(api, settle, stalled)
		return
	}
	for !e.finished {
		if _, err := stalled(api); err != nil {
			e.fail(err, timeout)
			return
		}
		if e.finished {
			return
		}
		select {
		case <-e.ctx.Done():
			e.fail(context.Cause(e.ctx), timeout)
			return
		case d := <-e.done:
			p, ok := e.pending[d.id]
			if !ok {
				continue
			}
			delete(e.pending, d.id)
			if p.record >= 0 {
				e.result.Calls[p.record].Duration = time.Since(p.started)
				e.result.Calls[p.record].Status = CallOK
				if !d.ok {
					e.result.Calls[p.record].Status = CallError
				}
			}
			payload := goja.Undefined()
			if d.hasData {
				payload = e.vm.ToValue(d.payload)
			}
			if _, err := settle(api, e.vm.ToValue(d.id), e.vm.ToValue(d.ok), payload); err != nil {
				e.fail(err, timeout)
				return
			}
		}
	}
}

func (e *execution) start(toolList, globalList []byte, storeJSON string) (*goja.Object, error) {
	prelude, err := e.vm.RunScript("codemode-prelude.js", preludeSource)
	if err != nil {
		return nil, err
	}
	fn, ok := goja.AssertFunction(prelude)
	if !ok {
		return nil, errors.New("prelude is not a function")
	}
	api, err := fn(goja.Undefined(), e.vm.ToValue(e.bridge), e.vm.ToValue(string(toolList)), e.vm.ToValue(string(globalList)), e.vm.ToValue(storeJSON))
	if err != nil {
		return nil, err
	}
	return api.ToObject(e.vm), nil
}

// fail settles the run for an error that escaped the VM: an interrupt from
// cancellation or the deadline, or a broken sandbox.
func (e *execution) fail(err error, timeout time.Duration) {
	if e.finished {
		return
	}
	if cause := context.Cause(e.ctx); cause != nil {
		if errors.Is(cause, errTimeout) {
			e.finish(&Error{Kind: ErrorTimeout, Message: fmt.Sprintf("Execution timed out after %d ms", timeout.Milliseconds())})
			return
		}
		message := "Execution aborted"
		if !errors.Is(cause, context.Canceled) {
			message = cause.Error()
		}
		e.finish(&Error{Kind: ErrorAborted, Message: message})
		return
	}
	var overflow *goja.StackOverflowError
	if errors.As(err, &overflow) {
		frames := strings.SplitN(overflow.String(), "\n", 11)
		e.finish(&Error{Kind: ErrorScript, Name: "RangeError", Message: "Maximum call stack size exceeded", Stack: "RangeError: Maximum call stack size exceeded\n" + strings.Join(frames[:min(len(frames), 10)], "\n")})
		return
	}
	e.finish(&Error{Kind: ErrorSandbox, Message: err.Error()})
}

func (e *execution) finish(err *Error) {
	if e.finished {
		return
	}
	e.finished = true
	if e.host != nil {
		e.host.finish(e)
	}
	e.result.Error = err
	if err != nil {
		e.result.Value, e.result.StoreWrites = nil, StoreWrites{}
	}
	now := time.Now()
	for _, p := range e.pending {
		if p.record >= 0 {
			e.result.Calls[p.record].Duration = now.Sub(p.started)
		}
		p.cancel()
	}
	e.pending = map[int64]*pendingCall{}
	// Stops script code still running after exit() or a caught limit error.
	e.vm.Interrupt(errFinished)
}

func (e *execution) bridge(call goja.FunctionCall) goja.Value {
	if e.finished {
		return goja.Undefined()
	}
	arg := func(i int) string { return call.Argument(i).String() }
	switch arg(0) {
	case "call", "global":
		e.startCall(arg(0) == "call", call.Argument(1).ToInteger(), arg(2), call.Argument(3))
	case "output":
		switch kind := arg(1); kind {
		case "image":
			e.result.Output = append(e.result.Output, OutputItem{Type: "image", Data: arg(2), MIMEType: arg(3)})
		default:
			e.result.Output = append(e.result.Output, OutputItem{Type: "text", Text: arg(2), Console: kind == "console"})
		}
	case "done":
		if !call.Argument(1).ToBoolean() {
			e.finish(scriptError(arg(2)))
			return goja.Undefined()
		}
		if value := call.Argument(2); !goja.IsUndefined(value) {
			e.result.Value = json.RawMessage(value.String())
		}
		if err := e.parseWrites(arg(3)); err != nil {
			e.finish(&Error{Kind: ErrorSandbox, Message: "Sandbox bridge broken: " + err.Error() + ". The script may have modified built-ins such as a prototype's toJSON."})
			return goja.Undefined()
		}
		e.finish(nil)
	}
	return goja.Undefined()
}

func (e *execution) startCall(isTool bool, id int64, name string, args goja.Value) {
	if e.host != nil {
		e.host.call(e, isTool, id, name, args)
		return
	}
	tool, ok := e.globals[name]
	if isTool {
		tool, ok = e.tools[name]
	}
	record := -1
	if isTool {
		record = len(e.result.Calls)
		e.result.Calls = append(e.result.Calls, Call{Name: name, Status: CallCancelled})
	}
	ctx, cancel := context.WithCancel(e.ctx)
	e.pending[id] = &pendingCall{record: record, started: time.Now(), cancel: cancel}
	var raw json.RawMessage
	if !goja.IsUndefined(args) {
		raw = json.RawMessage(args.String())
	}
	go func() {
		d := callDone{id: id}
		switch {
		case !ok:
			kind := "global"
			if isTool {
				kind = "tool"
			}
			d.payload, d.hasData = fmt.Sprintf("Unknown %s %q", kind, name), true
		default:
			d.ok, d.payload, d.hasData = runTool(ctx, tool, raw)
		}
		select {
		case e.done <- d:
		case <-e.closed:
		}
	}()
}

func runTool(ctx context.Context, tool Tool, args json.RawMessage) (ok bool, payload string, hasData bool) {
	defer func() {
		if r := recover(); r != nil {
			ok, payload, hasData = false, fmt.Sprint(r), true
		}
	}()
	value, err := tool.Execute(ctx, args)
	if err != nil {
		return false, err.Error(), true
	}
	if value == nil {
		return true, "", false
	}
	data, err := json.Marshal(value)
	if err != nil {
		return false, err.Error(), true
	}
	return true, string(data), true
}

func scriptError(payload string) *Error {
	var parsed struct {
		Name    *string `json:"name"`
		Message *string `json:"message"`
		Stack   *string `json:"stack"`
	}
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil || parsed.Message == nil {
		return &Error{Kind: ErrorSandbox, Message: "Sandbox bridge broken: script error is malformed"}
	}
	out := &Error{Kind: ErrorScript, Message: *parsed.Message}
	if parsed.Name != nil {
		out.Name = *parsed.Name
	}
	if parsed.Stack != nil {
		out.Stack = *parsed.Stack
	}
	return out
}

func (e *execution) parseWrites(payload string) error {
	var entries [][]string
	if err := json.Unmarshal([]byte(payload), &entries); err != nil {
		return errors.New("store writes are not an array")
	}
	for _, entry := range entries {
		switch len(entry) {
		case 1:
			e.result.StoreWrites.Delete = append(e.result.StoreWrites.Delete, entry[0])
		case 2:
			e.result.StoreWrites.Set[entry[0]] = json.RawMessage(entry[1])
		default:
			return errors.New("store writes contain a malformed entry")
		}
	}
	return nil
}

// Identifier is the name a script uses for a tool: characters that can't
// appear in a JavaScript identifier become "_".
func Identifier(name string) string {
	var b strings.Builder
	for _, r := range name {
		valid := r == '_' || r == '$' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (b.Len() > 0 && r >= '0' && r <= '9')
		if valid {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

func declareTools(list []Tool) (map[string]Tool, []byte, error) {
	type declared struct {
		Name        string `json:"name"`
		JSName      string `json:"jsName"`
		Description string `json:"description,omitempty"`
	}
	tools := map[string]Tool{}
	var decls []declared
	for _, tool := range list {
		if _, dup := tools[tool.Name]; dup {
			return nil, nil, fmt.Errorf("tool %q is already registered", tool.Name)
		}
		tools[tool.Name] = tool
		decls = append(decls, declared{Name: tool.Name, JSName: Identifier(tool.Name), Description: tool.Description})
	}
	data, err := json.Marshal(append([]declared{}, decls...))
	return tools, data, err
}

func declareGlobals(list []Tool) (map[string]Tool, []byte, error) {
	type declared struct {
		Name   string `json:"name"`
		Spread bool   `json:"spread"`
	}
	globals := map[string]Tool{}
	namespaces := map[string]bool{}
	var decls []declared
	for _, global := range list {
		parts := strings.Split(global.Name, ".")
		valid := len(parts) <= 2 && !reservedGlobals[parts[0]]
		for _, part := range parts {
			valid = valid && identifierPattern.MatchString(part)
		}
		if !valid {
			return nil, nil, fmt.Errorf("invalid global name %q", global.Name)
		}
		if _, dup := globals[global.Name]; dup {
			return nil, nil, fmt.Errorf("global %q is already registered", global.Name)
		}
		if len(parts) == 2 {
			namespaces[parts[0]] = true
		}
		globals[global.Name] = global
		decls = append(decls, declared{Name: global.Name, Spread: global.Spread})
	}
	for name := range namespaces {
		if _, ok := globals[name]; ok {
			return nil, nil, fmt.Errorf("global %q conflicts with the namespace %q", name, name)
		}
	}
	data, err := json.Marshal(append([]declared{}, decls...))
	return globals, data, err
}
