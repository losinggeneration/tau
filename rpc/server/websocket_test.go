package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	sessionpkg "github.com/losinggeneration/tau/agent/session"
	"github.com/losinggeneration/tau/sdk"
)

func dialBridge(t *testing.T, ctx context.Context, url string, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	return websocket.DefaultDialer.DialContext(ctx, url, header)
}

func writeWSCommand(t *testing.T, conn *websocket.Conn, id, typ string, args any) {
	t.Helper()
	data, err := json.Marshal(Command{ID: id, Type: typ, Args: mustRawArgs(t, args)})
	if err != nil {
		t.Fatalf("marshal command: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write command %s: %v", id, err)
	}
}

func mustRawArgs(t *testing.T, args any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return data
}

func readWSResponse(t *testing.T, conn *websocket.Conn, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for response %s", id)
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read response %s: %v", id, err)
		}
		var obj map[string]any
		if err := json.Unmarshal(msg, &obj); err != nil {
			t.Fatalf("parse websocket message %q: %v", msg, err)
		}
		if obj["id"] == id {
			return obj
		}
	}
}

func TestWebSocketBridgeRequiresToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mux := http.NewServeMux()
	mux.HandleFunc("/rpc", func(w http.ResponseWriter, r *http.Request) {
		if !validWebSocketToken(r, "secret") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/rpc"
	_, resp, err := dialBridge(t, ctx, url, nil)
	if err == nil {
		t.Fatal("dial without token succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401", resp)
	}
}

func TestWebSocketBridgeWithoutTokenOrStoreFails(t *testing.T) {
	if _, err := NewWebSocketBridge("127.0.0.1:0", "", &mockRuntime{}, newTestRegistry(), nil); err == nil {
		t.Fatal("bridge without token or store succeeded; want error")
	}
}

func TestWebSocketBridgeAcceptsStoredAndRevokedTokens(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := NewTokenStore(filepath.Join(t.TempDir(), "tokens.json"))
	tok, err := store.Add("phone")
	if err != nil {
		t.Fatalf("add device token: %v", err)
	}

	bridge, err := NewWebSocketBridge("127.0.0.1:0", "", &mockRuntime{}, newTestRegistry(), nil, WithTokenStore(store))
	if err != nil {
		t.Fatalf("new bridge with store: %v", err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- bridge.Run(ctx) }()

	dial := func() (*websocket.Conn, *http.Response, error) {
		return dialBridge(t, ctx, "ws://"+bridge.Addr()+"/rpc", nil)
	}

	// Stored device token upgrades.
	conn, resp, err := dialBridge(t, ctx, "ws://"+bridge.Addr()+"/rpc?token="+tok.Token, nil)
	if err != nil {
		t.Fatalf("dial with stored token: %v (resp %v)", err, resp)
	}
	defer func() { _ = conn.Close() }()

	// The connection works before revocation.
	cmd := Command{ID: "revive-1", Type: "get_state", Args: json.RawMessage(`{}`)}
	data, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal command: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write command: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("no response before revocation")
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read response before revocation: %v", err)
		}
		if strings.Contains(string(msg), "revive-1") {
			break
		}
	}
	_ = conn.SetReadDeadline(time.Time{})

	// Unknown token is rejected.
	_, resp, err = dial()
	if err == nil {
		t.Fatal("dial with unknown token succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown token status = %v, want 401", resp)
	}

	// Revoking stops the previously valid token immediately.
	if found, err := store.Revoke("phone"); err != nil || !found {
		t.Fatalf("revoke phone = (%t, %v), want (true, nil)", found, err)
	}
	_, resp, err = dial()
	if err == nil {
		t.Fatal("dial with revoked token succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token status = %v, want 401", resp)
	}

	// The live connection is cut at its next message after revocation.
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write command after revocation: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("connection survived a command after revocation; want close")
	}

	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("run bridge: %v", err)
	}
}

func TestWebSocketBridgeStaticTokenStillWorks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bridge, err := NewWebSocketBridge("127.0.0.1:0", "static-secret", &mockRuntime{}, newTestRegistry(), nil, WithTokenStore(NewTokenStore(filepath.Join(t.TempDir(), "tokens.json"))))
	if err != nil {
		t.Fatalf("new bridge: %v", err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- bridge.Run(ctx) }()

	header := http.Header{}
	header.Set("Authorization", "Bearer static-secret")
	conn, resp, err := dialBridge(t, ctx, "ws://"+bridge.Addr()+"/rpc", header)
	if err != nil {
		t.Fatalf("dial with static token: %v (resp %v)", err, resp)
	}
	_ = conn.Close()

	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("run bridge: %v", err)
	}
}

