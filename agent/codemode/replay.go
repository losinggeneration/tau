package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
)

// deterministicVersion is bumped whenever this file or the prelude changes
// what a deterministic script observes beyond the fingerprint's other parts.
const deterministicVersion = 1

// HostCall is a call a Start run's script made, answered with Run.Deliver.
type HostCall struct {
	// ID is the VM's number for the call, 1, 2, ... in the order the script
	// made its calls, tools and globals together.
	ID     int64
	Target string // "tool" or "global"
	Name   string
	// Args is the JSON the script passed, an array for spread globals, nil
	// for undefined.
	Args json.RawMessage
}

// Outcome answers a call. A failure becomes an Error with Message in the
// script, carrying the JSON object Properties except name, message and stack.
type Outcome struct {
	OK         bool
	Value      any
	Message    string
	Properties any
}

// During is the result the VM was processing when the script finished (0 for
// its first run) and the calls issued by then.
type During struct {
	Seq    int64
	Issued int64
}

// RunOptions configures a Start run. Tools and Globals only declare names;
// their Execute functions are ignored because the host answers every call.
// The callbacks run on the VM goroutine and can fire before Start returns.
type RunOptions struct {
	// StartedAt is the VM clock until the first result arrives, in ms since
	// the epoch.
	StartedAt float64
	// Seed seeds Math.random. A 32-bit unsigned integer.
	Seed    int64
	Tools   []Tool
	Globals []Tool
	Store   map[string]json.RawMessage
	// Timeout is the run's deadline; zero or negative means none.
	Timeout time.Duration
	// OnStarted gets the VM's fingerprint before the script runs.
	OnStarted func(fingerprint string)
	// OnCall gets each call. It runs on the VM goroutine and may call
	// Deliver itself or hand the call to another goroutine.
	OnCall func(HostCall)
	// OnIdle reports that the VM processed result seq (0: the script's first
	// run) and waits again, having issued calls in total.
	OnIdle func(seq, issued int64)
}

// Run is a running Start execution.
type Run struct {
	host   *hostControl
	done   chan struct{}
	result Result
}

type delivery struct {
	seq, id    int64
	now        float64
	ok         bool
	payload    string
	hasData    bool
	properties string
}

type hostControl struct {
	opts      RunOptions
	mu        sync.Mutex
	finished  bool
	waiting   map[int64]bool
	delivered map[int64]bool
	queue     []delivery
	wake      chan struct{}
	early     map[int64]delivery
	next      int64
	current   int64
	issued    int64
	clock     float64
}

// Start runs code deterministically with the caller answering its calls:
// the same script, start time, seed, declarations and results, delivered in
// any arrival order, make the same calls and output on any VM with the same
// fingerprint, so a host can replay a run after a restart.
func Start(ctx context.Context, code string, opts RunOptions) (*Run, error) {
	if math.IsNaN(opts.StartedAt) || math.IsInf(opts.StartedAt, 0) || opts.StartedAt < 0 {
		return nil, errors.New("startedAt must be a non-negative number of milliseconds since the epoch")
	}
	if opts.Seed < 0 || opts.Seed > math.MaxUint32 {
		return nil, errors.New("seed must be a 32-bit unsigned integer")
	}
	if opts.OnCall == nil {
		return nil, errors.New("OnCall is required")
	}
	h := &hostControl{
		opts: opts, waiting: map[int64]bool{}, delivered: map[int64]bool{}, early: map[int64]delivery{},
		wake: make(chan struct{}, 1), next: 1, clock: opts.StartedAt,
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = -1
	}
	random := mulberry32(uint32(opts.Seed) ^ 0x9e3779b9)
	e, cleanup, err := newExecution(ctx, Options{
		Tools: opts.Tools, Globals: opts.Globals, Store: opts.Store, Timeout: timeout,
		Now:  func() time.Time { return time.UnixMilli(int64(math.Round(h.clock))) },
		Rand: func() float64 { return float64(random()) / (1 << 32) },
	}, h)
	if err != nil {
		return nil, err
	}
	r := &Run{host: h, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		defer cleanup()
		e.run(code)
		r.result = e.result
	}()
	return r, nil
}

// Result waits for the run to stop.
func (r *Run) Result() Result {
	<-r.done
	return r.result
}

// Finished reports whether the run has its outcome; calls can no longer be
// answered once it does.
func (r *Run) Finished() bool {
	r.host.mu.Lock()
	defer r.host.mu.Unlock()
	return r.host.finished
}

// Deliver answers call id as result number seq. Results are numbered 1, 2, ...
// without gaps and apply strictly in that order whatever order they arrive in.
// While the VM processes one, its clock reads now (ms since the epoch), or the
// previous value if now is earlier. Deliveries after the run finished are
// ignored.
func (r *Run) Deliver(seq int64, now float64, id int64, outcome Outcome) error {
	h := r.host
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.finished {
		return nil
	}
	if math.IsNaN(now) || math.IsInf(now, 0) {
		return fmt.Errorf("the clock value of result %d is not a finite number", seq)
	}
	if seq < 1 || h.delivered[seq] {
		return fmt.Errorf("result number %d is not a positive integer used once", seq)
	}
	if !h.waiting[id] {
		return fmt.Errorf("call %d is not waiting for a result", id)
	}
	d := delivery{seq: seq, id: id, now: now, ok: outcome.OK}
	if outcome.OK {
		if outcome.Value != nil {
			data, err := json.Marshal(outcome.Value)
			if err != nil {
				return fmt.Errorf("result %d: %w", seq, err)
			}
			d.payload, d.hasData = string(data), true
		}
	} else {
		d.payload, d.hasData = outcome.Message, true
		if outcome.Properties != nil {
			data, err := json.Marshal(outcome.Properties)
			if err != nil || len(data) == 0 || data[0] != '{' {
				return fmt.Errorf("the properties of result %d are not an object", seq)
			}
			d.properties = string(data)
		}
	}
	h.delivered[seq] = true
	delete(h.waiting, id)
	h.queue = append(h.queue, d)
	select {
	case h.wake <- struct{}{}:
	default:
	}
	return nil
}

