package codemode

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func schema(t *testing.T, text string) any {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal([]byte(text), &v))
	return v
}

func TestSchemaToType(t *testing.T) {
	cases := []struct{ schema, want string }{
		{`{"type":"string"}`, "string"},
		{`{"type":"integer"}`, "number"},
		{`{"type":["string","null"]}`, "string | null"},
		{`{"const":"a"}`, `"a"`},
		{`{"enum":["a",1,null]}`, `"a" | 1 | null`},
		{`{"anyOf":[{"type":"string"},{"type":"number"}]}`, "string | number"},
		{`{"anyOf":[{"type":"string"},{}]}`, "unknown"},
		{`{"allOf":[{"anyOf":[{"type":"string"},{"type":"number"}]},{"const":1}]}`, "(string | number) & 1"},
		{`{"$ref":"#/defs/x"}`, "unknown"},
		{`true`, "unknown"},
		{`false`, "never"},
		{`{"type":"object","properties":{"city":{"type":"string"},"max-lines":{"type":"number"}},"required":["city"],"additionalProperties":false}`, `{ city: string; "max-lines"?: number; }`},
		{`{"type":"object","additionalProperties":{"type":"number"}}`, "{ [key: string]: number; }"},
		{`{"type":"object"}`, "{ [key: string]: unknown; }"},
		{`{"type":"object","properties":{},"additionalProperties":false}`, "{}"},
		{`{"type":"object","properties":{"weather":{"type":"array","description":"look up weather","items":{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}}},"required":["weather"]}`, "{\n  // look up weather\n  weather: Array<{ location: string; }>;\n}"},
		{`{"type":"object","properties":{"outer":{"type":"object","description":"Outer","properties":{"inner":{"type":"string","description":"Inner"}}}}}`, "{\n  // Outer\n  outer?: {\n    // Inner\n    inner?: string;\n  };\n}"},
		{
			`{"type":"object","properties":{"timeoutMs":{"type":"integer","minimum":0,"maximum":30000,"default":0,"description":"Wait in milliseconds."},"id":{"type":"string","pattern":"^[a-z]+$","maxLength":8},"tags":{"type":"array","items":{"type":"string"},"maxItems":5,"uniqueItems":true,"deprecated":true},"name":{"type":"string"}},"required":["id"]}`,
			"{\n  // @maxLength 8 @pattern ^[a-z]+$\n  id: string;\n  name?: string;\n  // @maxItems 5 @uniqueItems @deprecated\n  tags?: Array<string>;\n  // Wait in milliseconds.\n  // @integer @minimum 0 @maximum 30000 @default 0\n  timeoutMs?: number;\n}",
		},
		{`{"type":"object","properties":{"limit":{"anyOf":[{"type":"integer","minimum":1,"maximum":100},{"type":"null"}],"default":null}}}`, "{\n  // @integer @minimum 1 @maximum 100\n  limit?: number | null;\n}"},
		{`{"type":"object","properties":{"n":{"anyOf":[{"type":"integer","minimum":10,"maximum":50,"format":"int32"},{"type":"null"}],"minimum":0,"maximum":20,"format":"port"}}}`, "{\n  // @integer @minimum 10 @maximum 20 @format port @format int32\n  n?: number | null;\n}"},
		{`{"type":"object","properties":{"n":{"type":"integer","minimum":0,"exclusiveMinimum":true,"maximum":10,"exclusiveMaximum":false},"m":{"anyOf":[{"type":"number","maximum":5,"exclusiveMaximum":true},{"type":"null"}],"maximum":8}}}`, "{\n  // @maximum 8 @exclusiveMaximum 5\n  m?: number | null;\n  // @integer @exclusiveMinimum 0 @maximum 10\n  n?: number;\n}"},
		{`{"type":"object","properties":{"config":{"type":"object","default":{"text":"` + strings.Repeat("x", 100) + `"}}}}`, "{ config?: { [key: string]: unknown; }; }"},
		{`{"type":"object","properties":{"sep":{"type":"string","pattern":"^[\r\n\u2028\u2029]$","default":"\u2028","description":"a\u2028b"}}}`, "{\n  // a\n  // b\n  // @pattern ^[\\u000d\\u000a\\u2028\\u2029]$ @default \"\\u2028\"\n  sep?: string;\n}"},
		{`{"type":"object","properties":{"item":{"$ref":"#/$defs/Item"},"legacy":{"$ref":"#/definitions/Legacy"},"remote":{"$ref":"https://example.com/schema.json"}},"required":["item"],"$defs":{"Item":{"type":"object","properties":{"id":{"type":"string"},"parent":{"$ref":"#/$defs/Item"}},"required":["id"]}},"definitions":{"Legacy":{"enum":["a","b"]}}}`, `{ item: { id: string; parent?: unknown; }; legacy?: "a" | "b"; remote?: unknown; }`},
		{`{"type":"array","items":{"type":"string"}}`, "Array<string>"},
		{`{"type":"array","prefixItems":[{"type":"string"},{"type":"number"}]}`, "[string, number]"},
		{`{"type":"array"}`, "unknown[]"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, SchemaToType(schema(t, c.schema), 0), c.schema)
	}
}

func TestSchemaToTypeBudgetDropsTagsThenGivesUp(t *testing.T) {
	s := schema(t, `{"type":"object","properties":{"n":{"type":"integer","minimum":1}}}`)
	assert.Equal(t, "{\n  // @integer @minimum 1\n  n?: number;\n}", SchemaToType(s, 0))
	assert.Equal(t, "{ n?: number; }", SchemaToType(s, 20))
	assert.Equal(t, "unknown", SchemaToType(s, 10))
}

func TestSchemaToTypeAcceptsGoBuiltSchemas(t *testing.T) {
	s := map[string]any{
		"type":       "object",
		"properties": map[string]any{"n": map[string]any{"type": "integer", "minimum": 1}},
		"required":   []string{"n"},
	}
	assert.Equal(t, "{\n  // @integer @minimum 1\n  n: number;\n}", SchemaToType(s, 0))
}

func TestToolDeclarations(t *testing.T) {
	assert.Equal(t, "hidden_dynamic_tool(args: { city: string; }): Promise<{ ok: boolean; }>;", ToolSignature("hidden-dynamic-tool",
		schema(t, `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`),
		schema(t, `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)))
	assert.Equal(t, "free(args: unknown): Promise<unknown>;", ToolSignature("free", nil, nil))
	assert.Equal(t, "plain(args: { [key: string]: unknown; }): Promise<{ _meta?: { [key: string]: unknown; }; content: Array<{ [key: string]: unknown; }>; isError?: boolean; }>;",
		ToolSignature("plain", schema(t, `{"type":"object","properties":{}}`),
			schema(t, `{"type":"object","properties":{"content":{"type":"array","items":{"type":"object"}},"isError":{"type":"boolean"},"_meta":{"type":"object"}},"required":["content"]}`)))
	assert.Equal(t, "bar\n\ncodemode tool declaration:\n```ts\ndeclare const tools: { foo(args: string): Promise<unknown>; };\n```",
		ToolSample("foo", "bar", schema(t, `{"type":"string"}`), nil))
}
