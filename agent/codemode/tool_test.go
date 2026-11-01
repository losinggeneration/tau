package codemode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/losinggeneration/tau/agent"
	"github.com/losinggeneration/tau/ai"
)

func runCodemode(t *testing.T, code string, tools ...agent.Tool) *agent.ToolResult {
	t.Helper()
	return runCodemodeWithStore(t, nil, code, tools...)
}

func runCodemodeWithStore(t *testing.T, store *Store, code string, tools ...agent.Tool) *agent.ToolResult {
	t.Helper()
	args, err := json.Marshal(map[string]string{"code": code})
	require.NoError(t, err)
	current := &agent.AgentContext{Tools: append([]agent.Tool{NewTool(store)}, tools...)}
	return agent.RunToolCall(t.Context(), current, nil, ai.ToolCall{ID: "cm", Name: ToolName, Arguments: args}, agent.AgentLoopConfig{})
}

func resultBody(t *testing.T, res *agent.ToolResult) (header, body string) {
	t.Helper()
	blocks := res.NormalizedContent()
	require.NotEmpty(t, blocks)
	var parts []string
	for _, b := range blocks[1:] {
		if b.Type == ai.ContentBlockText {
			parts = append(parts, b.Text)
		}
	}
	return blocks[0].Text, strings.Join(parts, "|")
}

func TestCodemodeToolCallsNestedTools(t *testing.T) {
	object := map[string]any{"type": "object"}
	lookup := agent.Tool{
		Name: "lookup", Parameters: object, OutputSchema: object,
		Execute: func(agent.ToolContext) (*agent.ToolResult, error) {
			return &agent.ToolResult{IsError: true, Content: "miss", StructuredContent: json.RawMessage(`{"found":false}`)}, nil
		},
	}
	echo := agent.Tool{
		Name: "echo-text", Parameters: object,
		Execute: func(tc agent.ToolContext) (*agent.ToolResult, error) {
			return &agent.ToolResult{Content: "said " + tc.Args["word"].(string)}, nil
		},
	}
	broken := agent.Tool{
		Name: "broken", Parameters: object,
		Execute: func(agent.ToolContext) (*agent.ToolResult, error) {
			return &agent.ToolResult{IsError: true, Content: "exit 2"}, nil
		},
	}
	res := runCodemode(t, `
const found = await tools.lookup({});
text(JSON.stringify(found));
text(await tools.echo_text({ word: "hi" }));
console.log("log line");
try { await tools.broken({}) } catch (e) { text("caught " + e.message) }
return 42;`, lookup, echo, broken)

	header, body := resultBody(t, res)
	assert.False(t, res.IsError, body)
	assert.True(t, strings.HasPrefix(header, "Script completed\nWall time "), header)
	assert.True(t, strings.HasSuffix(header, " seconds\nOutput:\n"), header)
	assert.Equal(t, "==> text 1/4 <==\n{\"found\":false}\n==> text 2/4 <==\nsaid hi\n==> text 3/4 <==\ncaught exit 2\n==> text 4/4 <==\n42\n<console_output>\nlog line\n</console_output>", body)
	require.Len(t, res.NestedCalls, 3)
	assert.Equal(t, []string{"error", "ok", "error"}, []string{res.NestedCalls[0].Status, res.NestedCalls[1].Status, res.NestedCalls[2].Status})
}

