package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/losinggeneration/tau/rpc/coord"
)

const (
	// CoordSubprotocol is the WebSocket subprotocol a browser offers for the
	// coordinated bridge. The bridge selects it whenever it is offered.
	CoordSubprotocol = "tau.coord.v1"
	// CoordTokenSubprotocolPrefix carries a bearer token as a second offered
	// subprotocol, so browsers need not put credentials in the URL. The
	// bridge never echoes it back.
	CoordTokenSubprotocolPrefix = "tau.token."

	maxCoordBridgeClients = 16
	coordBridgeIOTimeout  = 10 * time.Second
	maxBrowserIDBytes     = 256
	maxBrowserTextBytes   = 4096
)

// CoordinatedBridge relays authenticated loopback WebSocket clients to the
// local coordinator. Each browser connection becomes one coordinator client
// connection whose hello the bridge writes itself, so the role is always
// client and no browser-supplied identity or credential is forwarded.
type CoordinatedBridge struct {
	socket   string
	token    string
	tokens   *TokenStore
	addr     string
	listener net.Listener
	server   *http.Server
	upgrader websocket.Upgrader
	// slots bounds concurrent /rpc handlers, including live relays.
	slots chan struct{}
	// ctx is set by Run before serving starts and is canceled on shutdown.
	ctx context.Context
}

// NewCoordinatedBridge binds a loopback-only bridge to the coordinator socket
// without serving it. It requires a static token or a token store.
func NewCoordinatedBridge(addr, socket, token string, store *TokenStore) (*CoordinatedBridge, error) {
	if strings.TrimSpace(token) == "" {
		token = ""
	}
	if token == "" && store == nil {
		return nil, errors.New("websocket token is required")
	}
	if !filepath.IsAbs(socket) {
		return nil, errors.New("coordinator socket path must be absolute")
	}
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("websocket address: %w", err)
	}
	if !loopbackHost(host) {
		return nil, errors.New("coordinated WebSocket access is loopback-only; listen on 127.0.0.1, [::1], or localhost")
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen websocket bridge: %w", err)
	}
	if tcp, ok := listener.Addr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		err := fmt.Errorf("websocket bridge bound non-loopback address %s", listener.Addr())
		if closeErr := listener.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return nil, err
	}
	b := &CoordinatedBridge{
		socket:   socket,
		token:    token,
		tokens:   store,
		addr:     listener.Addr().String(),
		listener: listener,
		slots:    make(chan struct{}, maxCoordBridgeClients),
	}
	b.upgrader = websocket.Upgrader{
		HandshakeTimeout: coordBridgeIOTimeout,
		Subprotocols:     []string{CoordSubprotocol},
		CheckOrigin:      sameOrigin,
	}
	mux := http.NewServeMux()
	registerCoordinatedWebApp(mux)
	mux.HandleFunc("/rpc", b.serveRPC)
	b.server = &http.Server{
		Handler:           localOnly(mux),
		ReadHeaderTimeout: coordBridgeIOTimeout,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	return b, nil
}

// Addr returns the bound bridge address.
func (b *CoordinatedBridge) Addr() string {
	if b == nil {
		return ""
	}
	return b.addr
}

// Run serves the bridge until ctx is canceled or serving fails, then closes
// every relay and waits for their handlers to return. It must be called once.
func (b *CoordinatedBridge) Run(ctx context.Context) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	b.ctx = ctx
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
	case err = <-serveErr:
		serveErr = nil
		if err != nil {
			err = fmt.Errorf("serve websocket bridge: %w", err)
		}
	}
	// Cancel first so pending coordinator dials and relays unwind before
	// Shutdown waits on handlers that have not yet hijacked.
	cancel()
	shutdownCtx, stop := context.WithTimeout(context.Background(), coordBridgeIOTimeout)
	defer stop()
	if shutdownErr := b.server.Shutdown(shutdownCtx); shutdownErr != nil {
		err = errors.Join(err, fmt.Errorf("shutdown websocket bridge: %w", shutdownErr))
	}
	if serveErr != nil {
		if e := <-serveErr; e != nil {
			err = errors.Join(err, fmt.Errorf("serve websocket bridge: %w", e))
		}
	}
	// Hijacked relays are invisible to Shutdown; filling every slot waits
	// for them and refuses late handlers.
	for range cap(b.slots) {
		b.slots <- struct{}{}
	}
	return err
}

