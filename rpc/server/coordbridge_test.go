package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/losinggeneration/tau/rpc/coord"
)

type relayWorker struct {
	link     *coord.Link
	commands chan coord.Frame
}

type relayHarness struct {
	socket  string
	cwd     string
	store   *TokenStore
	static  string
	bridge  *CoordinatedBridge
	workers chan *relayWorker
}

// relaySocketDir returns a private directory short enough for a Unix socket.
func relaySocketDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if len(dir) > 80 {
		var err error
		if dir, err = os.MkdirTemp("/tmp", "relay"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.RemoveAll(dir); err != nil {
				t.Error(err)
			}
		})
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// startRelay runs a real coordinator whose launcher registers an in-process
// fake worker, plus a coordinated bridge in front of it.
func startRelay(t *testing.T) *relayHarness {
	t.Helper()
	return startRelayWith(t, nil)
}

// startRelayWith lets configure adjust the bridge before it serves.
func startRelayWith(t *testing.T, configure func(*relayHarness)) *relayHarness {
	t.Helper()
	h := &relayHarness{
		socket:  filepath.Join(relaySocketDir(t), "c.sock"),
		cwd:     t.TempDir(),
		store:   NewTokenStore(filepath.Join(t.TempDir(), "tokens.json")),
		static:  "static-secret",
		workers: make(chan *relayWorker, 4),
	}
	if resolved, err := filepath.EvalSymlinks(h.cwd); err == nil {
		h.cwd = resolved
	}
	launch := func(ctx context.Context, req coord.LaunchRequest) error {
		dialCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		link, _, err := coord.Dial(dialCtx, h.socket, coord.Frame{
			Role:  coord.RoleWorker,
			Token: req.Token,
			Instance: &coord.Instance{
				ID: req.InstanceID, NodeID: "node", SessionID: "session-" + req.InstanceID, CWD: req.CWD,
			},
		})
		if err != nil {
			return err
		}
		w := &relayWorker{link: link, commands: make(chan coord.Frame, 16)}
		go w.serve()
		h.workers <- w
		return nil
	}
	coordCtx, stopCoord := context.WithCancel(context.Background())
	coordDone := make(chan error, 1)
	go func() { coordDone <- coord.Run(coordCtx, h.socket, launch) }()
	t.Cleanup(func() {
		stopCoord()
		select {
		case err := <-coordDone:
			if err != nil {
				t.Errorf("coordinator: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("coordinator did not stop")
		}
	})
	waitForSocket(t, h.socket)

	bridge, err := NewCoordinatedBridge("127.0.0.1:0", h.socket, h.static, h.store)
	if err != nil {
		t.Fatalf("new coordinated bridge: %v", err)
	}
	h.bridge = bridge
	if configure != nil {
		configure(h)
	}
	bridgeCtx, stopBridge := context.WithCancel(context.Background())
	bridgeDone := make(chan error, 1)
	go func() { bridgeDone <- bridge.Run(bridgeCtx) }()
	t.Cleanup(func() {
		stopBridge()
		select {
		case err := <-bridgeDone:
			if err != nil {
				t.Errorf("bridge: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("bridge did not stop")
		}
	})
	return h
}

func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if info, err := os.Stat(path); err == nil && info.Mode().Type() == os.ModeSocket {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("coordinator socket did not appear")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (w *relayWorker) serve() {
	defer func() {
		if err := w.link.Close(); err != nil {
			return
		}
	}()
	for {
		f, err := w.link.Read()
		if err != nil {
			return
		}
		if f.Type != coord.TypeCommand {
			continue
		}
		w.commands <- f
		result, err := json.Marshal(map[string]any{"type": "response", "id": f.ID, "ok": true})
		if err != nil {
			return
		}
		if err := w.link.Send(coord.Frame{Type: coord.TypeResponse, ID: f.ID, Result: result}); err != nil {
			return
		}
	}
}

func (w *relayWorker) emit(t *testing.T, event string) {
	t.Helper()
	if err := w.link.Send(coord.Frame{Type: coord.TypeEvent, Event: json.RawMessage(event)}); err != nil {
		t.Fatalf("emit event: %v", err)
	}
}

func (h *relayHarness) url() string { return "ws://" + h.bridge.Addr() + "/rpc" }

func (h *relayHarness) dial(t *testing.T, dialer *websocket.Dialer, header http.Header) *websocket.Conn {
	t.Helper()
	conn, resp, err := dialer.Dial(h.url(), header)
	if err != nil {
		t.Fatalf("dial bridge: %v (%v)", err, resp)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil && !strings.Contains(err.Error(), "closed") {
			t.Error(err)
		}
	})
	return conn
}

func (h *relayHarness) dialDevice(t *testing.T, token string) *websocket.Conn {
	t.Helper()
	conn := h.dial(t, &websocket.Dialer{Subprotocols: []string{CoordSubprotocol, CoordTokenSubprotocolPrefix + token}}, nil)
	if conn.Subprotocol() != CoordSubprotocol {
		t.Fatalf("subprotocol = %q, want %q", conn.Subprotocol(), CoordSubprotocol)
	}
	return conn
}

func (h *relayHarness) worker(t *testing.T) *relayWorker {
	t.Helper()
	select {
	case w := <-h.workers:
		return w
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not register")
		return nil
	}
}

func relayRead(t *testing.T, conn *websocket.Conn) (map[string]any, string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read relay: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("relay message %q: %v", data, err)
	}
	return obj, string(data)
}

func relayResponse(t *testing.T, conn *websocket.Conn, id string) map[string]any {
	t.Helper()
	for {
		obj, _ := relayRead(t, conn)
		if obj["type"] == coord.TypeResponse && obj["id"] == id {
			return obj
		}
	}
}

func relaySend(t *testing.T, conn *websocket.Conn, frame string) {
	t.Helper()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
		t.Fatalf("write relay: %v", err)
	}
}

func relayClosed(t *testing.T, conn *websocket.Conn, code int) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		_, data, err := conn.ReadMessage()
		if err == nil {
			if strings.Contains(string(data), `"type":"event"`) {
				t.Fatalf("event relayed after close condition: %s", data)
			}
			continue
		}
		if !websocket.IsCloseError(err, code) {
			t.Fatalf("close = %v, want code %d", err, code)
		}
		return
	}
}

