package tui

import "testing"

func TestCommandRegistryItemsAndConflicts(t *testing.T) {
	registry := NewCommandRegistry()
	registry.Register(SlashCommand{Name: "help", Description: "show help"})
	registry.Register(SlashCommand{Name: "model", ArgumentHint: "<name>", Description: "switch"})
	registry.Register(SlashCommand{Name: "help", Description: "again"})

	items := registry.Items("mo")
	if len(items) != 1 || items[0].Name != "/model" || items[0].Description != "<name> switch" {
		t.Fatalf("Items = %+v", items)
	}
	diagnostics := registry.ConflictDiagnostics()
	if len(diagnostics) != 1 || diagnostics[0] != "/help registered 2 times" {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
}
