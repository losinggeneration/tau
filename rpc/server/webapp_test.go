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
