package coord

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	maxQueuedFrames  = 64
	maxQueuedBytes   = 4 << 20
	writeTimeout     = 5 * time.Second
	handshakeTimeout = 10 * time.Second
)

// ErrSlowConsumer reports a peer that stopped draining its outbound queue.
var ErrSlowConsumer = errors.New("slow consumer: outbound queue full")

// Link is a bounded, bidirectional JSONL frame connection.
//
// Read must be called from a single goroutine. Send is safe for concurrent
// use and never blocks: frames queue for a serialized writer, and exceeding
// the queue bound closes the link rather than dropping frames.
type Link struct {
	conn    net.Conn
	scanner *bufio.Scanner
	out     chan []byte
	stop    chan struct{}
	done    chan struct{}

	connOnce sync.Once
	closeErr error

	mu      sync.Mutex
	closing bool
	err     error
	frames  int
	bytes   int
}

// NewLink wraps conn and starts its writer.
func NewLink(conn net.Conn) *Link {
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 4096), MaxFrameBytes)
	l := &Link{
		conn:    conn,
		scanner: scanner,
		out:     make(chan []byte, maxQueuedFrames),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go l.writeLoop()
	return l
}

// Dial connects to the coordinator at path and performs the hello/welcome
// handshake. Type and, when zero, Version are filled in on hello.
func Dial(ctx context.Context, path string, hello Frame) (*Link, Frame, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, Frame{}, fmt.Errorf("dial coordinator: %w", err)
	}
	link := NewLink(conn)
	welcome, err := link.handshake(ctx, hello)
	if err != nil {
		if closeErr := link.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return nil, Frame{}, err
	}
	return link, welcome, nil
}

func (l *Link) handshake(ctx context.Context, hello Frame) (Frame, error) {
	hello.Type = TypeHello
	if hello.Version == 0 {
		hello.Version = Version
	}
	deadline := time.Now().Add(handshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := l.conn.SetReadDeadline(deadline); err != nil {
		return Frame{}, fmt.Errorf("set handshake deadline: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { l.fail(ctx.Err()) })
	defer stop()
	if err := l.Send(hello); err != nil {
		return Frame{}, fmt.Errorf("send hello: %w", err)
	}
	reply, err := l.Read()
	if err != nil {
		return Frame{}, fmt.Errorf("read welcome: %w", err)
	}
	if !stop() {
		return Frame{}, fmt.Errorf("handshake: %w", ctx.Err())
	}
	if reply.Error != nil {
		return Frame{}, fmt.Errorf("coordinator rejected hello: %w", reply.Error)
	}
	if reply.Type != TypeWelcome || reply.Version != Version {
		return Frame{}, fmt.Errorf("%w: unexpected handshake reply %.32q version %d", ErrInvalidFrame, reply.Type, reply.Version)
	}
	if err := l.conn.SetReadDeadline(time.Time{}); err != nil {
		return Frame{}, fmt.Errorf("clear handshake deadline: %w", err)
	}
	return reply, nil
}

// Read returns the next frame. Any error, including a malformed frame,
// closes the link.
func (l *Link) Read() (Frame, error) {
	line, err := l.readLine()
	if err != nil {
		return Frame{}, err
	}
	f, err := decodeFrame(line)
	if err != nil {
		l.fail(err)
		return Frame{}, err
	}
	return f, nil
}

// readLine returns the next undecoded line, valid until the next read. A
// transport error closes the link.
func (l *Link) readLine() ([]byte, error) {
	if !l.scanner.Scan() {
		err := l.scanner.Err()
		switch {
		case err == nil:
			err = io.EOF
		case errors.Is(err, bufio.ErrTooLong):
			err = fmt.Errorf("read frame: %w", ErrFrameTooLarge)
		default:
			err = fmt.Errorf("read frame: %w", err)
		}
		l.fail(err)
		return nil, err
	}
	return l.scanner.Bytes(), nil
}

// Send queues f for writing. It returns ErrFrameTooLarge without closing the
// link when f cannot be encoded within MaxFrameBytes, and ErrSlowConsumer
// after closing the link when the outbound queue is full.
func (l *Link) Send(f Frame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("encode frame: %w", err)
	}
	if len(data)+1 > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	return l.enqueue(append(data, '\n'))
}

// enqueue queues one newline-terminated line, closing the link when the
// outbound queue is full.
func (l *Link) enqueue(data []byte) error {
	l.mu.Lock()
	if l.closing {
		err := l.err
		l.mu.Unlock()
		return fmt.Errorf("send on closed link: %w", err)
	}
	if l.frames >= maxQueuedFrames || l.bytes+len(data) > maxQueuedBytes {
		l.closeLocked(ErrSlowConsumer)
		l.mu.Unlock()
		l.closeConn()
		return ErrSlowConsumer
	}
	l.frames++
	l.bytes += len(data)
	l.out <- data
	l.mu.Unlock()
	return nil
}

// Close stops accepting frames, flushes queued frames within one write
// timeout, and closes the connection.
func (l *Link) Close() error {
	l.mu.Lock()
	l.closeLocked(net.ErrClosed)
	l.mu.Unlock()
	<-l.done
	return l.closeErr
}

// Done is closed once the link's connection is closed.
func (l *Link) Done() <-chan struct{} {
	return l.done
}

// Err reports why the link closed, or nil while it is open.
func (l *Link) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

func (l *Link) fail(err error) {
	l.mu.Lock()
	l.closeLocked(err)
	l.mu.Unlock()
	l.closeConn()
}

func (l *Link) closeLocked(err error) {
	if l.err == nil {
		l.err = err
	}
	if !l.closing {
		l.closing = true
		close(l.stop)
	}
}

func (l *Link) closeConn() {
	l.connOnce.Do(func() {
		if err := l.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			l.closeErr = fmt.Errorf("close link: %w", err)
		}
	})
}

func (l *Link) writeLoop() {
	defer close(l.done)
	defer l.closeConn()
	for {
		select {
		case data := <-l.out:
			if !l.write(data, time.Now().Add(writeTimeout)) {
				return
			}
		case <-l.stop:
			deadline := time.Now().Add(writeTimeout)
			for {
				select {
				case data := <-l.out:
					if !l.write(data, deadline) {
						return
					}
				default:
					return
				}
			}
		}
	}
}

func (l *Link) write(data []byte, deadline time.Time) bool {
	if err := l.conn.SetWriteDeadline(deadline); err != nil {
		l.fail(fmt.Errorf("set write deadline: %w", err))
		return false
	}
	if _, err := l.conn.Write(data); err != nil {
		l.fail(fmt.Errorf("write frame: %w", err))
		return false
	}
	l.mu.Lock()
	l.frames--
	l.bytes -= len(data)
	l.mu.Unlock()
	return true
}