func TestCodemodeToolReportsFailuresWithPartialOutput(t *testing.T) {
	object := map[string]any{"type": "object"}
	echo := agent.Tool{Name: "echo", Parameters: object, Execute: func(agent.ToolContext) (*agent.ToolResult, error) {
		return &agent.ToolResult{Content: "ok"}, nil
	}}
	res := runCodemode(t, "text('partial');\nawait tools.echo({});\nnull.x", echo)
	header, body := resultBody(t, res)
	assert.True(t, res.IsError)
	assert.True(t, strings.HasPrefix(header, "Script failed\n"), header)
	assert.True(t, strings.HasPrefix(body, "partial\nScript error:\nTypeError"), body)
	assert.True(t, strings.HasSuffix(body, "Tool calls made before the failure (they are not undone): echo (ok)"), body)

	res = runCodemode(t, "// @options: {\"timeout_ms\": 20}\nwhile (true) {}")
	_, body = resultBody(t, res)
	assert.True(t, res.IsError)
	assert.Contains(t, body, "Script error:\nScript timed out: ")
	assert.True(t, strings.HasSuffix(body, "No tool calls were made."), body)

	res = runCodemode(t, "// @options: {\"bogus\": 1}\ntext(1)")
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content, "@options only supports")
}

func TestCodemodeToolCancelsUnawaitedNestedCalls(t *testing.T) {
	object := map[string]any{"type": "object"}
	started := make(chan struct{})
	wait := agent.Tool{Name: "wait", Parameters: object, Execute: func(tc agent.ToolContext) (*agent.ToolResult, error) {
		close(started)
		<-tc.Context.Done()
		return nil, tc.Context.Err()
	}}
	ping := agent.Tool{Name: "ping", Parameters: object, Execute: func(agent.ToolContext) (*agent.ToolResult, error) {
		<-started
		return &agent.ToolResult{Content: "pong"}, nil
	}}
	res := runCodemode(t, "tools.wait({}); return await tools.ping({})", wait, ping)
	_, body := resultBody(t, res)
	assert.False(t, res.IsError, body)
	assert.Equal(t, "pong", body)
	statuses := map[string]string{}
	for _, call := range res.NestedCalls {
		statuses[call.Name] = call.Status
	}
	assert.Equal(t, map[string]string{"wait": "cancelled", "ping": "ok"}, statuses)
}

func TestCodemodeToolTruncatesAndSavesImages(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	res := runCodemode(t, "// @options: {\"max_output_tokens\": 2}\ntext('abcdefghijklmnop')")
	_, body := resultBody(t, res)
	assert.True(t, strings.HasPrefix(body, "Warning: truncated output (original token count: 4)\nTotal output lines: 1\n\nabcd…2 tokens truncated…mnop\n\n[Full output: "), body)
	path := strings.TrimSuffix(body[strings.Index(body, "[Full output: ")+len("[Full output: "):], " (read with offset/limit)]")
	full, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "abcdefghijklmnop", string(full))

	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n0000"))
	res = runCodemode(t, "image('data:image/png;base64,"+png+"'); image('data:image/png;base64,"+png+"')")
	blocks := res.NormalizedContent()
	require.Len(t, blocks, 5)
	assert.Equal(t, ai.ContentBlockImage, blocks[2].Type)
	assert.Equal(t, blocks[1].Text, blocks[3].Text, "an image shown twice is saved once")
	assert.Regexp(t, `^\[Image saved to .+\.png \(image/png, 12B\)\]$`, blocks[1].Text)
}

