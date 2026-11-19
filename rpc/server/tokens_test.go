package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenStoreAddListRevoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	store := NewTokenStore(path)

	tok, err := store.Add("phone")
	if err != nil {
		t.Fatalf("add phone: %v", err)
	}
	if tok.Name != "phone" || len(tok.Token) < 32 {
		t.Fatalf("token = %#v, want named token with long secret", tok)
	}
	if tok.Created.IsZero() {
		t.Fatal("created timestamp is zero")
	}

	tokens, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tokens) != 1 || tokens[0].Token != tok.Token {
		t.Fatalf("list = %#v, want the added token", tokens)
	}

	if !store.Contains(tok.Token) {
		t.Fatal("store does not contain the added token")
	}
	if store.Contains("bogus") || store.Contains("") {
		t.Fatal("store accepted a token that was never added")
	}

	// The file must be readable only by the owner since it holds secrets.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("token file perms = %o, want 600", perm)
	}

	// Revoking an unknown name reports false without touching the store.
	found, err := store.Revoke("tablet")
	if err != nil || found {
		t.Fatalf("revoke unknown = (%t, %v), want (false, nil)", found, err)
	}
	if !store.Contains(tok.Token) {
		t.Fatal("revoking an unknown name removed the existing token")
	}

	found, err = store.Revoke("phone")
	if err != nil || !found {
		t.Fatalf("revoke phone = (%t, %v), want (true, nil)", found, err)
	}
	if store.Contains(tok.Token) {
		t.Fatal("revoked token still accepted")
	}

	tokens, err = store.List()
	if err != nil {
		t.Fatalf("list after revoke: %v", err)
	}
	if len(tokens) != 0 {
		t.Fatalf("list after revoke = %#v, want empty", tokens)
	}
}

func TestTokenStoreAddReplacesSameName(t *testing.T) {
	store := NewTokenStore(filepath.Join(t.TempDir(), "tokens.json"))

	first, err := store.Add("phone")
	if err != nil {
		t.Fatalf("add phone: %v", err)
	}
	second, err := store.Add("phone")
	if err != nil {
		t.Fatalf("re-add phone: %v", err)
	}
	if first.Token == second.Token {
		t.Fatal("re-adding a device reused the old token")
	}

	tokens, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("list = %#v, want single entry per device", tokens)
	}
	if store.Contains(first.Token) {
		t.Fatal("old token still accepted after re-adding the device")
	}
	if !store.Contains(second.Token) {
		t.Fatal("replacement token not accepted")
	}
}

func TestTokenStorePersistsAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")

	tok, err := NewTokenStore(path).Add("tablet")
	if err != nil {
		t.Fatalf("add tablet: %v", err)
	}

	reopened := NewTokenStore(path)
	if !reopened.Contains(tok.Token) {
		t.Fatal("reopened store did not accept the persisted token")
	}

	var stored struct {
		Tokens []RemoteToken `json:"-"`
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	var entries []RemoteToken
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("parse token file: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "tablet" || entries[0].Token != tok.Token {
		t.Fatalf("token file entries = %#v, want the tablet token", entries)
	}
	stored.Tokens = entries
}

func TestTokenStoreRejectsEmptyName(t *testing.T) {
	store := NewTokenStore(filepath.Join(t.TempDir(), "tokens.json"))
	if _, err := store.Add(""); err == nil || !strings.Contains(err.Error(), "name cannot be empty") {
		t.Fatalf("add empty name err = %v, want name cannot be empty", err)
	}
	found, err := store.Revoke("")
	if err == nil || found {
		t.Fatalf("revoke empty name = (%t, %v), want error", found, err)
	}
}

func TestTokenStorePublishesWholeSnapshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	writer, reader := NewTokenStore(path), NewTokenStore(path)
	keep, err := writer.Add("keep")
	if err != nil {
		t.Fatal(err)
	}
	initial, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := writer.Add("changed")
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(initial, current) || current.Mode().Perm() != 0o600 {
		t.Fatal("add didn't publish a new private snapshot")
	}
	if !reader.Contains(keep.Token) || !reader.Contains(changed.Token) {
		t.Fatal("reader did not accept both device tokens")
	}
	if found, err := writer.Revoke("changed"); err != nil || !found {
		t.Fatalf("revoke = %t, %v", found, err)
	}
	if !reader.Contains(keep.Token) || reader.Contains(changed.Token) {
		t.Fatal("reader didn't see the updated snapshot")
	}
}
