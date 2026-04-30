package session

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/ai/store"
)

// TestCLIShareSuccess verifies that CLI.Share succeeds end-to-end with a fake
// GitHub Gist endpoint, correct auth header, and returns the gist URL.
func TestCLIShareSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	sessionDir := filepath.Join(tmpDir, "sessions")

	storage, err := NewStorageAt(sessionDir)
	if err != nil {
		t.Fatalf("NewStorageAt: %v", err)
	}

	// Write a github credential to a temp auth file.
	authPath := filepath.Join(tmpDir, "auth.json")
	if err := os.MkdirAll(filepath.Dir(authPath), 0o700); err != nil {
		t.Fatalf("create auth dir: %v", err)
	}
	creds := map[string]ai.Credential{
		"github": {
			Type:   ai.CredentialTypeAPIKey,
			APIKey: "test-gh-token-abc123",
		},
	}
	if err := writeTestAuth(authPath, creds); err != nil {
		t.Fatalf("write test auth: %v", err)
	}

	// Override config dir so CLI loads our temp auth file.
	t.Setenv("TAU_CONFIG_DIR", tmpDir)

	var (
		gotPath        string
		gotAuth        string
		gotContentType string
		gotBody        map[string]any
	)
	gistServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"abc123def456","html_url":"https://gist.github.com/abc123def456"}`)
	}))
	defer gistServer.Close()

	cli := &CLI{
		Storage:        storage,
		GistURL:        gistServer.URL,
		GistStatusCode: 200,
	}
	if err := cli.openStoreForTest(authPath); err != nil {
		t.Fatalf("open store: %v", err)
	}

	// Create a session so share has something to export.
	header := NewHeader(t.TempDir(), nil, nil)
	w, err := storage.Create(header)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	url, err := cli.Share(context.Background(), header.ID[:8], false)
	if err != nil {
		t.Fatalf("Share: %v", err)
	}

	if !strings.Contains(url, "abc123def456") {
		t.Errorf("Share url = %q, want gist ID embedded", url)
	}

	// The httptest server root is our mock /gists endpoint.
	if gotPath != "/" {
		t.Errorf("request path = %q, want / (server root = /gists)", gotPath)
	}

	if gotAuth != "Bearer test-gh-token-abc123" {
		t.Errorf("Authorization = %q, want Bearer token", gotAuth)
	}

	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}

	// Verify gist request body fields.
	desc, ok := gotBody["description"].(string)
	if !ok || !strings.Contains(desc, "Tau session") {
		t.Errorf("description = %v, want string containing 'Tau session'", gotBody["description"])
	}

	isPublic, ok := gotBody["public"].(bool)
	if !ok || isPublic {
		t.Errorf("public = %v, want false (secret gist by default)", isPublic)
	}

	files, ok := gotBody["files"].(map[string]any)
	if !ok {
		t.Fatalf("files = %v, want map", gotBody["files"])
	}
	if _, ok := files["session.html"]; !ok {
		t.Errorf("files = %v, want session.html key", files)
	}
}

// TestCLISharePublic verifies that --public creates a public gist.
func TestCLISharePublic(t *testing.T) {
	tmpDir := t.TempDir()
	sessionDir := filepath.Join(tmpDir, "sessions")

	storage, err := NewStorageAt(sessionDir)
	if err != nil {
		t.Fatalf("NewStorageAt: %v", err)
	}

	authPath := filepath.Join(tmpDir, "auth.json")
	if err := writeTestAuth(authPath, map[string]ai.Credential{
		"github": {Type: ai.CredentialTypeAPIKey, APIKey: "tok"},
	}); err != nil {
		t.Fatalf("write test auth: %v", err)
	}

	var gotBody map[string]any
	gistServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"pub123","html_url":"https://gist.github.com/pub123"}`)
	}))
	defer gistServer.Close()

	cli := &CLI{Storage: storage, GistURL: gistServer.URL, GistStatusCode: 200}
	if err := cli.openStoreForTest(authPath); err != nil {
		t.Fatalf("open store: %v", err)
	}

	header := NewHeader(t.TempDir(), nil, nil)
	w, err := storage.Create(header)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	_, err = cli.Share(context.Background(), header.ID[:8], true)
	if err != nil {
		t.Fatalf("Share(public=true): %v", err)
	}

	isPublic, ok := gotBody["public"].(bool)
	if !ok || !isPublic {
		t.Errorf("public = %v, want true", gotBody["public"])
	}
}

// TestCLIShareNoCredential verifies that Share returns a helpful error when
// no github credential is present.
func TestCLIShareNoCredential(t *testing.T) {
	tmpDir := t.TempDir()
	sessionDir := filepath.Join(tmpDir, "sessions")

	storage, err := NewStorageAt(sessionDir)
	if err != nil {
		t.Fatalf("NewStorageAt: %v", err)
	}

	authPath := filepath.Join(tmpDir, "auth.json")
	if err := writeTestAuth(authPath, nil); err != nil {
		t.Fatalf("write test auth: %v", err)
	}

	cli := &CLI{Storage: storage}
	if err := cli.openStoreForTest(authPath); err != nil {
		t.Fatalf("open store: %v", err)
	}

	header := NewHeader(t.TempDir(), nil, nil)
	w, err := storage.Create(header)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	_, err = cli.Share(context.Background(), header.ID[:8], false)
	if err == nil {
		t.Fatal("Share with no credential: want error, got nil")
	}
	if !strings.Contains(err.Error(), "tau login github") {
		t.Errorf("error = %q, want 'tau login github' message", err)
	}
}

// openStoreForTest replaces CLI.Store with a FileBackend pointing at authPath.
// This is a test seam to avoid configDir() picking up the real ~/.tau directory.
func (c *CLI) openStoreForTest(authPath string) error {
	fb := store.NewFileBackend(authPath, store.Options{})
	if err := fb.EnsureDir(); err != nil {
		return err
	}
	c.Store = fb
	return nil
}

// writeTestAuth writes a test auth.json file in Pi's flat provider-keyed format.
func writeTestAuth(path string, creds map[string]ai.Credential) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	wire := make(map[string]map[string]any, len(creds))
	for provider, cred := range creds {
		entry := map[string]any{"type": cred.Type}
		switch cred.Type {
		case ai.CredentialTypeAPIKey:
			entry["key"] = cred.APIKey
		case ai.CredentialTypeOAuth:
			entry["access"] = cred.AccessToken
			entry["refresh"] = cred.RefreshToken
			entry["expires"] = cred.ExpiresAt.UnixMilli()
		}
		wire[provider] = entry
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
