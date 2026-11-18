package coord

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

const testTimeout = 2 * time.Second

func eventFrame(size int) Frame {
	return Frame{Type: TypeEvent, Event: json.RawMessage(`{"d":"` + strings.Repeat("x", size) + `"}`)}
}

func waitDone(t *testing.T, l *Link) {
	t.Helper()
	select {
	case <-l.Done():
	case <-time.After(testTimeout):
		t.Fatal("link did not close")
	}
}

func TestLinkSlowConsumerClosesOnFrameCount(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	l := NewLink(a)

	for i := range maxQueuedFrames {
		if err := l.Send(eventFrame(1)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if err := l.Send(eventFrame(1)); !errors.Is(err, ErrSlowConsumer) {
		t.Fatalf("overflow send error = %v, want ErrSlowConsumer", err)
	}
	waitDone(t, l)
	if !errors.Is(l.Err(), ErrSlowConsumer) {
		t.Fatalf("Err() = %v, want ErrSlowConsumer", l.Err())
	}
	if err := l.Send(eventFrame(1)); err == nil {
		t.Fatal("send after overflow succeeded")
	}
}

func TestLinkSlowConsumerClosesOnBytes(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	l := NewLink(a)

	const size = 1_000_000
	for i := range maxQueuedBytes / (size + 64) {
		if err := l.Send(eventFrame(size)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if err := l.Send(eventFrame(size)); !errors.Is(err, ErrSlowConsumer) {
		t.Fatalf("overflow send error = %v, want ErrSlowConsumer", err)
	}
	waitDone(t, l)
}

func TestLinkSendTooLargeKeepsLinkOpen(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	l := NewLink(a)
	defer l.fail(io.EOF)

	if err := l.Send(eventFrame(MaxFrameBytes)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("send error = %v, want ErrFrameTooLarge", err)
	}
	select {
	case <-l.Done():
		t.Fatal("oversized send closed the link")
	default:
	}
	if l.Err() != nil {
		t.Fatalf("Err() = %v, want nil", l.Err())
	}
}

func TestLinkReadRejectsOversizedFrame(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	l := NewLink(a)
	go func() {
		_, _ = b.Write([]byte(strings.Repeat("a", MaxFrameBytes+1) + "\n"))
	}()

	if _, err := l.Read(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("read error = %v, want ErrFrameTooLarge", err)
	}
	waitDone(t, l)
}

func TestLinkReadRejectsMalformedFrame(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	l := NewLink(a)
	go func() {
		_, _ = b.Write([]byte(`{"type":"hello","extra":1}` + "\n"))
	}()

	if _, err := l.Read(); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("read error = %v, want ErrInvalidFrame", err)
	}
	waitDone(t, l)
}

func TestLinkReadEOF(t *testing.T) {
	a, b := net.Pipe()
	l := NewLink(a)
	if err := b.Close(); err != nil {
		t.Fatalf("close peer: %v", err)
	}
	if _, err := l.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("read error = %v, want EOF", err)
	}
	waitDone(t, l)
}

func TestLinkCloseFlushesQueuedFrames(t *testing.T) {
	a, b := net.Pipe()
	l := NewLink(a)
	lines := make(chan []string, 1)
	go func() {
		var got []string
		scanner := bufio.NewScanner(b)
		for scanner.Scan() {
			got = append(got, scanner.Text())
		}
		lines <- got
	}()

	for _, id := range []string{"1", "2", "3"} {
		if err := l.Send(Frame{Type: TypeResponse, ID: id}); err != nil {
			t.Fatalf("send %s: %v", id, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case got := <-lines:
		if len(got) != 3 {
			t.Fatalf("peer read %d frames, want 3: %q", len(got), got)
		}
		for i, line := range got {
			f, err := decodeFrame([]byte(line))
			if err != nil || f.ID != []string{"1", "2", "3"}[i] {
				t.Fatalf("frame %d = %+v, %v", i, f, err)
			}
		}
	case <-time.After(testTimeout):
		t.Fatal("peer did not observe close")
	}
	if !errors.Is(l.Err(), net.ErrClosed) {
		t.Fatalf("Err() = %v, want net.ErrClosed", l.Err())
	}
}
