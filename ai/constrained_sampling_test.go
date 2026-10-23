package ai

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestStrictToolParameters(t *testing.T) {
	for _, test := range []struct {
		name, schema string
		invalid      bool
	}{
		{name: "optional", schema: `{"type":"object","properties":{"q":{"type":"string"}}}`},
		{name: "required", schema: `{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`},
		{name: "nested", schema: `{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{"x":{"type":"integer"}}}}}}`},
		{name: "nullable", schema: `{"type":"object","properties":{"q":{"type":["string","null"]}}}`},
		{name: "reference", schema: `{"type":"object","$ref":"#/$defs/x"}`, invalid: true},
		{name: "bounds", schema: `{"type":"object","properties":{"n":{"type":"number","minimum":0}}}`, invalid: true},
		{name: "additional", schema: `{"type":"object","additionalProperties":true}`, invalid: true},
		{name: "boolean", schema: `{"type":"object","properties":{"x":true}}`, invalid: true},
		{name: "unknown required", schema: `{"type":"object","required":["missing"]}`, invalid: true},
		{name: "object union", schema: `{"type":"object","properties":{"q":{"anyOf":[{"type":"object"},{"type":"null"}]}}}`, invalid: true},
		{name: "tuple", schema: `{"type":"object","properties":{"q":{"type":"array","items":[{"type":"string"}]}}}`, invalid: true},
		{name: "bad format", schema: `{"type":"object","properties":{"q":{"type":"string","format":"password"}}}`, invalid: true},
		{name: "good format", schema: `{"type":"object","properties":{"q":{"type":"string","format":"email"}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, mode := range []string{"prefer", "require"} {
				tool := Tool{Name: "lookup", Parameters: json.RawMessage(test.schema), ConstrainedSampling: json.RawMessage(`{"type":"json_schema","strict":"` + mode + `"}`)}
				parameters, strict, err := strictToolParameters(tool, true, anthropicUnsupportedStrictKeyword)
				if test.invalid {
					if mode == "require" {
						if err == nil {
							t.Fatal("required unsupported schema accepted")
						}
						continue
					}
					if err != nil || strict || string(parameters) != test.schema {
						t.Fatalf("fallback parameters=%s strict=%t err=%v", parameters, strict, err)
					}
				} else {
					if err != nil || !strict {
						t.Fatalf("strict=%t err=%v", strict, err)
					}
					var schema map[string]any
					if err := json.Unmarshal(parameters, &schema); err != nil {
						t.Fatal(err)
					}
					if schema["additionalProperties"] != false {
						t.Fatalf("schema=%s", parameters)
					}
				}
				if string(tool.Parameters) != test.schema {
					t.Fatal("input mutated")
				}
			}
		})
	}
}

func TestStrictToolOptionalPropertiesBecomeNullable(t *testing.T) {
	tool := Tool{Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"},"n":{"type":["number","null"]}}}`), ConstrainedSampling: json.RawMessage(`{"type":"json_schema","strict":"require"}`)}
	got, strict, err := strictToolParameters(tool, true, nil)
	if err != nil || !strict {
		t.Fatalf("strict=%t err=%v", strict, err)
	}
	var actual, expected any
	if err := json.Unmarshal(got, &actual); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"q":{"anyOf":[{"type":"string"},{"type":"null"}]},"n":{"type":["number","null"]}},"required":["n","q"],"additionalProperties":false}`), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("schema=%s", got)
	}
}

func TestGrammarToolsResolveLikeUpstream(t *testing.T) {
	params := json.RawMessage(`{"type":"object","properties":{"source":{"type":"string"}},"required":["source"]}`)
	newTool := func(variants string, parameters json.RawMessage) Tool {
		return Tool{Name: "code", Parameters: parameters, ConstrainedSampling: json.RawMessage(`{"type":"grammar","variants":` + variants + `}`)}
	}
	if grammar, err := resolveGrammarTool(newTool(`{"openai_lark":"start: x"}`, params), false); grammar != nil || err != nil {
		t.Fatalf("unsupported provider = %v, %v; want plain JSON tool", grammar, err)
	}
	grammar, err := resolveGrammarTool(newTool(`{"openai_lark":"start: x","openai_regex":"x+"}`, params), true)
	if err != nil || *grammar != (grammarTool{syntax: "lark", definition: "start: x", property: "source"}) {
		t.Fatalf("lark grammar = %+v, %v", grammar, err)
	}
	grammar, err = resolveGrammarTool(newTool(`{"openai_lark":"  ","openai_regex":"x+"}`, params), true)
	if err != nil || grammar.syntax != "regex" || grammar.definition != "x+" {
		t.Fatalf("regex grammar = %+v, %v", grammar, err)
	}
	for name, test := range map[string]struct {
		variants   string
		parameters string
		message    string
	}{
		"no variant":    {`{}`, string(params), "no supported grammar variant"},
		"not object":    {`{"openai_regex":"x"}`, `{"type":"string"}`, "requires an object parameter schema"},
		"two required":  {`{"openai_regex":"x"}`, `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a","b"]}`, "exactly one required string property"},
		"missing entry": {`{"openai_regex":"x"}`, `{"type":"object","required":["a"]}`, "requires a properties entry for a"},
		"not string":    {`{"openai_regex":"x"}`, `{"type":"object","properties":{"a":{"type":"number"}},"required":["a"]}`, "property a must have type string"},
	} {
		if _, err := resolveGrammarTool(newTool(test.variants, json.RawMessage(test.parameters)), true); err == nil || !strings.Contains(err.Error(), test.message) {
			t.Errorf("%s: err = %v, want %q", name, err, test.message)
		}
	}
	if _, err := grammarToolInput("code", json.RawMessage(`{"source":1}`), "source"); err == nil {
		t.Fatal("non-string grammar input accepted")
	}
}

func TestGrammarInputBufferStreamsJSONArguments(t *testing.T) {
	buffer := newGrammarInputBuffer("source")
	var deltas strings.Builder
	for _, next := range []string{"print(", `print("hi")`} {
		delta, err := buffer.append(next, false)
		if err != nil {
			t.Fatal(err)
		}
		deltas.WriteString(delta)
	}
	if delta, err := buffer.append(`print("hi")`, false); err != nil || delta != "" {
		t.Fatalf("unchanged input delta = %q, %v", delta, err)
	}
	deltas.WriteString(buffer.finish())
	if got := deltas.String(); got != `{"source":"print(\"hi\")"}` {
		t.Fatalf("deltas = %s", got)
	}
	if string(buffer.arguments()) != deltas.String() {
		t.Fatalf("arguments = %s", buffer.arguments())
	}
	if delta, err := buffer.append(`print("hi")`, true); err != nil || delta != "" {
		t.Fatalf("repeated close = %q, %v", delta, err)
	}
	if _, err := buffer.append(`print("bye")`, true); err == nil {
		t.Fatal("input changed after close accepted")
	}
	if _, err := newGrammarInputBuffer("source").append("", true); err != nil {
		t.Fatal(err)
	}
	shrinking := newGrammarInputBuffer("source")
	if _, err := shrinking.append("abc", false); err != nil {
		t.Fatal(err)
	}
	if _, err := shrinking.append("ab", false); err == nil {
		t.Fatal("non-monotonic input accepted")
	}
}