func TestCodemodeToolDiscoveryGlobals(t *testing.T) {
	object := map[string]any{"type": "object"}
	docs := &agent.ToolNamespace{Name: "mcp__docs", Description: "Docs server", Instructions: "Search first"}
	search := agent.Tool{
		Name: "mcp__docs__search", Description: "Search the documentation pages", Namespace: docs, Exposure: agent.ToolDeferred, Parameters: object,
		Execute: func(agent.ToolContext) (*agent.ToolResult, error) { return &agent.ToolResult{Content: "found"}, nil },
	}
	weather := agent.Tool{
		Name: "weather", Description: "Current weather forecast", Parameters: object,
		Execute: func(agent.ToolContext) (*agent.ToolResult, error) { return &agent.ToolResult{Content: "sunny"}, nil },
	}
	res := runCodemode(t, `
const hits = await searchTools("documentation pages");
text(JSON.stringify(hits.map((h) => h.name)));
text(String(hits[0].description.includes("Search the documentation pages")));
text(JSON.stringify((await searchTools("weather", { namespace: "mcp__docs" })).length));
text(String((await describeTool("weather")).includes("Current weather forecast")));
text(String(await describeTool("missing")));
text(JSON.stringify(await describeNamespace("mcp__docs")));
text(String(await describeNamespace("nope")));
try { await searchTools(1) } catch (e) { text(e.message) }
try { await searchTools("x", { limit: 1.5 }) } catch (e) { text(e.message) }
return await tools.mcp__docs__search({});`, search, weather)
	_, body := resultBody(t, res)
	assert.False(t, res.IsError, body)
	parts := strings.Split(body, "\n")
	var texts []string
	for _, line := range parts {
		if !strings.HasPrefix(line, "==> text ") {
			texts = append(texts, line)
		}
	}
	assert.Equal(t, []string{
		`["mcp__docs__search"]`,
		"true",
		"0",
		"true",
		"undefined",
		`{"name":"mcp__docs","description":"Docs server","instructions":"Search first","tools":["mcp__docs__search"]}`,
		"undefined",
		"searchTools() expects a query string",
		"searchTools() limit must be a positive integer",
		"found",
	}, texts)
}

func TestCodemodeToolPersistsStoreWritesOfSuccessfulScripts(t *testing.T) {
	values := map[string]json.RawMessage{}
	var saved []string
	store := &Store{
		Load: func(context.Context) (map[string]json.RawMessage, error) { return maps.Clone(values), nil },
		Save: func(_ context.Context, writes StoreWrites) error {
			data, err := json.Marshal(writes)
			if err != nil {
				return err
			}
			saved = append(saved, string(data))
			return ApplyStoreWrites(values, data)
		},
	}
	res := runCodemodeWithStore(t, store, `store("a", {n: 1}); store("b", 2)`)
	require.False(t, res.IsError)
	res = runCodemodeWithStore(t, store, `store("a", undefined); text(JSON.stringify([load("a"), load("b")]))`)
	_, body := resultBody(t, res)
	assert.Equal(t, `[null,2]`, body)
	res = runCodemodeWithStore(t, store, `store("b", 3); throw new Error("no")`)
	require.True(t, res.IsError)
	res = runCodemodeWithStore(t, store, `text("x")`)
	require.False(t, res.IsError)
	assert.Equal(t, []string{`{"set":{"a":{"n":1},"b":2},"delete":[]}`, `{"set":{},"delete":["a"]}`}, saved)
	assert.Equal(t, map[string]json.RawMessage{"b": json.RawMessage(`2`)}, values)
}

func TestCodemodeReadsStructuredBuiltinImages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image.png")
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a/0YAAAAASUVORK5CYII=")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	code := fmt.Sprintf("const value = await tools.read({path: %q}); text(value.type); text(value.mimeType); image(value);", path)
	args, err := json.Marshal(map[string]string{"code": code})
	require.NoError(t, err)
	current := &agent.AgentContext{Tools: []agent.Tool{NewTool(nil), agent.ReadTool(agent.ToolConfig{AutoResizeImagesSet: true})}, Capabilities: ai.Capabilities{Vision: true}}
	result := agent.RunToolCall(t.Context(), current, nil, ai.ToolCall{ID: "read-image", Name: ToolName, Arguments: args}, agent.AgentLoopConfig{})
	assert.False(t, result.IsError, result.Content)
	blocks := result.NormalizedContent()
	require.Len(t, blocks, 3)
	assert.Contains(t, blocks[1].Text, "image")
	assert.Contains(t, blocks[1].Text, "image/png")
	assert.Equal(t, ai.ContentBlockImage, blocks[2].Type)
	assert.Equal(t, base64.StdEncoding.EncodeToString(data), blocks[2].Data)
	require.Len(t, result.NestedCalls, 1)
	assert.Equal(t, "ok", result.NestedCalls[0].Status)
}