func TestWebSocketBridgeRejectsSecondLiveResume(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sessionsDir := t.TempDir()
	storage, err := sessionpkg.NewStorageAt(sessionsDir)
	if err != nil {
		t.Fatalf("new storage: %v", err)
	}
	header := sessionpkg.NewHeader(".", nil, nil, "shared")
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close seed writer: %v", err)
	}

	runtime, err := sdk.NewRuntime(sdk.RuntimeOptions{SessionsDir: sessionsDir})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	defer func() { _ = runtime.Close(context.Background()) }()

	bridge, err := NewWebSocketBridge("127.0.0.1:0", "secret", runtime, newTestRegistry(), nil)
	if err != nil {
		t.Fatalf("new bridge: %v", err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- bridge.Run(ctx) }()

	conn1, _, err := dialBridge(t, ctx, "ws://"+bridge.Addr()+"/rpc?token=secret", nil)
	if err != nil {
		t.Fatalf("dial conn1: %v", err)
	}
	defer func() { _ = conn1.Close() }()
	conn2, _, err := dialBridge(t, ctx, "ws://"+bridge.Addr()+"/rpc?token=secret", nil)
	if err != nil {
		t.Fatalf("dial conn2: %v", err)
	}
	defer func() { _ = conn2.Close() }()
	if _, _, err := conn1.ReadMessage(); err != nil {
		t.Fatalf("read conn1 meta: %v", err)
	}
	if _, _, err := conn2.ReadMessage(); err != nil {
		t.Fatalf("read conn2 meta: %v", err)
	}

	writeWSCommand(t, conn1, "resume-1", "resume_session", map[string]any{"id": header.ID})
	if resp := readWSResponse(t, conn1, "resume-1"); resp["ok"] != true {
		t.Fatalf("first resume response = %#v, want ok", resp)
	}

	writeWSCommand(t, conn2, "resume-2", "resume_session", map[string]any{"id": header.ID})
	resp := readWSResponse(t, conn2, "resume-2")
	if resp["ok"] != false {
		t.Fatalf("second resume response = %#v, want failure", resp)
	}
	errObj, ok := resp["error"].(map[string]any)
	if !ok || !strings.Contains(fmt.Sprint(errObj["message"]), "already open") {
		t.Fatalf("second resume error = %#v, want already open", resp["error"])
	}

	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("run bridge: %v", err)
	}
}

func TestWebSocketBridgeConnectionSurvivesHandlerReturn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bridge, err := NewWebSocketBridge("127.0.0.1:0", "secret", &mockRuntime{}, newTestRegistry(), nil)
	if err != nil {
		t.Fatalf("new bridge: %v", err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- bridge.Run(ctx) }()

	conn, resp, err := dialBridge(t, ctx, "ws://"+bridge.Addr()+"/rpc?token=secret", nil)
	if err != nil {
		t.Fatalf("dial: %v (resp %v)", err, resp)
	}
	defer func() { _ = conn.Close() }()

	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read meta: %v", err)
	}

	// Give the HTTP handler time to return; a hijacked request context is
	// canceled around then, so this is the window where the connection
	// goroutine died when it inherited the request context.
	time.Sleep(100 * time.Millisecond)

	cmd := Command{ID: "alive-1", Type: "get_state", Args: json.RawMessage(`{}`)}
	data, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal command: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write command: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("no response after handler returned; connection goroutine died")
		}
		conn.SetReadDeadline(time.Now().Add(time.Second))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		if strings.Contains(string(msg), "alive-1") {
			if !strings.Contains(string(msg), `"ok":true`) {
				t.Fatalf("response = %s, want ok", msg)
			}
			return
		}
		// Skip streamed events until the response for our command shows up.
	}
}

func TestWebSocketBridgeForwardsCommands(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mux := http.NewServeMux()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	mux.HandleFunc("/rpc", func(w http.ResponseWriter, r *http.Request) {
		if !validWebSocketToken(r, "secret") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		go runWebSocketConn(ctx, conn, &mockRuntime{}, newTestRegistry(), nil, nil)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/rpc?token=secret"
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	_, meta, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if !strings.Contains(string(meta), `"type":"meta"`) {
		t.Fatalf("meta = %s", meta)
	}

	cmd := Command{ID: "state-1", Type: "get_state", Args: json.RawMessage(`{}`)}
	data, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal command: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write command: %v", err)
	}

	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	resp := parseResp(t, string(msg))
	if resp["id"] != "state-1" || resp["ok"] != true {
		t.Fatalf("response = %#v", resp)
	}
}

func TestWebSocketWriterSerializesLines(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		writer := &webSocketWriter{conn: conn}
		if _, err := writer.Write([]byte(`{"type":"meta"}` + "\n")); err != nil {
			t.Errorf("write websocket: %v", err)
		}
		if err := conn.Close(); err != nil {
			t.Errorf("close websocket: %v", err)
		}
	}))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read websocket: %v", err)
	}
	reader := bufio.NewReader(strings.NewReader(string(msg)))
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read line: %v", err)
	}
	if line != `{"type":"meta"}`+"\n" {
		t.Fatalf("line = %q", line)
	}
}
