package tui

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/losinggeneration/tau/ai"
	rpcclient "github.com/losinggeneration/tau/rpc/client"
	"github.com/losinggeneration/tau/rpc/coord"
	"github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
)

const coordTestInstance = "inst-a"

// coordFake is a scripted coordinator on a real Unix socket. Each accepted
// connection completes the handshake, answers get_commands and detach
// itself, and hands every other frame to the test.
type coordFake struct {
	path  string
	conns chan *coordFakeConn

	mu         sync.Mutex
	links      []*coord.Link
	generation string
}

type coordFakeConn struct {
	link     *coord.Link
	clientID string
	frames   chan coord.Frame
	detached chan string
}

func newCoordFake(t *testing.T) *coordFake {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fc := &coordFake{path: path, conns: make(chan *coordFakeConn, 4), generation: "gen"}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close listener: %v", err)
		}
		fc.mu.Lock()
		links := slices.Clone(fc.links)
		fc.mu.Unlock()
		for _, link := range links {
			if err := link.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Errorf("close fake link: %v", err)
			}
		}
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			link := coord.NewLink(conn)
			fc.mu.Lock()
			fc.links = append(fc.links, link)
			fc.mu.Unlock()
			go fc.serve(link)
		}
	}()
	return fc
}

func (fc *coordFake) serve(link *coord.Link) {
	hello, err := link.Read()
	if err != nil {
		return
	}
	fc.mu.Lock()
	generation := fc.generation
	fc.mu.Unlock()
	if err := link.Send(coord.Frame{Type: coord.TypeWelcome, Version: coord.Version, Role: coord.RoleClient, Generation: generation, ClientID: hello.ClientID}); err != nil {
		return
	}
	c := &coordFakeConn{link: link, clientID: hello.ClientID, frames: make(chan coord.Frame, 32), detached: make(chan string, 4)}
	fc.conns <- c
	defer close(c.frames)
	for {
		f, err := link.Read()
		if err != nil {
			return
		}
		switch {
		case f.Type == coord.TypeDetach:
			if link.Send(coord.Frame{Type: coord.TypeResponse, ID: f.ID, InstanceID: f.InstanceID}) != nil {
				return
			}
			c.detached <- f.InstanceID
		case commandType(f) == "get_commands":
			result := map[string]any{"commands": []map[string]any{{"name": "compact", "description": "Compact", "source": "builtin"}}}
			if link.Send(coordResult(f, result)) != nil {
				return
			}
		default:
			c.frames <- f
		}
	}
}

func (fc *coordFake) accept(t *testing.T) *coordFakeConn {
	t.Helper()
	select {
	case c := <-fc.conns:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("no coordinator connection")
	}
	return nil
}

func (c *coordFakeConn) next(t *testing.T) coord.Frame {
	t.Helper()
	select {
	case f, ok := <-c.frames:
		if !ok {
			t.Fatal("coordinator connection closed")
		}
		return f
	case <-time.After(2 * time.Second):
		t.Fatal("no frame from client")
	}
	return coord.Frame{}
}

func (c *coordFakeConn) expectCommand(t *testing.T, kind string) (coord.Frame, json.RawMessage) {
	t.Helper()
	f := c.next(t)
	var cmd struct {
		Type string          `json:"type"`
		Args json.RawMessage `json:"args"`
	}
	if f.Type != coord.TypeCommand || json.Unmarshal(f.Command, &cmd) != nil || cmd.Type != kind {
		t.Fatalf("frame = %s %s, want command %s", f.Type, f.Command, kind)
	}
	if f.InstanceID != coordTestInstance {
		t.Fatalf("command target = %q", f.InstanceID)
	}
	return f, cmd.Args
}

func (c *coordFakeConn) send(t *testing.T, f coord.Frame) {
	t.Helper()
	if err := c.link.Send(f); err != nil {
		t.Fatalf("fake send: %v", err)
	}
}

func (c *coordFakeConn) reply(t *testing.T, f coord.Frame, result any) {
	t.Helper()
	c.send(t, coordResult(f, result))
}

func (c *coordFakeConn) serveSnapshot(t *testing.T, snapshot map[string]any) {
	t.Helper()
	f := c.next(t)
	if f.Type != coord.TypeAttach || f.InstanceID != coordTestInstance {
		t.Fatalf("frame = %+v, want attach", f)
	}
	c.send(t, coord.Frame{Type: coord.TypeResponse, ID: f.ID, InstanceID: coordTestInstance})
	f, _ = c.expectCommand(t, "get_snapshot")
	c.reply(t, f, snapshot)
}

func (c *coordFakeConn) event(t *testing.T, offset uint64, raw string) {
	t.Helper()
	c.send(t, coordEventFrame(offset, raw))
}

func coordEventFrame(offset uint64, raw string) coord.Frame {
	return coord.Frame{
		Type:       coord.TypeEvent,
		InstanceID: coordTestInstance,
		SessionID:  "s1",
		Position:   &coord.Position{InstanceID: coordTestInstance, Epoch: "e1", Offset: offset},
		Event:      json.RawMessage(raw),
	}
}

func commandType(f coord.Frame) string {
	var cmd struct {
		Type string `json:"type"`
	}
	if f.Type != coord.TypeCommand || json.Unmarshal(f.Command, &cmd) != nil {
		return ""
	}
	return cmd.Type
}

