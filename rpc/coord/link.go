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

	"github.com/gorilla/websocket"
)

const (
	maxQueuedFrames  = 64
	maxQueuedBytes   = 4 << 20
	writeTimeout     = 5 * time.Second
	handshakeTimeout = 10 * time.Second
)

// ErrSlowConsumer reports a peer that stopped draining its outbound queue.
var ErrSlowConsumer = errors.New("slow consumer: outbound queue full")

// Link is a bounded, bidirectional frame connection for JSONL or WebSocket.
//
// Read must be called from a single goroutine. Send is safe for concurrent
// use and never blocks: frames queue for a serialized writer, and exceeding
// the queue bound closes the link rather than dropping frames.
type Link struct {
	conn    net.Conn
	ws      *websocket.Conn
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
	l := newLink(conn)
	l.scanner = scanner
	go l.writeLoop()
	return l
}

// NewWebSocketLink takes ownership of a connected WebSocket. Each text
// message is one frame; the authenticated bridge sends welcome first.
func NewWebSocketLink(ws *websocket.Conn) *Link {
	ws.SetReadLimit(MaxFrameBytes - 1)
	l := newLink(ws.NetConn())
	l.ws = ws
	go l.writeLoop()
	return l
}

func newLink(conn net.Conn) *Link {
	return &Link{
		conn: conn,
		out:  make(chan []byte, maxQueuedFrames),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
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
	if l.ws != nil {
		kind, data, err := l.ws.ReadMessage()
		if err == nil && kind != websocket.TextMessage {
			err = fmt.Errorf("%w: expected a WebSocket text message", ErrInvalidFrame)
		} else if errors.Is(err, websocket.ErrReadLimit) {
			err = ErrFrameTooLarge
		}
		if err != nil {
			l.fail(err)
		}
		return data, err
	}
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
	data, err := encodeFrame(f)
	if err != nil {
		return err
	}
	return l.enqueue(data)
}

func encodeFrame(f Frame) ([]byte, error) {
	data, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("encode frame: %w", err)
	}
	if len(data)+1 > MaxFrameBytes {
		return nil, ErrFrameTooLarge
	}
	return append(data, '\n'), nil
}

// enqueue queues one immutable newline-terminated line. A full queue closes the link.
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
	var err error
	if l.ws == nil {
		if err := l.conn.SetWriteDeadline(deadline); err != nil {
			l.fail(fmt.Errorf("set write deadline: %w", err))
			return false
		}
		_, err = l.conn.Write(data)
	} else if err = l.ws.SetWriteDeadline(deadline); err == nil {
		err = l.ws.WriteMessage(websocket.TextMessage, data[:len(data)-1])
	}
	if err != nil {
		l.fail(fmt.Errorf("write frame: %w", err))
		return false
	}
	l.mu.Lock()
	l.frames--
	l.bytes -= len(data)
	l.mu.Unlock()
	return true
}
