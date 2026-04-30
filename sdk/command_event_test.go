package sdk

import (
	"context"
	"testing"

	"github.com/losinggeneration/tau/commands"
)

func TestCommandResultEventUsesCommandNameAndType(t *testing.T) {
	reg := commands.NewInMemoryRegistry()
	if err := reg.Register(commands.Info{Name: "hello", Source: commands.Plugin}, func(context.Context, string, commands.CommandUI, commands.CommandSession) (commands.Result, error) {
		return commands.Result{Output: "hello output"}, nil
	}); err != nil {
		t.Fatalf("register command: %v", err)
	}

	s := &agentSession{id: "session-1", cmdRegistry: reg}
	events, err := s.executeSlashCommand(context.Background(), "/hello Ada")
	if err != nil {
		t.Fatalf("executeSlashCommand: %v", err)
	}

	var got Event
	for ev := range events {
		if ev.Type == "command_result" {
			got = ev
			break
		}
	}
	if got.Type == "" {
		t.Fatal("expected command_result event")
	}
	payload, ok := got.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map[string]any", got.Payload)
	}
	if payload["command"] != "hello" {
		t.Fatalf("command payload = %v, want hello", payload["command"])
	}
	if payload["output"] != "hello output" {
		t.Fatalf("output payload = %v, want hello output", payload["output"])
	}
	if payload["is_error"] != false {
		t.Fatalf("is_error payload = %v, want false", payload["is_error"])
	}
}