func (h *hostControl) started() {
	if h.opts.OnStarted == nil {
		return
	}
	version := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/dop251/goja" {
				version = dep.Version
			}
		}
	}
	prelude := fnv.New32a()
	_, _ = prelude.Write([]byte(preludeSource)) // hash writes never fail
	zone := zoneName(os.LookupEnv, os.Readlink)
	if zone == "" {
		name, offset := time.UnixMilli(int64(h.opts.StartedAt)).Zone()
		zone = fmt.Sprintf("%s%+d", name, offset)
	}
	h.opts.OnStarted(fmt.Sprintf("goja %s, prelude %08x, deterministic %d, stack %d, zone %s", version, prelude.Sum32(), deterministicVersion, maxCallStackSize, zone))
}

// zoneName names the IANA zone behind time.Local, which the VM's Date uses,
// the way Go picks it: TZ, then the /etc/localtime link. An offset alone
// changes with daylight saving time, so a replay in another season would
// mismatch. Empty when the zone has no name to find, as on Windows.
func zoneName(lookupEnv func(string) (string, bool), readlink func(string) (string, error)) string {
	if tz, ok := lookupEnv("TZ"); ok {
		if tz = strings.TrimPrefix(tz, ":"); tz != "" {
			return tz
		}
		return "UTC"
	}
	if target, err := readlink("/etc/localtime"); err == nil {
		if _, name, ok := strings.Cut(target, "zoneinfo/"); ok {
			return name
		}
	}
	return ""
}

func (h *hostControl) call(e *execution, isTool bool, id int64, name string, args goja.Value) {
	target := "global"
	record := -1
	if isTool {
		target = "tool"
		record = len(e.result.Calls)
		e.result.Calls = append(e.result.Calls, Call{Name: name, Status: CallCancelled})
	}
	e.pending[id] = &pendingCall{record: record, started: time.Now(), cancel: func() {}}
	var raw json.RawMessage
	if !goja.IsUndefined(args) {
		raw = json.RawMessage(args.String())
	}
	h.mu.Lock()
	h.waiting[id] = true
	h.issued++
	h.mu.Unlock()
	h.opts.OnCall(HostCall{ID: id, Target: target, Name: name, Args: raw})
}

func (h *hostControl) finish(e *execution) {
	h.mu.Lock()
	h.finished = true
	h.mu.Unlock()
	e.result.During = &During{Seq: h.current, Issued: h.issued}
}

func (h *hostControl) idle() {
	if h.opts.OnIdle != nil {
		h.mu.Lock()
		issued := h.issued
		h.mu.Unlock()
		h.opts.OnIdle(h.current, issued)
	}
}

// runHosted applies delivered results in seq order until the run finishes.
func (e *execution) runHosted(api goja.Value, settle, stalled goja.Callable) {
	h := e.host
	if !e.finished {
		h.idle()
	}
	for !e.finished {
		if d, ok := h.early[h.next]; ok {
			delete(h.early, h.next)
			h.next++
			h.current = d.seq
			h.clock = math.Max(h.clock, d.now)
			if p, ok := e.pending[d.id]; ok {
				delete(e.pending, d.id)
				if p.record >= 0 {
					e.result.Calls[p.record].Duration = time.Since(p.started)
					e.result.Calls[p.record].Status = CallOK
					if !d.ok {
						e.result.Calls[p.record].Status = CallError
					}
				}
			}
			args := []goja.Value{e.vm.ToValue(d.id), e.vm.ToValue(d.ok), goja.Undefined()}
			if d.hasData {
				args[2] = e.vm.ToValue(d.payload)
			}
			if d.properties != "" {
				args = append(args, e.vm.ToValue(d.properties))
			}
			if _, err := settle(api, args...); err != nil {
				e.fail(err, e.timeout)
				return
			}
			if !e.finished {
				h.idle()
			}
			continue
		}
		if _, err := stalled(api); err != nil {
			e.fail(err, e.timeout)
			return
		}
		if e.finished {
			return
		}
		select {
		case <-e.ctx.Done():
			e.fail(context.Cause(e.ctx), e.timeout)
			return
		case <-h.wake:
			h.mu.Lock()
			for _, d := range h.queue {
				h.early[d.seq] = d
			}
			h.queue = nil
			h.mu.Unlock()
		}
	}
}

// mulberry32 is upstream's PRNG, one 32-bit draw per call.
func mulberry32(seed uint32) func() uint32 {
	state := seed
	return func() uint32 {
		state += 0x6d2b79f5
		value := (state ^ (state >> 15)) * (1 | state)
		value = (value + (value^(value>>7))*(61|value)) ^ value
		return value ^ (value >> 14)
	}
}
