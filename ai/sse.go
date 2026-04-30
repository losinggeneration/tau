package ai

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// The single Server-Sent Events reader for every provider that speaks SSE.
// Each provider previously carried its own: three of them buffered the whole
// response with io.ReadAll before emitting a single event, which meant those
// providers did not actually stream, and two never checked their scanner's
// error, so a mid-stream read failure looked like a clean end of stream.

// sseMaxLineBytes bounds a single SSE line. Tool-call arguments can be large,
// and bufio.Scanner's 64KiB default silently truncated them.
const sseMaxLineBytes = 1024 * 1024

// SSEReader reads Server-Sent Events incrementally from a reader.
type SSEReader struct {
	scanner *bufio.Scanner
	err     error
}

// NewSSEReader creates a new SSE reader from an io.Reader. The reader is
// consumed lazily, so events are delivered as the underlying stream produces
// them.
func NewSSEReader(r io.Reader) *SSEReader {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), sseMaxLineBytes)

	return &SSEReader{scanner: scanner}
}

// Next returns the next SSE data payload, skipping comments, blank lines,
// non-data fields and the [DONE] sentinel. It reports false once the stream is
// exhausted or fails; call Error to distinguish the two.
func (sr *SSEReader) Next() (string, bool) {
	for sr.scanner.Scan() {
		// bufio.ScanLines already strips a trailing \r.
		line := strings.TrimSpace(sr.scanner.Text())

		// The space after "data:" is optional in the SSE grammar, and
		// providers differ on whether they emit it.
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}

		return data, true
	}

	if err := sr.scanner.Err(); err != nil {
		sr.err = fmt.Errorf("read SSE stream: %w", err)
	}

	return "", false
}

// Error reports a read failure that ended the stream early, or nil if the
// stream ended cleanly.
func (sr *SSEReader) Error() error {
	return sr.err
}