func coordResult(f coord.Frame, result any) coord.Frame {
	data, err := json.Marshal(map[string]any{"type": "response", "id": "worker", "ok": true, "result": result})
	if err != nil {
		panic(err)
	}
	return coord.Frame{Type: coord.TypeResponse, ID: f.ID, InstanceID: coordTestInstance, Result: data}
}

func testCoordSnapshot(offset uint64, sessionID string) map[string]any {
	return map[string]any{
		"position": coord.Position{InstanceID: coordTestInstance, Epoch: "e1", Offset: offset},
		"instance": coord.Instance{ID: coordTestInstance, CWD: "/work", SessionID: sessionID},
		"history": map[string]any{
			"entries": []map[string]any{
				{"entry_id": "e-1", "message": map[string]any{"Role": "user", "Text": "hi"}},
				{"entry_id": "e-2", "omitted": true},
			},
			"before": "e-0",
		},
		"state": map[string]any{
			"session_id": sessionID, "model": "anthropic/claude", "thinking_level": "high",
			"is_streaming": true, "queue": map[string]int{"steering": 1, "follow_up": 2},
			"stats": sdk.SessionStats{SessionID: sessionID, Model: "anthropic/claude", ThinkingLevel: "high", ContextWindow: 32000, TotalTokens: 100, Usage: ai.Usage{Input: 5}},
		},
		"live":        map[string]any{"messages": []map[string]any{{"id": "m1", "role": "assistant", "text": "partial"}}, "tools": []any{}, "bash": []any{}},
		"ui_requests": []map[string]any{{"id": "ui-1", "method": "confirm", "title": "Proceed?"}},
	}
}

func openCoordTestSession(t *testing.T, fc *coordFake) (*CoordinatedSession, *coordFakeConn) {
	t.Helper()
	type opened struct {
		s   *CoordinatedSession
		err error
	}
	ch := make(chan opened, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s, err := OpenCoordinatedSession(ctx, fc.path, coordTestInstance)
		ch <- opened{s, err}
	}()
	conn := fc.accept(t)
	conn.serveSnapshot(t, testCoordSnapshot(2, "s1"))
	r := <-ch
	if r.err != nil {
		t.Fatalf("open: %v", r.err)
	}
	t.Cleanup(func() {
		if err := r.s.Close(context.Background()); err != nil {
			t.Errorf("close session: %v", err)
		}
	})
	return r.s, conn
}