func (b *CoordinatedBridge) serveRPC(w http.ResponseWriter, r *http.Request) {
	select {
	case b.slots <- struct{}{}:
	default:
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}
	defer func() { <-b.slots }()
	if !sameOrigin(r) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	token := coordBridgeToken(r)
	if !b.authorized(token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !websocket.IsWebSocketUpgrade(r) {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return
	}
	dialCtx, cancel := context.WithTimeout(b.ctx, coordBridgeIOTimeout)
	link, welcome, err := coord.Dial(dialCtx, b.socket, coord.Frame{Role: coord.RoleClient})
	cancel()
	if err != nil {
		http.Error(w, "coordinator unavailable", http.StatusServiceUnavailable)
		return
	}
	conn, err := b.upgrader.Upgrade(w, r, nil)
	if err != nil {
		if closeErr := link.Close(); closeErr != nil {
			coordBridgeLog("close coordinator link: %v", closeErr)
		}
		return
	}
	b.relay(conn, link, welcome, b.guard(token))
}

// relay pumps frames both ways until either side closes, the token is
// revoked, or the bridge stops. It returns after both directions finish.
func (b *CoordinatedBridge) relay(conn *websocket.Conn, link *coord.Link, welcome coord.Frame, guard func() bool) {
	sock := &relaySocket{conn: conn}
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				coordBridgeLog("close websocket: %v", err)
			}
			if err := link.Close(); err != nil {
				coordBridgeLog("close coordinator link: %v", err)
			}
		})
	}
	stop := context.AfterFunc(b.ctx, func() {
		sock.closeWith(websocket.CloseGoingAway, "bridge stopping")
		closeAll()
	})
	defer stop()

	outbound := make(chan struct{})
	go func() {
		defer close(outbound)
		defer closeAll()
		if err := sock.send(welcome); err != nil {
			return
		}
		for {
			f, err := link.Read()
			if err != nil {
				sock.closeWith(websocket.CloseGoingAway, "coordinator link closed")
				return
			}
			if guard != nil && !guard() {
				sock.closeWith(websocket.ClosePolicyViolation, "token revoked")
				return
			}
			if err := sock.send(f); err != nil {
				return
			}
		}
	}()

	conn.SetReadLimit(coord.MaxFrameBytes - 1)
	for {
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if guard != nil && !guard() {
			sock.closeWith(websocket.ClosePolicyViolation, "token revoked")
			break
		}
		if messageType != websocket.TextMessage {
			sock.closeWith(websocket.CloseUnsupportedData, "one JSON text frame per message")
			break
		}
		f, rejection := parseBrowserFrame(data)
		if rejection == nil {
			err = link.Send(f)
			if errors.Is(err, coord.ErrFrameTooLarge) {
				rejection = &coord.Error{Code: coord.CodeLimitExceeded, Message: "frame exceeds limit"}
			} else if err != nil {
				break
			}
		}
		if rejection != nil {
			if err := sock.send(coord.Frame{Type: coord.TypeResponse, ID: f.ID, InstanceID: f.InstanceID, Error: rejection}); err != nil {
				break
			}
		}
	}
	closeAll()
	<-outbound
}

