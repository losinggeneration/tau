package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestWebAppServesControllerAndAssets(t *testing.T) {
	bridge, err := NewWebSocketBridge("127.0.0.1:0", "secret", &mockRuntime{}, newTestRegistry(), nil)
	if err != nil {
		t.Fatalf("new bridge: %v", err)
	}
	defer func() { _ = bridge.listener.Close() }()

	cases := []struct {
		path        string
		contentType string
		want        []string
	}{
		{"/", "text/html", []string{"tau remote", "machine", "width:auto"}},
		{"/app.js", "text/javascript", []string{"list_sessions", "pin_session", "unpin_session", "hostname", "Notification", "ui_response", "Open a session first"}},
		{"/manifest.webmanifest", "application/manifest+json", []string{"standalone"}},
		{"/sw.js", "text/javascript", []string{"tau-remote-v1"}},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(http.MethodGet, tc.path, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		rec := newResponseRecorder()
		bridge.server.Handler.ServeHTTP(rec, req)
		if rec.status != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", tc.path, rec.status)
		}
		if got := rec.header.Get("Content-Type"); !strings.HasPrefix(got, tc.contentType) {
			t.Fatalf("%s content-type = %q, want prefix %q", tc.path, got, tc.contentType)
		}
		for _, want := range tc.want {
			if !strings.Contains(rec.body.String(), want) {
				t.Fatalf("%s body does not contain %q", tc.path, want)
			}
		}
	}
}

func TestCoordinatedWebAppServesInstanceController(t *testing.T) {
	mux := http.NewServeMux()
	registerCoordinatedWebApp(mux)
	for _, tc := range []struct {
		path string
		want []string
		omit []string
	}{
		{"/", []string{"Running instances", "Saved sessions", "Start instance", "Detach", "Stop instance"}, []string{`id="url"`, `<h2>Sessions</h2>`, `id="steer"`}},
		{"/app.js", []string{"watch_instances", "instance_update", "get_ui_requests", "ui_resolved"}, nil},
	} {
		req, err := http.NewRequest(http.MethodGet, tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		rec := newResponseRecorder()
		mux.ServeHTTP(rec, req)
		if rec.status != http.StatusOK {
			t.Fatalf("%s status = %d", tc.path, rec.status)
		}
		for _, want := range tc.want {
			if !strings.Contains(rec.body.String(), want) {
				t.Fatalf("%s missing %q", tc.path, want)
			}
		}
		for _, omit := range tc.omit {
			if strings.Contains(rec.body.String(), omit) {
				t.Fatalf("%s unexpectedly contains %q", tc.path, omit)
			}
		}
	}
}

type responseRecorder struct {
	header http.Header
	body   strings.Builder
	status int
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{header: make(http.Header), status: http.StatusOK}
}

func (r *responseRecorder) Header() http.Header { return r.header }

func (r *responseRecorder) Write(data []byte) (int, error) { return r.body.Write(data) }

func (r *responseRecorder) WriteHeader(statusCode int) { r.status = statusCode }

var (
	_ http.ResponseWriter = (*responseRecorder)(nil)
	_ io.Writer           = (*responseRecorder)(nil)
)