func TestCoordinatedBridgeClientsShareCoordinator(t *testing.T) {
	h := startRelay(t)
	device, err := h.store.Add("phone")
	if err != nil {
		t.Fatal(err)
	}
	a := h.dialDevice(t, device.Token)
	b := h.dial(t, websocket.DefaultDialer, http.Header{"Authorization": {"Bearer " + h.static}})

	welcomeA, rawA := relayRead(t, a)
	welcomeB, _ := relayRead(t, b)
	if welcomeA["type"] != coord.TypeWelcome || welcomeA["role"] != coord.RoleClient || welcomeB["role"] != coord.RoleClient {
		t.Fatalf("welcomes = %v / %v, want client role", welcomeA, welcomeB)
	}
	if welcomeA["client_id"] == "" || welcomeA["client_id"] == welcomeB["client_id"] {
		t.Fatalf("clients share identity: %v / %v", welcomeA["client_id"], welcomeB["client_id"])
	}
	if strings.Contains(rawA, device.Token) {
		t.Fatalf("welcome leaked credential: %s", rawA)
	}

	// Credentials and worker identity a browser smuggles in are dropped.
	start, err := json.Marshal(map[string]any{
		"type": "start_instance", "id": "start", "cwd": h.cwd,
		"role": "worker", "token": device.Token, "client_id": "spoof",
		"instance": map[string]any{"instance_id": "forged", "node_id": "evil"},
	})
	if err != nil {
		t.Fatal(err)
	}
	relaySend(t, a, string(start))
	started := relayResponse(t, a, "start")
	instance, _ := started["instance_id"].(string)
	if started["error"] != nil || instance == "" || instance == "forged" {
		t.Fatalf("start = %v", started)
	}
	w := h.worker(t)

	relaySend(t, b, `{"type":"list_instances","id":"list"}`)
	listed := relayResponse(t, b, "list")
	if instances, _ := listed["instances"].([]any); len(instances) != 1 {
		t.Fatalf("list from second client = %v, want shared instance", listed)
	}
	relaySend(t, b, `{"type":"attach","id":"attach","instance_id":"`+instance+`"}`)
	if attached := relayResponse(t, b, "attach"); attached["error"] != nil {
		t.Fatalf("attach = %v", attached)
	}

	relaySend(t, a, `{"type":"command","id":"cmd","instance_id":"`+instance+`","client_id":"spoof","token":"x","command":{"type":"get_state"}}`)
	if resp := relayResponse(t, a, "cmd"); resp["error"] != nil || resp["instance_id"] != instance {
		t.Fatalf("command response = %v", resp)
	}
	select {
	case cmd := <-w.commands:
		if cmd.ClientID != welcomeA["client_id"] || cmd.Token != "" {
			t.Fatalf("worker saw client %q token %q, want bridge-assigned identity %q", cmd.ClientID, cmd.Token, welcomeA["client_id"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not receive command")
	}

	w.emit(t, `{"type":"text_delta","delta":"hi"}`)
	event, raw := relayRead(t, b)
	if event["type"] != coord.TypeEvent || event["instance_id"] != instance || !strings.Contains(raw, `"delta":"hi"`) {
		t.Fatalf("subscriber event = %s", raw)
	}
}

func TestCoordinatedBridgeRelaysRequestCorrelationWithoutTrust(t *testing.T) {
	h := startRelay(t)
	auth := http.Header{"Authorization": {"Bearer " + h.static}}
	a := h.dial(t, websocket.DefaultDialer, auth)
	b := h.dial(t, websocket.DefaultDialer, auth)
	welcomeA, _ := relayRead(t, a)
	relayRead(t, b)

	launchID := uuid.NewString()
	relaySend(t, a, `{"type":"start_instance","id":"start","request_id":"`+launchID+`","client_id":"spoof","cwd":"`+h.cwd+`"}`)
	started := relayResponse(t, a, "start")
	instance, _ := started["instance_id"].(string)
	summary, _ := started["instance"].(map[string]any)
	if started["error"] != nil || started["request_id"] != launchID || summary["launch_request_id"] != launchID || instance == launchID {
		t.Fatalf("start = %v", started)
	}
	w := h.worker(t)

	// Another client of the same user reconciles the launch by its retained ID.
	relaySend(t, b, `{"type":"list_instances","id":"list"}`)
	listed, _ := relayResponse(t, b, "list")["instances"].([]any)
	if len(listed) != 1 {
		t.Fatalf("listed = %v", listed)
	}
	if entry, _ := listed[0].(map[string]any); entry["launch_request_id"] != launchID {
		t.Fatalf("listed = %v", listed)
	}

	commandID := uuid.NewString()
	relaySend(t, a, `{"type":"command","id":"cmd","request_id":"`+commandID+`","client_id":"spoof","instance_id":"`+instance+`","command":{"type":"get_state"}}`)
	if resp := relayResponse(t, a, "cmd"); resp["error"] != nil || resp["request_id"] != commandID {
		t.Fatalf("command response = %v", resp)
	}
	select {
	case cmd := <-w.commands:
		if cmd.RequestID != commandID || cmd.ClientID != welcomeA["client_id"] || cmd.InstanceID != instance {
			t.Fatalf("worker command = %+v, want request %q from %q", cmd, commandID, welcomeA["client_id"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not receive command")
	}

	relaySend(t, a, `{"type":"command","id":"long","request_id":"`+strings.Repeat("x", 300)+`","instance_id":"`+instance+`","command":{"type":"get_state"}}`)
	if resp := relayResponse(t, a, "long"); resp["error"] == nil || resp["request_id"] != nil {
		t.Fatalf("oversized request id = %v, want rejection", resp)
	}
	relaySend(t, a, `{"type":"command","id":"bad","request_id":"not-a-uuid","instance_id":"`+instance+`","command":{"type":"get_state"}}`)
	if resp := relayResponse(t, a, "bad"); resp["error"] == nil || resp["request_id"] != nil {
		t.Fatalf("non-UUID request id = %v, want rejection", resp)
	}
}

func TestCoordinatedBridgeRejectsUnauthorizedRequests(t *testing.T) {
	h := startRelay(t)
	cases := []struct {
		name   string
		url    string
		header http.Header
		want   int
	}{
		{"no token", h.url(), nil, http.StatusUnauthorized},
		{"wrong token", h.url(), http.Header{"Authorization": {"Bearer nope"}}, http.StatusUnauthorized},
		{"valid query token", h.url() + "?token=" + h.static, nil, http.StatusUnauthorized},
		{"query token beside header", h.url() + "?token=" + h.static, http.Header{"Authorization": {"Bearer " + h.static}}, http.StatusUnauthorized},
		{"foreign origin", h.url(), http.Header{"Authorization": {"Bearer " + h.static}, "Origin": {"http://evil.example"}}, http.StatusForbidden},
		{"cross-scheme origin", h.url(), http.Header{"Authorization": {"Bearer " + h.static}, "Origin": {"https://" + h.bridge.Addr()}}, http.StatusForbidden},
		{"null origin", h.url(), http.Header{"Authorization": {"Bearer " + h.static}, "Origin": {"null"}}, http.StatusForbidden},
		{"repeated origin", h.url(), http.Header{"Authorization": {"Bearer " + h.static}, "Origin": {"http://" + h.bridge.Addr(), "http://" + h.bridge.Addr()}}, http.StatusForbidden},
		{"rebound host", h.url(), http.Header{"Authorization": {"Bearer " + h.static}, "Host": {"evil.example"}}, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, resp, err := websocket.DefaultDialer.Dial(tc.url, tc.header)
			if err == nil {
				if closeErr := conn.Close(); closeErr != nil {
					t.Error(closeErr)
				}
				t.Fatal("dial succeeded")
			}
			if resp == nil || resp.StatusCode != tc.want {
				t.Fatalf("response = %v, want %d", resp, tc.want)
			}
		})
	}
	// A same-origin page offering its credential as a subprotocol connects.
	dialer := &websocket.Dialer{Subprotocols: []string{CoordSubprotocol, CoordTokenSubprotocolPrefix + h.static}}
	conn, resp, err := dialer.Dial(h.url(), http.Header{"Origin": {"http://" + h.bridge.Addr()}})
	if err != nil {
		t.Fatalf("same-origin dial: %v (%v)", err, resp)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	}()
	if welcome, _ := relayRead(t, conn); welcome["type"] != coord.TypeWelcome {
		t.Fatalf("same-origin connection received %v", welcome)
	}
}

func TestCoordinatedBridgeOriginPolicy(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		origins []string
		want    bool
	}{
		{"native client without origin", "127.0.0.1:8080", nil, true},
		{"same origin", "127.0.0.1:8080", []string{"http://127.0.0.1:8080"}, true},
		{"same localhost origin", "localhost:8080", []string{"http://localhost:8080"}, true},
		{"same ipv6 origin", "[::1]:8080", []string{"http://[::1]:8080"}, true},
		{"https scheme", "127.0.0.1:8080", []string{"https://127.0.0.1:8080"}, false},
		{"websocket scheme", "127.0.0.1:8080", []string{"ws://127.0.0.1:8080"}, false},
		{"uppercase scheme", "127.0.0.1:8080", []string{"HTTP://127.0.0.1:8080"}, false},
		{"other port", "127.0.0.1:8080", []string{"http://127.0.0.1:8081"}, false},
		{"missing port", "127.0.0.1:8080", []string{"http://127.0.0.1"}, false},
		{"other loopback name", "127.0.0.1:8080", []string{"http://localhost:8080"}, false},
		{"foreign host", "127.0.0.1:8080", []string{"http://evil.example:8080"}, false},
		{"trailing slash", "127.0.0.1:8080", []string{"http://127.0.0.1:8080/"}, false},
		{"path", "127.0.0.1:8080", []string{"http://127.0.0.1:8080/rpc"}, false},
		{"query", "127.0.0.1:8080", []string{"http://127.0.0.1:8080?x=1"}, false},
		{"userinfo", "127.0.0.1:8080", []string{"http://user@127.0.0.1:8080"}, false},
		{"padded", "127.0.0.1:8080", []string{" http://127.0.0.1:8080"}, false},
		{"null", "127.0.0.1:8080", []string{"null"}, false},
		{"empty", "127.0.0.1:8080", []string{""}, false},
		{"comma list", "127.0.0.1:8080", []string{"http://127.0.0.1:8080, http://127.0.0.1:8080"}, false},
		{"repeated", "127.0.0.1:8080", []string{"http://127.0.0.1:8080", "http://127.0.0.1:8080"}, false},
		{"non-loopback host", "example.com:8080", []string{"http://example.com:8080"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/rpc", nil)
			if tc.origins != nil {
				r.Header["Origin"] = tc.origins
			}
			if got := sameOrigin(r); got != tc.want {
				t.Fatalf("sameOrigin(%q on %s) = %t, want %t", tc.origins, tc.host, got, tc.want)
			}
		})
	}
}

func TestCoordinatedBridgeTokenSources(t *testing.T) {
	protocol := "Sec-WebSocket-Protocol"
	cases := []struct {
		name   string
		target string
		header http.Header
		want   string
	}{
		{"bearer header", "/rpc", http.Header{"Authorization": {"Bearer secret"}}, "secret"},
		{"token subprotocol", "/rpc", http.Header{protocol: {CoordSubprotocol + ", " + CoordTokenSubprotocolPrefix + "secret"}}, "secret"},
		{"none", "/rpc", nil, ""},
		{"query token", "/rpc?token=secret", nil, ""},
		{"query token beside header", "/rpc?token=secret", http.Header{"Authorization": {"Bearer secret"}}, ""},
		{"non-bearer header", "/rpc", http.Header{"Authorization": {"Basic secret"}}, ""},
		{"repeated header", "/rpc", http.Header{"Authorization": {"Bearer secret", "Bearer secret"}}, ""},
		{"header and subprotocol", "/rpc", http.Header{"Authorization": {"Bearer secret"}, protocol: {CoordTokenSubprotocolPrefix + "secret"}}, ""},
		{"two subprotocol tokens", "/rpc", http.Header{protocol: {CoordTokenSubprotocolPrefix + "a, " + CoordTokenSubprotocolPrefix + "b"}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080"+tc.target, nil)
			for k, v := range tc.header {
				r.Header[http.CanonicalHeaderKey(k)] = v
			}
			if got := coordBridgeToken(r); got != tc.want {
				t.Fatalf("token = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCoordinatedBridgeRechecksDeviceAfterDial(t *testing.T) {
	h := startRelayWith(t, func(h *relayHarness) {
		h.bridge.dial = func(ctx context.Context, socket string, hello coord.Frame) (*coord.Link, coord.Frame, error) {
			link, welcome, err := coord.Dial(ctx, socket, hello)
			if _, revokeErr := h.store.Revoke("phone"); revokeErr != nil {
				t.Errorf("revoke during dial: %v", revokeErr)
			}
			return link, welcome, err
		}
	})
	phone, err := h.store.Add("phone")
	if err != nil {
		t.Fatal(err)
	}
	dialer := &websocket.Dialer{Subprotocols: []string{CoordSubprotocol, CoordTokenSubprotocolPrefix + phone.Token}}
	conn, resp, err := dialer.Dial(h.url(), nil)
	if err == nil {
		if closeErr := conn.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("device revoked during dial was upgraded")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("response = %v, want %d", resp, http.StatusUnauthorized)
	}
	if h.store.Contains(phone.Token) {
		t.Fatal("dial hook did not revoke the device")
	}

	// The static token cannot be revoked, so it still connects.
	static := h.dial(t, websocket.DefaultDialer, http.Header{"Authorization": {"Bearer " + h.static}})
	if welcome, _ := relayRead(t, static); welcome["type"] != coord.TypeWelcome {
		t.Fatalf("static connection received %v", welcome)
	}
}

func TestCoordinatedBridgePageForbidsReferrer(t *testing.T) {
	h := startRelay(t)
	resp, err := http.Get("http://" + h.bridge.Addr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("page status %d referrer policy %q", resp.StatusCode, resp.Header.Get("Referrer-Policy"))
	}
}

func TestCoordinatedBridgeRevocationCutsBothDirections(t *testing.T) {
	h := startRelay(t)
	phone, err := h.store.Add("phone")
	if err != nil {
		t.Fatal(err)
	}
	laptop, err := h.store.Add("laptop")
	if err != nil {
		t.Fatal(err)
	}
	subscriber := h.dialDevice(t, phone.Token)
	sender := h.dialDevice(t, laptop.Token)
	relayRead(t, subscriber)
	relayRead(t, sender)

	relaySend(t, subscriber, `{"type":"start_instance","id":"start","cwd":"`+h.cwd+`"}`)
	instance, _ := relayResponse(t, subscriber, "start")["instance_id"].(string)
	w := h.worker(t)
	relaySend(t, subscriber, `{"type":"attach","id":"attach","instance_id":"`+instance+`"}`)
	relayResponse(t, subscriber, "attach")

	for _, name := range []string{"phone", "laptop"} {
		if found, err := h.store.Revoke(name); err != nil || !found {
			t.Fatalf("revoke %s = %t, %v", name, found, err)
		}
	}
	w.emit(t, `{"type":"text_delta","delta":"secret"}`)
	relayClosed(t, subscriber, websocket.ClosePolicyViolation)

	relaySend(t, sender, `{"type":"list_instances","id":"list"}`)
	relayClosed(t, sender, websocket.ClosePolicyViolation)
}

func TestCoordinatedBridgeEnforcesMessageShape(t *testing.T) {
	h := startRelay(t)
	conn := h.dial(t, websocket.DefaultDialer, http.Header{"Authorization": {"Bearer " + h.static}})
	relayRead(t, conn)

	relaySend(t, conn, `{"type":"session_directory","id":"dir","cwd":"/tmp"}`)
	if resp := relayResponse(t, conn, "dir"); resp["error"] == nil {
		t.Fatalf("browser session directory lookup allowed: %v", resp)
	}
	relaySend(t, conn, `{"type":"hello","id":"h","role":"worker","token":"x"}`)
	if resp := relayResponse(t, conn, "h"); resp["error"] == nil {
		t.Fatalf("browser hello = %v, want rejection", resp)
	}
	relaySend(t, conn, `{"type":"list_instances","id":"a"} {"type":"list_instances","id":"b"}`)
	if obj, _ := relayRead(t, conn); obj["error"] == nil {
		t.Fatalf("two records in one message = %v, want rejection", obj)
	}
	relaySend(t, conn, `{"type":"list_instances","id":"`+strings.Repeat("x", 300)+`"}`)
	if obj, _ := relayRead(t, conn); obj["error"] == nil {
		t.Fatalf("oversized id = %v, want rejection", obj)
	}
	relaySend(t, conn, `{"type":"list_instances","id":"still-open"}`)
	if resp := relayResponse(t, conn, "still-open"); resp["error"] != nil {
		t.Fatalf("list after rejections = %v", resp)
	}

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	relayClosed(t, conn, websocket.CloseUnsupportedData)
}

func TestCoordinatedBridgeWithoutCoordinatorRefusesUpgrade(t *testing.T) {
	bridge, err := NewCoordinatedBridge("127.0.0.1:0", filepath.Join(t.TempDir(), "missing.sock"), "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bridge.Run(ctx) }()
	_, resp, err := websocket.DefaultDialer.Dial("ws://"+bridge.Addr()+"/rpc", http.Header{"Authorization": {"Bearer secret"}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("dial = %v, %v; want 503", resp, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatedBridgeIsLoopbackOnly(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "c.sock")
	for _, addr := range []string{"0.0.0.0:0", ":0", "[::]:0", "example.com:0"} {
		bridge, err := NewCoordinatedBridge(addr, socket, "secret", nil)
		if err == nil {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			t.Errorf("listen %s succeeded at %s; want refusal", addr, bridge.Addr())
			if err := bridge.Run(ctx); err != nil {
				t.Error(err)
			}
		}
	}
	if _, err := NewCoordinatedBridge("127.0.0.1:0", socket, "", nil); err == nil {
		t.Fatal("bridge without credentials succeeded")
	}
}
