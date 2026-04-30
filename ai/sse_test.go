package ai

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// TestSSEReaderStreamsIncrementally is the regression guard for this reader's
// reason to exist. Several providers previously buffered the whole response
// with io.ReadAll before yielding anything, so no event reached the caller
// until the stream had finished. A test that feeds a complete buffer cannot
// tell the difference, so this one keeps the stream open: the first payload
// must arrive while later bytes are still unwritten.
func TestSSEReaderStreamsIncrementally(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pr.Close() })

	go func() {
		_, _ = io.WriteString(pw, "data: {\"n\":1}\n\n")
		// Deliberately leave the stream open. A buffering reader blocks here
		// forever; an incremental one has already delivered the first event.
		time.Sleep(2 * time.Second)
		_, _ = io.WriteString(pw, "data: {\"n\":2}\n\n")
		_ = pw.Close()
	}()

	reader := NewSSEReader(pr)

	first := make(chan string, 1)
	go func() {
		if data, ok := reader.Next(); ok {
			first <- data
		}
	}()

	select {
	case got := <-first:
		if got != `{"n":1}` {
			t.Errorf("first payload = %q, want %q", got, `{"n":1}`)
		}
	case <-time.After(time.Second):
		t.Fatal("first event did not arrive before the stream finished: the reader is buffering, not streaming")
	}
}

// TestSSEReaderSkipsNonData verifies comments, blank lines, other SSE fields
// and the [DONE] sentinel never reach the caller.
func TestSSEReaderSkipsNonData(t *testing.T) {
	sse := strings.Join([]string{
		": this is a comment",
		"event: message",
		"",
		"data: {\"n\":1}",
		"",
		"id: 42",
		"data: [DONE]",
		"data: ",
		"data: {\"n\":2}",
	}, "\n")

	reader := NewSSEReader(strings.NewReader(sse))

	var got []string
	for {
		data, ok := reader.Next()
		if !ok {
			break
		}
		got = append(got, data)
	}

	want := []string{`{"n":1}`, `{"n":2}`}
	if len(got) != len(want) {
		t.Fatalf("payloads = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("payload %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestSSEReaderOptionalSpaceAfterColon verifies both "data:" and "data: " are
// accepted. The space is optional in the SSE grammar and providers differ:
// Gemini CLI omits it, the OpenAI family emits it.
func TestSSEReaderOptionalSpaceAfterColon(t *testing.T) {
	reader := NewSSEReader(strings.NewReader("data:{\"n\":1}\ndata: {\"n\":2}\n"))

	for _, want := range []string{`{"n":1}`, `{"n":2}`} {
		got, ok := reader.Next()
		if !ok {
			t.Fatalf("Next() ended early, wanted %q", want)
		}
		if got != want {
			t.Errorf("Next() = %q, want %q", got, want)
		}
	}
}

// TestSSEReaderReportsReadError verifies a mid-stream failure is surfaced
// rather than looking like a clean end of stream. Two providers previously
// never checked their scanner's error, and one discarded it outright.
func TestSSEReaderReportsReadError(t *testing.T) {
	pr, pw := io.Pipe()
	sentinel := errors.New("connection reset")

	go func() {
		_, _ = io.WriteString(pw, "data: {\"n\":1}\n")
		_ = pw.CloseWithError(sentinel)
	}()

	reader := NewSSEReader(pr)

	if _, ok := reader.Next(); !ok {
		t.Fatal("expected the first payload before the failure")
	}
	if _, ok := reader.Next(); ok {
		t.Fatal("expected the stream to end after the failure")
	}

	err := reader.Error()
	if err == nil {
		t.Fatal("Error() = nil, want the read failure")
	}
	if !strings.Contains(err.Error(), sentinel.Error()) {
		t.Errorf("Error() = %v, want it to wrap %v", err, sentinel)
	}
}

// TestSSEReaderCleanEOFHasNoError verifies a normally-terminated stream does
// not report an error, so callers can use Error to distinguish the two.
func TestSSEReaderCleanEOFHasNoError(t *testing.T) {
	reader := NewSSEReader(strings.NewReader("data: {\"n\":1}\n"))

	for {
		if _, ok := reader.Next(); !ok {
			break
		}
	}

	if err := reader.Error(); err != nil {
		t.Errorf("Error() = %v, want nil after clean EOF", err)
	}
}

// TestSSEReaderHandlesLargePayload verifies a payload beyond bufio.Scanner's
// 64KiB default survives. Mistral used a default-sized scanner, so large
// tool-call arguments were silently truncated.
func TestSSEReaderHandlesLargePayload(t *testing.T) {
	large := strings.Repeat("x", 256*1024)
	reader := NewSSEReader(strings.NewReader("data: " + large + "\n"))

	got, ok := reader.Next()
	if !ok {
		t.Fatal("large payload was dropped")
	}
	if len(got) != len(large) {
		t.Errorf("payload length = %d, want %d", len(got), len(large))
	}
}
