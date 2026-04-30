package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/losinggeneration/tau/agent/plugins"
	"github.com/losinggeneration/tau/commands"
	"github.com/losinggeneration/tau/sdk"
)

// RunWebSocket exposes the JSONL control API over a WebSocket endpoint.
func RunWebSocket(ctx context.Context, addr, token string, runtime sdk.Runtime, cmdRegistry commands.Registry, pluginManager *plugins.Manager, opts ...BridgeOption) (err error) {
	listener, err := NewWebSocketBridge(addr, token, runtime, cmdRegistry, pluginManager, opts...)
	if err != nil {
		return err
	}
	return listener.Run(ctx)
}

// BridgeOption configures optional WebSocket bridge behavior.
type BridgeOption func(*WebSocketBridge)

// WithTokenStore accepts per-device bearer tokens from the store in addition
// to the static bridge token. The store is consulted per request so revoked
// tokens stop working immediately.
func WithTokenStore(store *TokenStore) BridgeOption {
	return func(b *WebSocketBridge) { b.tokens = store }
}

// WebSocketBridge owns a configured WebSocket bridge listener.
type WebSocketBridge struct {
	addr    string
	token   string
	tokens  *TokenStore
	runtime sdk.Runtime

	// baseCtx is the daemon context captured by Run. Connection goroutines
	// must use it: the per-request context is canceled once the upgrade
	// handler returns, which would kill the connection right after it opens.
	baseCtx  context.Context
	registry commands.Registry
	plugins  *plugins.Manager
	server   *http.Server
	listener net.Listener
}

// NewWebSocketBridge creates a WebSocket bridge listener without serving it.
// It requires either a static token or a token store via WithTokenStore.
func NewWebSocketBridge(addr, token string, runtime sdk.Runtime, cmdRegistry commands.Registry, pluginManager *plugins.Manager, opts ...BridgeOption) (*WebSocketBridge, error) {
	bridge := &WebSocketBridge{
		addr:     addr,
		token:    token,
		runtime:  runtime,
		registry: cmdRegistry,
		plugins:  pluginManager,
	}
	for _, opt := range opts {
		opt(bridge)
	}
	if strings.TrimSpace(token) == "" && bridge.tokens == nil {
		return nil, fmt.Errorf("websocket token is required")
	}
	if addr == "" {
		addr = "127.0.0.1:0"
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen websocket bridge: %w", err)
	}
	bridge.addr = listener.Addr().String()
	bridge.listener = listener
	mux := http.NewServeMux()
	registerWebApp(mux)
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	mux.HandleFunc("/rpc", func(w http.ResponseWriter, r *http.Request) {
		if !bridge.validToken(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connCtx := bridge.baseCtx
		if connCtx == nil {
			connCtx = context.WithoutCancel(r.Context())
		}
		go runWebSocketConn(connCtx, conn, bridge.runtime, bridge.registry, bridge.plugins, bridge.tokenGuard(bearerToken(r)))
	})
	bridge.server = &http.Server{Handler: mux}
	return bridge, nil
}

// Addr returns the bound bridge address.
func (b *WebSocketBridge) Addr() string {
	if b == nil {
		return ""
	}
	return b.addr
}

// Run serves the WebSocket bridge until ctx is canceled.
func (b *WebSocketBridge) Run(ctx context.Context) (err error) {
	b.baseCtx = ctx
	defer func() {
		if closeErr := b.listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			err = errors.Join(err, fmt.Errorf("close websocket listener: %w", closeErr))
		}
	}()

	serveErr := make(chan error, 1)
	go func() {
		err := b.server.Serve(b.listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := b.server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown websocket bridge: %w", err)
		}
		if err := <-serveErr; err != nil {
			return fmt.Errorf("serve websocket bridge: %w", err)
		}
		return nil
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("serve websocket bridge: %w", err)
		}
		return nil
	}
}

func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.URL.Query().Get("token")
}

func validWebSocketToken(r *http.Request, token string) bool {
	if token == "" {
		return false
	}
	return bearerToken(r) == token
}

// validToken accepts the static bridge token or any non-revoked device token.
func (b *WebSocketBridge) validToken(r *http.Request) bool {
	got := bearerToken(r)
	if got == "" {
		return false
	}
	if validWebSocketToken(r, b.token) {
		return true
	}
	return b.tokens != nil && b.tokens.Contains(got)
}

// tokenGuard returns a per-message revocation check for a connection that
// authenticated with a store device token. Static tokens cannot be revoked
// without a restart, so they get no guard.
func (b *WebSocketBridge) tokenGuard(token string) func() bool {
	if b.tokens == nil || token == "" || token == b.token {
		return nil
	}
	return func() bool { return b.tokens.Contains(token) }
}

func runWebSocketConn(ctx context.Context, conn *websocket.Conn, runtime sdk.Runtime, cmdRegistry commands.Registry, pluginManager *plugins.Manager, tokenGuard func() bool) {
	defer func() {
		if err := conn.Close(); err != nil {
			if _, writeErr := fmt.Fprintf(os.Stderr, "tau websocket connection close error: %v\n", err); writeErr != nil {
				return
			}
		}
	}()

	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if err := pw.Close(); err != nil {
				return
			}
		}()
		for {
			messageType, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if tokenGuard != nil && !tokenGuard() {
				// Device token revoked since connect: cut the connection so
				// the client must re-authenticate.
				return
			}
			if messageType != websocket.TextMessage {
				continue
			}
			if _, err := pw.Write(data); err != nil {
				return
			}
			if _, err := pw.Write([]byte("\n")); err != nil {
				return
			}
		}
	}()

	srv := NewServer(runtime, cmdRegistry, pluginManager)
	srv.reader = bufio.NewReader(pr)
	srv.writer = &webSocketWriter{conn: conn}
	if err := srv.Run(ctx); err != nil {
		if _, writeErr := fmt.Fprintf(os.Stderr, "tau websocket connection error: %v\n", err); writeErr != nil {
			return
		}
		return
	}
	<-done
}

type webSocketWriter struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (w *webSocketWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.conn.WriteMessage(websocket.TextMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
