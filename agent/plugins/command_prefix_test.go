package plugins

import (
	"context"
	"testing"

	"github.com/losinggeneration/tau/commands"
)

func TestRegisterPluginDoesNotApplyToolPrefixToCommands(t *testing.T) {
	registry := commands.NewInMemoryRegistry()
	mgr := NewManager(t.TempDir(), nil, registry)
	mgr.plugins["hello-world"] = &PluginInfo{Name: "hello-world", grpcClient: &mockPluginClient{}}

	mgr.registerPlugin(&PluginInfo{
		Name: "hello-world",
		Path: "/tmp/hello-world",
		Registrations: Registrations{
			Tools:      []ToolRegistration{{Name: "hello", Description: "tool"}},
			Commands:   []CommandRegistration{{Name: "hello", Description: "command"}},
			ToolPrefix: "hello_",
		},
	})

	if _, ok := mgr.toolHandlers["hello_hello"]; !ok {
		t.Fatal("expected tool handler to use tool prefix")
	}
	if _, ok := mgr.commandHandlers["hello"]; !ok {
		t.Fatal("expected command handler to be registered without tool prefix")
	}
	if _, ok := mgr.commandHandlers["hello_hello"]; ok {
		t.Fatal("did not expect command handler to use tool prefix")
	}
	if _, _, ok := registry.Lookup("hello"); !ok {
		t.Fatal("expected slash command registry to expose /hello")
	}
	if _, _, ok := registry.Lookup("hello_hello"); ok {
		t.Fatal("did not expect slash command registry to expose /hello_hello")
	}
}

func TestUnregisterPluginDoesNotApplyToolPrefixToCommands(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil, nil)
	info := &PluginInfo{
		Name: "hello-world",
		Registrations: Registrations{
			Tools:      []ToolRegistration{{Name: "hello"}},
			Commands:   []CommandRegistration{{Name: "hello"}},
			ToolPrefix: "hello_",
		},
	}
	mgr.toolHandlers["hello_hello"] = func(context.Context, string, string, string) (string, bool, error) { return "", false, nil }
	mgr.commandHandlers["hello"] = func(context.Context, string, string) (string, bool, error) { return "", false, nil }

	mgr.unregisterPluginLocked(info)

	if _, ok := mgr.toolHandlers["hello_hello"]; ok {
		t.Fatal("expected prefixed tool handler to be removed")
	}
	if _, ok := mgr.commandHandlers["hello"]; ok {
		t.Fatal("expected unprefixed command handler to be removed")
	}
}
