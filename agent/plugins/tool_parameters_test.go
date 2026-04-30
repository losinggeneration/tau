package plugins

import (
	"context"
	"testing"
)

func TestToolsParsesJSONSchemaParameterValues(t *testing.T) {
	m := NewManager(t.TempDir(), nil, nil)
	m.plugins["hello-world"] = &PluginInfo{
		Name: "hello-world",
		Registrations: Registrations{
			Tools: []ToolRegistration{{
				Name:        "hello",
				Description: "greet",
				Parameters: map[string]string{
					"type":       "object",
					"properties": `{"name":{"type":"string","description":"Optional name to greet."}}`,
				},
			}},
		},
	}
	m.toolHandlers["hello"] = func(ctx context.Context, toolName, toolCallID, arguments string) (string, bool, error) {
		return "", false, nil
	}

	tools := m.Tools()
	if len(tools) != 1 {
		t.Fatalf("Tools() returned %d tools, want 1", len(tools))
	}
	if got := tools[0].Parameters["type"]; got != "object" {
		t.Fatalf("parameters.type = %#v, want object", got)
	}
	properties, ok := tools[0].Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("parameters.properties = %#v (%T), want object", tools[0].Parameters["properties"], tools[0].Parameters["properties"])
	}
	name, ok := properties["name"].(map[string]any)
	if !ok {
		t.Fatalf("parameters.properties.name = %#v, want object", properties["name"])
	}
	if got := name["type"]; got != "string" {
		t.Fatalf("parameters.properties.name.type = %#v, want string", got)
	}
}