func nextCoordEvent(t *testing.T, s *CoordinatedSession) sdk.Event {
	t.Helper()
	select {
	case ev, ok := <-s.Events():
		if !ok {
			t.Fatal("events closed")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
	return sdk.Event{}
}

func expectCoordStatus(t *testing.T, s *CoordinatedSession, state string) {
	t.Helper()
	ev := nextCoordEvent(t, s)
	if ev.Type != CoordinatedStatusEvent || payloadValue(ev.Payload, "state") != state {
		t.Fatalf("event = %+v, want status %s", ev, state)
	}
}

func coordAsync[T any](fn func() (T, error)) <-chan struct {
	v   T
	err error
} {
	ch := make(chan struct {
		v   T
		err error
	}, 1)
	go func() {
		v, err := fn()
		ch <- struct {
			v   T
			err error
		}{v, err}
	}()
	return ch
}

func TestCoordinatedSessionOpenEmitsResetThenLiveEvents(t *testing.T) {
	fc := newCoordFake(t)
	s, conn := openCoordTestSession(t, fc)

	ev := nextCoordEvent(t, s)
	reset, ok := ev.Payload.(CoordinatedReset)
	if ev.Type != CoordinatedResetEvent || !ok {
		t.Fatalf("first event = %+v, want reset", ev)
	}
	if reset.Reason != CoordinatedResetAttach || reset.SessionID != "s1" || reset.HistoryBefore != "e-0" || len(reset.History) != 2 {
		t.Fatalf("reset = %+v", reset)
	}
	if reset.History[0].Message.Text != "hi" || reset.History[1].Message.Text != omittedHistoryText {
		t.Fatalf("history = %+v", reset.History)
	}
	if len(reset.Live.Messages) != 1 || reset.Live.Messages[0].Text != "partial" || len(reset.UIRequests) != 1 {
		t.Fatalf("live/ui = %+v %+v", reset.Live, reset.UIRequests)
	}
	state := s.State()
	if state.SessionID() != "s1" || state.Model() != "anthropic/claude" || state.CWD() != "/work" || !s.IsStreaming() || s.ThinkingLevel() != "high" {
		t.Fatalf("state = %+v streaming=%v", state, s.IsStreaming())
	}
	if steer, follow := s.QueueSizes(); steer != 1 || follow != 2 {
		t.Fatalf("queue = %d/%d", steer, follow)
	}
	if reqs := s.PendingUIRequests(); len(reqs) != 1 || reqs[0].Title != "Proceed?" {
		t.Fatalf("ui requests = %+v", reqs)
	}

	if stats := s.Stats(); stats.ContextWindow != 32000 || stats.TotalTokens != 100 || stats.Usage.Input != 5 {
		t.Fatalf("snapshot stats: %+v", stats)
	}
	conn.event(t, 2, `{"type":"text_delta","delta":"dup","seq":1}`)
	conn.event(t, 3, `{"type":"text_delta","delta":"new","seq":2,"session_id":"s1"}`)
	ev = nextCoordEvent(t, s)
	if ev.Type != "text_delta" || payloadValue(ev.Payload, "delta") != "new" || ev.Seq != 2 || ev.SessionID != "s1" {
		t.Fatalf("live event = %+v", ev)
	}
	conn.event(t, 4, `{"type":"ui_resolved","id":"ui-1","outcome":"answered"}`)
	conn.event(t, 5, `{"type":"queue_update","steering":0,"follow_up":0}`)
	conn.event(t, 6, `{"type":"agent_end","stats":{"thinking_level":"low","total_tokens":120,"context_window":32000,"usage":{"input":10,"output":3}}}`)
	for _, want := range []string{"ui_resolved", "queue_update", "agent_end"} {
		if ev := nextCoordEvent(t, s); ev.Type != want {
			t.Fatalf("event = %s, want %s", ev.Type, want)
		}
	}
	if steer, follow := s.QueueSizes(); steer != 0 || follow != 0 || s.IsStreaming() || len(s.PendingUIRequests()) != 0 {
		t.Fatalf("state after events: queue %d/%d streaming %v ui %+v", steer, follow, s.IsStreaming(), s.PendingUIRequests())
	}
	a := newTestApp(t)
	a.sess, a.coordinated = s, s
	a.footer = NewFooter(&footerData{})
	a.refreshCoordinatedModel()
	if d := a.footer.data; d.contextUsed != 120 || d.contextWindow != 32000 || d.inputTokens != 10 || d.outputTokens != 3 || d.thinkingLevel != "low" || a.currentModel != "anthropic/claude" {
		t.Fatalf("live footer: %+v", d)
	}
	deadline := time.Now().Add(time.Second)
	for len(s.SlashCommands()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if cmds := s.SlashCommands(); len(cmds) != 1 || cmds[0].Name != "compact" {
		t.Fatalf("commands = %+v", cmds)
	}
}

func TestCoordinatedSessionSendOnlyIssuesCommand(t *testing.T) {
	fc := newCoordFake(t)
	s, conn := openCoordTestSession(t, fc)
	nextCoordEvent(t, s)

	done := coordAsync(func() (<-chan sdk.Event, error) {
		return s.Send(context.Background(), sdk.Message{
			Text:          "hello",
			AttachmentIDs: []string{"att-1"},
			Images:        []ai.ContentBlock{{Type: ai.ContentBlockImage, Data: "aGk="}},
		})
	})
	f, raw := conn.expectCommand(t, "send_message")
	var args struct {
		ExpectedSessionID string   `json:"expected_session_id"`
		Text              string   `json:"text"`
		Images            []string `json:"images"`
		AttachmentIDs     []string `json:"attachment_ids"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatal(err)
	}
	if args.ExpectedSessionID != "s1" || args.Text != "hello" || !slices.Equal(args.Images, []string{"aGk="}) || !slices.Equal(args.AttachmentIDs, []string{"att-1"}) {
		t.Fatalf("send args = %+v", args)
	}
	if !coord.ValidRequestID(f.RequestID) {
		t.Fatalf("mutation request id = %q", f.RequestID)
	}
	conn.reply(t, f, map[string]any{"ok": true, "disposition": "started"})
	r := <-done
	if r.err != nil {
		t.Fatalf("send: %v", r.err)
	}
	select {
	case _, ok := <-r.v:
		if ok {
			t.Fatal("send channel delivered an event")
		}
	default:
		t.Fatal("send channel is not closed")
	}
}

func TestCoordinatedSessionDoesNotResendUncertainMutation(t *testing.T) {
	fc := newCoordFake(t)
	s, conn := openCoordTestSession(t, fc)
	nextCoordEvent(t, s)

	done := coordAsync(func() (<-chan sdk.Event, error) { return s.SendText(context.Background(), "once") })
	f, _ := conn.expectCommand(t, "send_message")
	conn.send(t, coord.Frame{Type: coord.TypeResponse, ID: f.ID, InstanceID: coordTestInstance, Error: &coord.Error{Code: coord.CodeOutcomeUnknown}})
	if r := <-done; !errors.Is(r.err, rpcclient.ErrOutcomeUnknown) {
		t.Fatalf("send error = %v, want outcome unknown", r.err)
	}

	interrupted := coordAsync(func() (struct{}, error) { return struct{}{}, s.Interrupt() })
	f, _ = conn.expectCommand(t, "interrupt")
	conn.reply(t, f, map[string]any{"ok": true})
	if r := <-interrupted; r.err != nil {
		t.Fatalf("interrupt: %v", r.err)
	}
}

func TestCoordinatedSessionResyncsAfterCursorGap(t *testing.T) {
	fc := newCoordFake(t)
	s, conn := openCoordTestSession(t, fc)
	nextCoordEvent(t, s)

	conn.event(t, 5, `{"type":"text_delta","delta":"skipped"}`)
	ev := nextCoordEvent(t, s)
	if ev.Type != CoordinatedStatusEvent || payloadValue(ev.Payload, "state") != CoordinatedReconnecting || !strings.Contains(payloadValue(ev.Payload, "error").(string), "gap") {
		t.Fatalf("event = %+v, want reconnecting after gap", ev)
	}
	next := fc.accept(t)
	if next.clientID != conn.clientID {
		t.Fatalf("client id changed: %q -> %q", conn.clientID, next.clientID)
	}
	next.serveSnapshot(t, testCoordSnapshot(9, "s2"))
	expectCoordStatus(t, s, CoordinatedConnected)
	ev = nextCoordEvent(t, s)
	reset, ok := ev.Payload.(CoordinatedReset)
	if ev.Type != CoordinatedResetEvent || !ok || reset.Reason != CoordinatedResetResync || reset.SessionID != "s2" {
		t.Fatalf("event = %+v, want resync reset", ev)
	}
	if s.ID() != "s2" {
		t.Fatalf("session id = %q", s.ID())
	}
	next.event(t, 10, `{"type":"text_delta","delta":"after"}`)
	if ev := nextCoordEvent(t, s); payloadValue(ev.Payload, "delta") != "after" {
		t.Fatalf("event after resync = %+v", ev)
	}
}

func TestCoordinatedSessionReconnectResumesFromCursor(t *testing.T) {
	fc := newCoordFake(t)
	s, conn := openCoordTestSession(t, fc)
	nextCoordEvent(t, s)
	conn.event(t, 3, `{"type":"text_delta","delta":"a"}`)
	nextCoordEvent(t, s)

	fc.mu.Lock()
	fc.generation = "replacement"
	fc.mu.Unlock()
	if err := conn.link.Close(); err != nil {
		t.Fatalf("drop coordinator link: %v", err)
	}
	expectCoordStatus(t, s, CoordinatedReconnecting)
	if _, err := s.SendText(context.Background(), "while down"); !errors.Is(err, ErrCoordinatorUnavailable) {
		t.Fatalf("send while reconnecting = %v", err)
	}

	// The replacement accepts clients before the worker registers again.
	empty := fc.accept(t)
	f := empty.next(t)
	if f.Type != coord.TypeAttach {
		t.Fatalf("frame = %+v, want attach", f)
	}
	empty.send(t, coord.Frame{
		Type: coord.TypeResponse, ID: f.ID, InstanceID: f.InstanceID,
		Error: &coord.Error{Code: coord.CodeNotFound, Message: "instance not found"},
	})
	f = empty.next(t)
	if f.Type != coord.TypeListInstances {
		t.Fatalf("frame = %+v, want worker registry", f)
	}
	empty.send(t, coord.Frame{Type: coord.TypeResponse, ID: f.ID})
	expectCoordStatus(t, s, CoordinatedReconnecting)

	next := fc.accept(t)
	f = next.next(t)
	if f.Type != coord.TypeAttach {
		t.Fatalf("frame = %+v, want attach", f)
	}
	next.send(t, coord.Frame{Type: coord.TypeResponse, ID: f.ID, InstanceID: coordTestInstance})
	f, raw := next.expectCommand(t, "replay_events")
	var args struct {
		After coord.Position `json:"after"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || args.After.Offset != 3 || args.After.Epoch != "e1" {
		t.Fatalf("replay args = %s (%v)", raw, err)
	}
	next.reply(t, f, map[string]any{"events": []coord.Frame{coordEventFrame(4, `{"type":"text_delta","delta":"b"}`)}, "more": false})

	expectCoordStatus(t, s, CoordinatedConnected)
	if ev := nextCoordEvent(t, s); ev.Type != "text_delta" || payloadValue(ev.Payload, "delta") != "b" {
		t.Fatalf("replayed event = %+v, want continuation without reset", ev)
	}
}

func TestCoordinatedSessionCloseDetachesWithoutStopping(t *testing.T) {
	fc := newCoordFake(t)
	s, conn := openCoordTestSession(t, fc)
	nextCoordEvent(t, s)

	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case id := <-conn.detached:
		if id != coordTestInstance {
			t.Fatalf("detached %q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not detach")
	}
	for f := range conn.frames {
		if f.Type == coord.TypeStopInstance || commandType(f) == "shutdown" {
			t.Fatalf("close stopped the instance: %+v", f)
		}
	}
	for range s.Events() {
	}
	if _, err := s.SendText(context.Background(), "late"); !errors.Is(err, ErrCoordinatedSessionClosed) {
		t.Fatalf("send after close = %v", err)
	}
}

func TestCoordinatedSessionHistoryPages(t *testing.T) {
	fc := newCoordFake(t)
	s, conn := openCoordTestSession(t, fc)
	nextCoordEvent(t, s)

	entries, next, err := s.History("", 1)
	if err != nil || len(entries) != 1 || entries[0].EntryID != "e-2" || next != "e-2" {
		t.Fatalf("newest page = %+v %q %v", entries, next, err)
	}

	type page struct {
		entries []sdk.HistoryEntry
		next    string
	}
	done := coordAsync(func() (page, error) {
		entries, next, err := s.History("e-0", 10)
		return page{entries, next}, err
	})
	f, raw := conn.expectCommand(t, "get_history")
	var args struct {
		SessionID string `json:"session_id"`
		Before    string `json:"before"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || args.SessionID != "s1" || args.Before != "e-0" {
		t.Fatalf("history args = %s (%v)", raw, err)
	}
	conn.reply(t, f, map[string]any{"entries": []map[string]any{{"entry_id": "e-a", "message": map[string]any{"Role": "assistant", "Text": "old"}}}, "before": ""})
	r := <-done
	if r.err != nil || len(r.v.entries) != 1 || r.v.entries[0].Message.Text != "old" || r.v.next != "" {
		t.Fatalf("older page = %+v %v", r.v, r.err)
	}
}

func TestCoordinatedSessionReportsWorkerExit(t *testing.T) {
	fc := newCoordFake(t)
	s, conn := openCoordTestSession(t, fc)
	nextCoordEvent(t, s)
	conn.send(t, coord.Frame{
		Type: coord.TypeEvent, InstanceID: coordTestInstance,
		Instance: &coord.Instance{ID: coordTestInstance, LinkState: coord.LinkDisconnected},
		Error:    &coord.Error{Code: coord.CodeWorkerLinkLost, Message: "worker link closed"},
	})
	expectCoordStatus(t, s, CoordinatedReconnecting)

	for _, state := range []string{coord.LinkDisconnected, coord.LinkExited} {
		next := fc.accept(t)
		f := next.next(t)
		if f.Type != coord.TypeAttach || f.InstanceID != coordTestInstance {
			t.Fatalf("frame = %+v, want attach", f)
		}
		next.send(t, coord.Frame{
			Type: coord.TypeResponse, ID: f.ID, InstanceID: f.InstanceID,
			Error: &coord.Error{Code: coord.CodeUnavailable, Message: "worker is not connected"},
		})
		f = next.next(t)
		if f.Type != coord.TypeListInstances {
			t.Fatalf("frame = %+v, want worker registry", f)
		}
		next.send(t, coord.Frame{
			Type: coord.TypeResponse, ID: f.ID,
			Instances: []coord.Instance{{ID: coordTestInstance, LinkState: state}},
		})
		if state == coord.LinkDisconnected {
			expectCoordStatus(t, s, CoordinatedReconnecting)
		}
	}

	expectCoordStatus(t, s, CoordinatedExited)
	if s.IsStreaming() || s.Connected() || len(s.PendingUIRequests()) != 0 {
		t.Fatal("exited instance still has active state")
	}
}

func TestCoordinatedSessionForgets(t *testing.T) {
	fc := newCoordFake(t)
	s, conn := openCoordTestSession(t, fc)
	nextCoordEvent(t, s)
	var forgetter sdk.Forgetter = s

	listed := coordAsync(forgetter.GetForgetEntries)
	f, raw := conn.expectCommand(t, "get_forget_entries")
	if !strings.Contains(string(raw), `"expected_session_id":"s1"`) {
		t.Fatalf("list args = %s", raw)
	}
	conn.reply(t, f, map[string]any{"entries": []map[string]string{{"entry_id": "e1", "role": "user", "text": "first"}}})
	if r := <-listed; r.err != nil || !slices.Equal(r.v, []sdk.ForkEntry{{EntryID: "e1", Role: "user", Text: "first"}}) {
		t.Fatalf("entries = %+v, %v", r.v, r.err)
	}

	forgot := coordAsync(func() (struct{}, error) { return struct{}{}, forgetter.Forget("e1") })
	f, raw = conn.expectCommand(t, "forget")
	var args struct {
		ExpectedSessionID string `json:"expected_session_id"`
		EntryID           string `json:"entry_id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatal(err)
	}
	if args.ExpectedSessionID != "s1" || args.EntryID != "e1" || !coord.ValidRequestID(f.RequestID) {
		t.Fatalf("forget args = %+v request=%q", args, f.RequestID)
	}
	conn.reply(t, f, map[string]any{"ok": true})
	if r := <-forgot; r.err != nil {
		t.Fatalf("forget: %v", r.err)
	}
}

func TestCoordinatedContextEditRefreshesWithoutLosingReply(t *testing.T) {
	for _, tc := range []struct {
		command string
		run     func(context.Context, *CoordinatedSession) error
	}{
		{"forget", func(_ context.Context, s *CoordinatedSession) error { return s.Forget("e-1") }},
		{"edit_context", func(ctx context.Context, s *CoordinatedSession) error {
			args := map[string]any{"entry_id": "e-1", "replacement": "edited"}
			return s.call(ctx, coordinatedMutationTimeout, "edit_context", args, nil)
		}},
	} {
		t.Run(tc.command, func(t *testing.T) {
			s, conn := openCoordTestSession(t, newCoordFake(t))
			nextCoordEvent(t, s)

			done := coordAsync(func() (struct{}, error) { return struct{}{}, tc.run(t.Context(), s) })
			f, _ := conn.expectCommand(t, tc.command)
			// The worker publishes the edit before replying to the command.
			conn.event(t, 3, `{"type":"context_edited","session_id":"s1"}`)
			snapshotRequest, _ := conn.expectCommand(t, "get_snapshot")
			conn.reply(t, f, map[string]bool{"ok": true})
			select {
			case r := <-done:
				if r.err != nil {
					t.Fatalf("worker applied %s but client returned %v", tc.command, r.err)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("%s reply was not delivered", tc.command)
			}

			snapshot := testCoordSnapshot(4, "s1")
			snapshot["live"] = CoordinatedLive{}
			snapshot["ui_requests"] = []any{}
			snapshot["history"] = map[string]any{"entries": []map[string]any{
				{"entry_id": "edited", "message": map[string]any{"Role": "user", "Text": "edited"}},
			}}
			conn.event(t, 4, `{"type":"text_delta","delta":"already in snapshot"}`)
			conn.event(t, 5, `{"type":"text_delta","delta":"after snapshot"}`)
			conn.reply(t, snapshotRequest, snapshot)

			reset := nextCoordEvent(t, s)
			if r, ok := reset.Payload.(CoordinatedReset); !ok || r.Reason != CoordinatedResetResync || len(r.History) != 1 || r.History[0].EntryID != "edited" {
				t.Fatalf("event = %+v, want refreshed history", reset)
			}
			if ev := nextCoordEvent(t, s); payloadValue(ev.Payload, "delta") != "after snapshot" {
				t.Fatalf("event = %+v, want first event after refreshed snapshot", ev)
			}
			if !s.Connected() {
				t.Fatal("context edit dropped the connection")
			}
		})
	}
}

func TestCoordinatedRemoteCompactionShowsStatus(t *testing.T) {
	s, conn := openCoordTestSession(t, newCoordFake(t))
	a := newTestApp(t)
	a.sess, a.coordinated = s, s
	a.status = tauui.NewStatusEntriesView(5)
	a.dialogPicker = tauui.NewDialogPicker()
	a.applyCoordinatedEvent(nextCoordEvent(t, s))
	// The run already has a loader row. A remote compaction must make
	// room for a second row without any editor input from this client.
	var laidOutHeight int
	a.loopCtx = testCtx()
	a.loopCtx.InvalidateLayout = func() { laidOutHeight = a.status.MinSize().H }
	conn.event(t, 3, `{"type":"compaction_start","session_id":"s1","reason":"threshold"}`)
	a.applyCoordinatedEvent(nextCoordEvent(t, s))
	if text := a.statusEntries[compactionStatusKey]; !strings.Contains(text, "Compacting...") || laidOutHeight != 2 {
		t.Fatalf("compaction status = %q, laid-out status rows = %d, want a visible compaction row alongside the run loader", text, laidOutHeight)
	}
	conn.event(t, 4, `{"type":"compaction_end","session_id":"s1","reason":"threshold","aborted":true}`)
	a.applyCoordinatedEvent(nextCoordEvent(t, s))
	if _, shown := a.statusEntries[compactionStatusKey]; shown || laidOutHeight != 1 {
		t.Fatalf("compaction status still present: %v, laid-out status rows = %d, want only the run loader", shown, laidOutHeight)
	}
}

func TestCoordinatedResyncRestoresCompactionStatus(t *testing.T) {
	fc := newCoordFake(t)
	s, conn := openCoordTestSession(t, fc)
	a := newTestApp(t)
	a.sess, a.coordinated = s, s
	a.status = tauui.NewStatusEntriesView(5)
	a.dialogPicker = tauui.NewDialogPicker()
	a.applyCoordinatedEvent(nextCoordEvent(t, s))

	// Miss compaction_start at offset 3. The replacement snapshot must
	// restore the compaction even though no agent turn is streaming.
	conn.event(t, 4, `{"type":"text_delta","delta":"skipped"}`)
	expectCoordStatus(t, s, CoordinatedReconnecting)
	next := fc.accept(t)
	snapshot := testCoordSnapshot(4, "s1")
	snapshot["state"] = map[string]any{"session_id": "s1", "is_compacting": true}
	snapshot["live"] = CoordinatedLive{}
	snapshot["ui_requests"] = []any{}
	next.serveSnapshot(t, snapshot)
	expectCoordStatus(t, s, CoordinatedConnected)
	a.applyCoordinatedEvent(nextCoordEvent(t, s))
	if text := a.status.Entries()[compactionStatusKey]; !strings.Contains(text, "Compacting...") || a.isStreaming() {
		t.Fatalf("resynced compaction status = %q, streaming = %v, want only the compaction loader", text, a.isStreaming())
	}

	next.event(t, 5, `{"type":"compaction_end","reason":"manual","summary":"Kept context"}`)
	request, _ := next.expectCommand(t, "get_snapshot")
	snapshot["position"] = coord.Position{InstanceID: coordTestInstance, Epoch: "e1", Offset: 5}
	snapshot["state"] = map[string]any{"session_id": "s1"}
	next.reply(t, request, snapshot)
	a.applyCoordinatedEvent(nextCoordEvent(t, s))
	if _, shown := a.status.Entries()[compactionStatusKey]; shown || a.programStatus.compacting {
		t.Fatal("compaction status outlived the completion snapshot")
	}
}

func TestCoordinatedCompactRefreshesWithoutLosingReply(t *testing.T) {
	s, conn := openCoordTestSession(t, newCoordFake(t))
	a := newTestApp(t)
	a.sess, a.coordinated = s, s
	a.dialogPicker = tauui.NewDialogPicker()
	a.footer = NewFooter(&footerData{cwd: s.State().CWD()})
	a.applyCoordinatedEvent(nextCoordEvent(t, s))
	if got := a.footer.data.contextUsed; got != 100 {
		t.Fatalf("initial context tokens = %d, want 100", got)
	}
	conn.event(t, 3, `{"type":"usage","stats":{"total_tokens":16000,"context_window":32000,"context_used_pct":50,"usage":{"input":5}}}`)
	a.applyCoordinatedEvent(nextCoordEvent(t, s))
	if got := a.footer.contextUsageText(); got != "50%/32k" {
		t.Fatalf("initial footer = %q, want 50%%/32k", got)
	}

	done := coordAsync(func() (struct{}, error) { return struct{}{}, s.Compact(t.Context(), "keep blockers") })
	f, raw := conn.expectCommand(t, "compact")
	if !strings.Contains(string(raw), `"custom_instructions":"keep blockers"`) {
		t.Fatalf("compact args = %s", raw)
	}
	conn.event(t, 4, `{"type":"compaction_start","reason":"manual"}`)
	if ev := nextCoordEvent(t, s); ev.Type != "compaction_start" {
		t.Fatalf("event = %+v, want compaction_start", ev)
	}
	// The worker publishes completion before replying to the command.
	conn.event(t, 5, `{"type":"compaction_end","reason":"manual","summary":"Kept blockers"}`)
	snapshotRequest, _ := conn.expectCommand(t, "get_snapshot")
	conn.reply(t, f, map[string]bool{"ok": true})
	if r := <-done; r.err != nil {
		t.Fatalf("worker completed compaction but client returned %v", r.err)
	}
	snapshot := testCoordSnapshot(6, "s1")
	snapshot["state"] = map[string]any{
		"session_id": "s1", "model": "anthropic/claude",
		"stats": sdk.SessionStats{ContextWindow: 32_000, ContextUsedPct: 2, TotalTokens: 640, Usage: ai.Usage{Input: 5}},
	}
	snapshot["live"] = CoordinatedLive{}
	snapshot["ui_requests"] = []any{}
	snapshot["history"] = map[string]any{"entries": []map[string]any{
		{"entry_id": "compacted", "message": map[string]any{"Role": "system", "Text": "Kept blockers"}},
	}}
	conn.event(t, 6, `{"type":"text_delta","delta":"already in snapshot"}`)
	conn.event(t, 7, `{"type":"text_delta","delta":"after snapshot"}`)
	conn.reply(t, snapshotRequest, snapshot)
	reset := nextCoordEvent(t, s)
	if r, ok := reset.Payload.(CoordinatedReset); !ok || len(r.History) != 1 || r.History[0].EntryID != "compacted" {
		t.Fatalf("event = %+v, want refreshed history", reset)
	}
	a.applyCoordinatedEvent(reset)
	if got := a.footer.contextUsageText(); got != "2%/32k" || a.footer.data.contextUsed != 640 || a.footer.data.inputTokens != 5 {
		t.Fatalf("refreshed footer = %q, context tokens = %d, cumulative input = %d", got, a.footer.data.contextUsed, a.footer.data.inputTokens)
	}
	if ev := nextCoordEvent(t, s); payloadValue(ev.Payload, "delta") != "after snapshot" {
		t.Fatalf("event = %+v, want first event after refreshed snapshot", ev)
	}
}

func TestCoordinatedCompactFailureKeepsEventAndReply(t *testing.T) {
	s, conn := openCoordTestSession(t, newCoordFake(t))
	nextCoordEvent(t, s)

	done := coordAsync(func() (struct{}, error) { return struct{}{}, s.Compact(t.Context()) })
	f, _ := conn.expectCommand(t, "compact")
	conn.event(t, 3, `{"type":"compaction_end","reason":"manual","error_message":"nothing to compact"}`)
	if ev := nextCoordEvent(t, s); ev.Type != "compaction_end" || payloadValue(ev.Payload, "error_message") != "nothing to compact" {
		t.Fatalf("event = %+v, want compaction failure without a reset", ev)
	}
	conn.send(t, coord.Frame{
		Type: coord.TypeResponse, ID: f.ID, InstanceID: coordTestInstance,
		Result: json.RawMessage(`{"type":"response","id":"worker","ok":false,"error":{"code":"error","message":"nothing to compact"}}`),
	})
	if r := <-done; r.err == nil || !strings.Contains(r.err.Error(), "nothing to compact") || errors.Is(r.err, rpcclient.ErrOutcomeUnknown) {
		t.Fatalf("compact error = %v, want the worker's error", r.err)
	}
}

func TestCoordinatedSessionQueueControls(t *testing.T) {
	fc := newCoordFake(t)
	s, conn := openCoordTestSession(t, fc)
	nextCoordEvent(t, s)
	var _ sdk.QueueController = s

	type result struct {
		messages []sdk.Message
		err      error
	}
	done := make(chan result, 1)
	go func() {
		messages, err := s.ClearQueuedMessages(sdk.QueueFollowUp)
		done <- result{messages, err}
	}()
	f, raw := conn.expectCommand(t, "clear_queue")
	var args struct {
		Session string `json:"expected_session_id"`
		Queue   string `json:"queue"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || args.Session != "s1" || args.Queue != "follow_up" {
		t.Fatalf("clear args = %s (%v)", raw, err)
	}
	conn.reply(t, f, map[string]any{"ok": true, "steering": []string{}, "follow_up": []string{"later"}})
	if got := <-done; got.err != nil || len(got.messages) != 1 || got.messages[0].Text != "later" {
		t.Fatalf("clear = %+v", got)
	}

	errs := make(chan error, 1)
	go func() { errs <- s.PauseQueue(sdk.QueueSteering) }()
	f, _ = conn.expectCommand(t, "pause_queue")
	conn.reply(t, f, map[string]any{"ok": true})
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	go func() {
		events, err := s.ResumeQueue(context.Background(), sdk.QueueSteering)
		if events != nil {
			err = errors.New("coordinated resume returned a local event channel")
		}
		errs <- err
	}()
	f, _ = conn.expectCommand(t, "resume_queue")
	conn.reply(t, f, map[string]any{"ok": true, "disposition": "started"})
	if err := <-errs; err != nil {
		t.Fatal(err)
	}

	conn.event(t, 3, `{"type":"queue_update","steering":1,"follow_up":0,"steering_paused":true,"follow_up_paused":false}`)
	if ev := nextCoordEvent(t, s); ev.Type != "queue_update" {
		t.Fatalf("event = %s", ev.Type)
	}
	if steering, followUp := s.QueuesPaused(); !steering || followUp {
		t.Fatalf("paused = %v/%v", steering, followUp)
	}
}

func TestCoordinatedSessionEditsQueuedPrompts(t *testing.T) {
	s, conn := openCoordTestSession(t, newCoordFake(t))
	nextCoordEvent(t, s)
	editor, ok := any(s).(sdk.QueueEditor)
	if !ok {
		t.Fatal("coordinated session cannot edit queued messages")
	}
	conn.event(t, 3, `{"type":"queue_update","steering":0,"follow_up":1,"items":[{"id":"q1","text":"before","kind":"follow_up","editable":true}]}`)
	nextCoordEvent(t, s)
	if items := editor.QueuedPrompts(); len(items) != 1 || items[0].ID != "q1" || items[0].Text != "before" || !items[0].Editable {
		t.Fatalf("queued prompts = %+v", items)
	}

	done := make(chan bool, 1)
	go func() { done <- editor.EditQueuedText("q1", "after") }()
	f, raw := conn.expectCommand(t, "edit_queued_text")
	var args struct {
		Session string `json:"expected_session_id"`
		ID      string `json:"id"`
		Text    string `json:"text"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || args.Session != "s1" || args.ID != "q1" || args.Text != "after" {
		t.Fatalf("edit args = %s (%v)", raw, err)
	}
	conn.reply(t, f, map[string]any{"ok": true})
	if !<-done {
		t.Fatal("edit reported failure")
	}

	go func() { done <- editor.EditQueuedText("gone", "late") }()
	f, _ = conn.expectCommand(t, "edit_queued_text")
	conn.send(t, coordError(f, "queue_changed", "That message has already left the queue."))
	if <-done {
		t.Fatal("rejected edit reported success")
	}
}

func TestCoordinatedRestoreClearsWorkerQueue(t *testing.T) {
	s, conn := openCoordTestSession(t, newCoordFake(t))
	a := newTestApp(t)
	a.sess, a.coordinated = s, s
	a.ctx = context.Background()
	a.status = tauui.NewStatusEntriesView(5)
	a.dialogPicker = tauui.NewDialogPicker()
	a.pending = NewPendingView()
	a.editor = newPromptEditorForTest()
	a.loopCtx = testCtx()
	a.editor.SetText(a.loopCtx, "draft")
	a.applyCoordinatedEvent(nextCoordEvent(t, s))

	a.restoreQueuedMessages(a.loopCtx)
	f, raw := conn.expectCommand(t, "take_queued_text")
	var args struct {
		Session string `json:"expected_session_id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || args.Session != "s1" {
		t.Fatalf("take args = %s (%v)", raw, err)
	}
	conn.reply(t, f, map[string]any{"ok": true, "steering": []string{"steer"}, "follow_up": []string{"later"}, "kept": 1})

	type taken struct {
		queued []string
		kept   int
		err    error
	}
	done := make(chan taken, 1)
	go func() {
		queued, kept, err := s.takeQueuedText(context.Background(), "s1")
		done <- taken{queued, kept, err}
	}()
	f, _ = conn.expectCommand(t, "take_queued_text")
	conn.reply(t, f, map[string]any{"ok": true, "steering": []string{"steer"}, "follow_up": []string{"later"}, "kept": 1})
	got := <-done
	if got.err != nil || got.kept != 1 || !slices.Equal(got.queued, []string{"steer", "later"}) {
		t.Fatalf("take = %+v", got)
	}

	a.finishCoordinatedRestore(got.queued, got.kept, nil)
	if text, want := a.editor.Text(), "steer\n\nlater\n\ndraft"; text != want {
		t.Fatalf("editor = %q, want %q", text, want)
	}
	msgs := a.conv.Messages()
	if last := msgs[len(msgs)-1].Text; !strings.Contains(last, "Left 1 queued message with attachments") {
		t.Fatalf("kept notice = %q", last)
	}

	a.pendingQueue = []string{"still queued"}
	a.finishCoordinatedRestore(nil, 0, errors.New("worker unavailable"))
	if text, want := a.editor.Text(), "steer\n\nlater\n\ndraft"; text != want {
		t.Fatalf("failed restore changed editor: %q", text)
	}
	if len(a.pendingQueue) != 1 {
		t.Fatalf("failed restore dropped pending view: %v", a.pendingQueue)
	}
}

func coordError(f coord.Frame, code, message string) coord.Frame {
	data, err := json.Marshal(map[string]any{"type": "response", "id": "worker", "ok": false, "error": map[string]string{"code": code, "message": message}})
	if err != nil {
		panic(err)
	}
	return coord.Frame{Type: coord.TypeResponse, ID: f.ID, InstanceID: coordTestInstance, Result: data}
}
