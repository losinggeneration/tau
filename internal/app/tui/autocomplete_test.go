package tui

import (
	"context"
	"strings"
	"testing"
)

// TestAutocompleteIntegration_BuildProviderListsLocalCommands
// verifies the autocomplete CombinedProvider is seeded with the App's
// local command registry so slash-command completions match what the
// inline selector offers.
func TestAutocompleteIntegration_BuildProviderListsLocalCommands(t *testing.T) {
	app := newAppWithSelector(t)
	provider, err := app.buildAutocompleteProvider()
	if err != nil {
		t.Fatalf("buildAutocompleteProvider: %v", err)
	}
	if provider == nil {
		t.Fatal("provider is nil")
	}

	got, err := provider.GetSuggestions(context.Background(), []string{"/he"}, 0, 3, false)
	if err != nil {
		t.Fatalf("GetSuggestions: %v", err)
	}
	if got == nil {
		t.Fatal("GetSuggestions returned nil for /he")
	}
	found := false
	for _, item := range got.Items {
		if item.Value == "help" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected /help in suggestions, got %#v", got.Items)
	}
}

// TestAutocompleteIntegration_EmptyPrefixListsAll verifies that
// GetSuggestions with an empty prefix returns every registered
// command (the same set completionItems returns), so the popup can
// populate when the user types "/".
func TestAutocompleteIntegration_EmptyPrefixListsAll(t *testing.T) {
	app := newAppWithSelector(t)
	provider, err := app.buildAutocompleteProvider()
	if err != nil {
		t.Fatalf("buildAutocompleteProvider: %v", err)
	}
	got, err := provider.GetSuggestions(context.Background(), []string{"/"}, 0, 1, false)
	if err != nil {
		t.Fatalf("GetSuggestions: %v", err)
	}
	if got == nil || len(got.Items) < 5 {
		t.Fatalf("expected many slash commands, got %d", len(got.Items))
	}
}

// TestAutocompleteIntegration_ApplyCompletionReturnsValue verifies
// that the autocomplete ApplyCompletion helper rewrites the editor
// line to use the selected item and that the resulting cursor lands
// right after the completed command.
func TestAutocompleteIntegration_ApplyCompletionReturnsValue(t *testing.T) {
	app := newAppWithSelector(t)
	line, cursor, ok := app.autocompleteCompleteItem("/he", 0, 3, "/he", "help")
	if !ok {
		t.Fatal("autocompleteCompleteItem returned ok=false")
	}
	if !strings.HasPrefix(line, "/help") {
		t.Fatalf("line after completion = %q, want prefix '/help'", line)
	}
	if cursor < 5 {
		t.Fatalf("cursor after completion = %d, want >=5", cursor)
	}
}

// TestAutocompleteIntegration_ExternalCommandsSurfaced verifies that
// session-provided slash commands (returned by sess.SlashCommands())
// show up in the autocomplete provider alongside the local registry.
func TestAutocompleteIntegration_ExternalCommandsSurfaced(t *testing.T) {
	app := newAppWithSelector(t)
	// Inject a fake external command into the test session so we
	// don't need to construct a full session with plugins.
	app.testExternalCommands = []ExternalCommand{
		{Name: "alpha", Description: "alpha cmd", Source: "test"},
		{Name: "beta", Description: "beta cmd", ArgumentHint: "<arg>", Source: "test"},
	}

	provider, err := app.buildAutocompleteProvider()
	if err != nil {
		t.Fatalf("buildAutocompleteProvider: %v", err)
	}
	got, err := provider.GetSuggestions(context.Background(), []string{"/al"}, 0, 3, false)
	if err != nil {
		t.Fatalf("GetSuggestions: %v", err)
	}
	if got == nil {
		t.Fatal("expected alpha in suggestions, got nil")
	}
	found := false
	for _, item := range got.Items {
		if item.Value == "alpha" {
			found = true
			if item.Description != "alpha cmd" {
				t.Errorf("alpha description = %q, want 'alpha cmd'", item.Description)
			}
		}
	}
	if !found {
		t.Fatalf("expected alpha suggestion, got %#v", got.Items)
	}
}

// TestAutocompleteIntegration_NilApp verifies the helpers tolerate a
// nil app without panicking.
func TestAutocompleteIntegration_NilApp(t *testing.T) {
	var nilApp *App
	if cmds := newAutocompleteCommandProvider(nilApp); cmds != nil {
		t.Errorf("nil app should produce nil commands, got %v", cmds)
	}
}

// TestCompletionPrefixFromLine verifies the prefix extractor handles
// slash, non-slash, and whitespace cases correctly.
func TestCompletionPrefixFromLine(t *testing.T) {
	tests := []struct {
		line string
		col  int
		want string
	}{
		{"/he", 3, "he"},
		{"/", 1, ""},
		{"hello", 5, ""},
		{"/help arg", 5, "help"},
		{"", 0, ""},
	}
	for _, tt := range tests {
		got := completionPrefixFromLine(tt.line, tt.col)
		if got != tt.want {
			t.Errorf("completionPrefixFromLine(%q, %d) = %q, want %q", tt.line, tt.col, got, tt.want)
		}
	}
}
