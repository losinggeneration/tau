package ai

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestGoogleStrictToolsMatchUpstream(t *testing.T) {
	tool := func(strict string) Tool {
		return Tool{Name: "lookup", Description: "find", Parameters: json.RawMessage(`{"$schema":"x","type":"object","properties":{"q":{"type":"string"}}}`), ConstrainedSampling: json.RawMessage(`{"type":"json_schema","strict":"` + strict + `"}`)}
	}
	gemini3 := &Model{ID: "gemini-3-pro"}
	declarations, strict, err := googleFunctionDeclarations(gemini3, []Tool{tool("prefer")}, googleJSONSchemaParameters)
	if err != nil || !strict {
		t.Fatalf("gemini 3 preferred = %v, %v", strict, err)
	}
	schema := declarations[0]["parametersJsonSchema"].(map[string]any)
	if schema["additionalProperties"] != false || !reflect.DeepEqual(schema["required"], []any{"q"}) {
		t.Fatalf("strict schema = %v", schema)
	}

	gemini25 := &Model{ID: "gemini-2.5-flash"}
	declarations, strict, err = googleFunctionDeclarations(gemini25, []Tool{tool("prefer")}, googleJSONSchemaParameters)
	if err != nil || strict || declarations[0]["parametersJsonSchema"].(map[string]any)["additionalProperties"] != nil {
		t.Fatalf("gemini 2.5 preferred = %v, %v, %v", declarations, strict, err)
	}
	if _, _, err := googleFunctionDeclarations(gemini25, []Tool{tool("require")}, googleJSONSchemaParameters); err == nil {
		t.Fatal("required strictness accepted before Gemini 3")
	}

	declarations, _, err = googleFunctionDeclarations(gemini25, []Tool{tool("prefer")}, googleLegacyParameters)
	if err != nil {
		t.Fatal(err)
	}
	if legacy := declarations[0]["parameters"].(map[string]any); legacy["$schema"] != nil || legacy["type"] != "object" {
		t.Fatalf("legacy parameters = %v", legacy)
	}

	for _, test := range []struct {
		choice any
		strict bool
		want   string
	}{
		{nil, false, ""},
		{nil, true, "VALIDATED"},
		{"auto", true, "VALIDATED"},
		{"auto", false, "AUTO"},
		{"none", true, "NONE"},
		{"any", true, "ANY"},
	} {
		if got := googleFunctionCallingMode(test.choice, test.strict); got != test.want {
			t.Errorf("mode(%v, %v) = %q, want %q", test.choice, test.strict, got, test.want)
		}
	}

	prompt := &Context{Tools: []Tool{tool("prefer")}, Messages: []Message{{Role: "user", Content: "hi"}}}
	body, err := buildVertexRequestBody(&Model{ID: "gemini-3-pro"}, prompt, &options{})
	if err != nil {
		t.Fatal(err)
	}
	if mode := body["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)["mode"]; mode != "VALIDATED" {
		t.Fatalf("vertex mode = %v", mode)
	}
	request, err := buildGeminiCLIRequest(&Model{ID: "gemini-3-pro"}, prompt, "project", false, &options{})
	if err != nil {
		t.Fatal(err)
	}
	if request.Request.ToolConfig == nil || request.Request.ToolConfig.FunctionCallingConfig.Mode != "VALIDATED" {
		t.Fatalf("gemini cli tool config = %+v", request.Request.ToolConfig)
	}
	encoded, err := json.Marshal(request.Request.Tools)
	if err != nil {
		t.Fatal(err)
	}
	var tools []map[string][]map[string]any
	if err := json.Unmarshal(encoded, &tools); err != nil {
		t.Fatal(err)
	}
	if tools[0]["functionDeclarations"][0]["parametersJsonSchema"] == nil {
		t.Fatalf("gemini cli dropped parameters: %s", encoded)
	}
}

func TestGoogleSharedThinkingBudgets(t *testing.T) {
	for _, tc := range []struct {
		id    string
		level ReasoningLevel
		want  int
	}{
		{"gemini-2.5-pro", ReasoningHigh, 3000},
		{"gemini-2.5-pro", ReasoningLow, 0},
		{"gemini-2.5-pro", ReasoningOff, 0},
		{"gemini-2.5-flash-lite", ReasoningMinimal, 512},
	} {
		t.Run(tc.id+string(tc.level), func(t *testing.T) {
			thinking, ok, err := resolveGoogleThinking(&Model{ID: tc.id, Reasoning: true}, applyOptions([]Option{WithReasoning(tc.level), WithThinkingBudgets(&ThinkingBudgets{Low: new(0), High: new(3000)})}))
			if err != nil || !ok || thinking.budget == nil || *thinking.budget != tc.want {
				t.Fatalf("thinking=%+v ok=%v err=%v want=%d", thinking, ok, err, tc.want)
			}
		})
	}
}

func TestSharedThinkingBudgetsDoNotReplaceGoogleLevels(t *testing.T) {
	model := &Model{ID: "gemini-3-flash", Reasoning: true}
	thinking, ok, err := resolveGoogleThinking(model, applyOptions([]Option{WithReasoning(ReasoningLow), WithThinkingBudgets(&ThinkingBudgets{Low: new(1234)})}))
	if err != nil || !ok || thinking.level != GoogleThinkingLevelLow || thinking.budget != nil {
		t.Fatalf("thinking=%+v ok=%v err=%v", thinking, ok, err)
	}
}