// browserFrame lists the only fields a browser may set. Role, version,
// token, client and worker identities are dropped by decoding. RequestID is
// a correlation identifier the coordinator validates; it grants nothing.
type browserFrame struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`
	RequestID  string          `json:"request_id"`
	InstanceID string          `json:"instance_id"`
	CWD        string          `json:"cwd"`
	Command    json.RawMessage `json:"command"`
}

// parseBrowserFrame turns one browser message into a client operation. On
// rejection, the returned frame carries only identifiers safe to echo.
func parseBrowserFrame(data []byte) (coord.Frame, *coord.Error) {
	var in browserFrame
	if err := json.Unmarshal(data, &in); err != nil {
		return coord.Frame{}, &coord.Error{Code: coord.CodeInvalidFrame, Message: "message must be one JSON object"}
	}
	if len(in.ID) > maxBrowserIDBytes || len(in.InstanceID) > maxBrowserIDBytes {
		return coord.Frame{}, &coord.Error{Code: coord.CodeInvalidFrame, Message: "identifier exceeds limit"}
	}
	if in.RequestID != "" && !coord.ValidRequestID(in.RequestID) {
		return coord.Frame{ID: in.ID, InstanceID: in.InstanceID}, &coord.Error{Code: coord.CodeInvalidFrame, Message: "request_id must be a random UUID"}
	}
	f := coord.Frame{Type: in.Type, ID: in.ID, RequestID: in.RequestID, InstanceID: in.InstanceID}
	switch in.Type {
	case coord.TypeListInstances, coord.TypeWatchInstances, coord.TypeUnwatchInstances, coord.TypeStartInstance, coord.TypeAttach,
		coord.TypeDetach, coord.TypeCommand, coord.TypeStopInstance:
	default:
		return coord.Frame{ID: in.ID, InstanceID: in.InstanceID}, &coord.Error{Code: coord.CodeInvalidFrame, Message: "unsupported client operation"}
	}
	if len(in.CWD) > maxBrowserTextBytes {
		return coord.Frame{ID: in.ID, InstanceID: in.InstanceID}, &coord.Error{Code: coord.CodeInvalidFrame, Message: "cwd exceeds limit"}
	}
	f.CWD, f.Command = in.CWD, in.Command
	return f, nil
}

// relaySocket serializes data writes; gorilla allows control frames and
// Close concurrently with them.
type relaySocket struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (s *relaySocket) send(f coord.Frame) error {
	f.Token = ""
	data, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("encode frame: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.conn.SetWriteDeadline(time.Now().Add(coordBridgeIOTimeout)); err != nil {
		return err
	}
	return s.conn.WriteMessage(websocket.TextMessage, data)
}

func (s *relaySocket) closeWith(code int, reason string) {
	err := s.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	if err != nil && !errors.Is(err, websocket.ErrCloseSent) && !errors.Is(err, net.ErrClosed) {
		coordBridgeLog("send websocket close: %v", err)
	}
}

// coordBridgeToken accepts the legacy bearer header or query token, or a
// token offered as a subprotocol.
func coordBridgeToken(r *http.Request) string {
	if token := bearerToken(r); token != "" {
		return token
	}
	for _, protocol := range websocket.Subprotocols(r) {
		if token, ok := strings.CutPrefix(protocol, CoordTokenSubprotocolPrefix); ok {
			return token
		}
	}
	return ""
}

func (b *CoordinatedBridge) authorized(token string) bool {
	if token == "" {
		return false
	}
	if b.token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(b.token)) == 1 {
		return true
	}
	return b.tokens != nil && b.tokens.Contains(token)
}

// guard returns a revocation check for store device tokens. The static token
// cannot be revoked without a restart, so it gets none.
func (b *CoordinatedBridge) guard(token string) func() bool {
	if b.tokens == nil || (b.token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(b.token)) == 1) {
		return nil
	}
	return func() bool { return b.tokens.Contains(token) }
}

// localOnly refuses requests whose Host is not a loopback name, which
// defeats DNS rebinding against the loopback listener.
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "no-referrer")
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if !loopbackHost(host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// sameOrigin admits clients without an Origin, such as local tools, and
// browsers only from the page this bridge serves.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func coordBridgeLog(format string, args ...any) {
	if _, err := fmt.Fprintf(os.Stderr, "tau websocket bridge: "+format+"\n", args...); err != nil {
		return
	}
}
