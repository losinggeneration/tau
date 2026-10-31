package codemode

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"

	"github.com/losinggeneration/tau/agent"
)

func TestCatalogDescriptionGroupsAndOmitsDeferredTools(t *testing.T) {
	tools := []agent.Tool{
		{Name: "z_read", Description: "Read z.", Namespace: &agent.ToolNamespace{Name: "z", Description: "Z namespace"}},
		{Name: "echo-text", Description: "Echo.", Parameters: map[string]any{"type": "object"}},
		{Name: "a_read", Description: "Read a.", Namespace: &agent.ToolNamespace{Name: "a"}},
		{Name: "deferred", Exposure: agent.ToolDeferred, Namespace: &agent.ToolNamespace{Name: "secret"}},
		{Name: ToolName},
	}
	text := CatalogDescription(tools, -1)
	assert.Contains(t, text, "### `echo_text` (`echo-text`)\nEcho.")
	assert.Contains(t, text, "Promise<string>")
	assert.Contains(t, text, "## z\nZ namespace")
	assert.Less(t, strings.Index(text, "### `echo_text`"), strings.Index(text, "## a"))
	assert.Less(t, strings.Index(text, "## a"), strings.Index(text, "## z"))
	assert.NotContains(t, text, "secret")
	assert.NotContains(t, text, "### `codemode`")
	assert.Equal(t, Description, CatalogDescription(nil, 3000))
	assert.Equal(t, Description, CatalogDescription([]agent.Tool{{Name: "late", Exposure: agent.ToolDeferred}}, 3000))
	assert.Equal(t, Description, CatalogDescription([]agent.Tool{{Name: "local"}}, 0))
}

func TestCatalogDescriptionSharesBudgetBetweenNamespaces(t *testing.T) {
	tools := []agent.Tool{
		{Name: "long", Description: strings.Repeat("long ", 100), Namespace: &agent.ToolNamespace{Name: "a"}},
		{Name: "one", Description: "Short.", Namespace: &agent.ToolNamespace{Name: "a"}},
		{Name: "two", Description: "Short.", Namespace: &agent.ToolNamespace{Name: "b"}},
	}
	cost := func(name string) int {
		section := "### `" + name + "`\n" + ToolSample(name, "Short.", nil, textOutputSchema)
		return (utf8.RuneCountInString(section) + 3) / 4
	}
	text := CatalogDescription(tools, cost("one")+cost("two"))
	assert.Contains(t, text, "### `one`")
	assert.Contains(t, text, "### `two`")
	assert.NotContains(t, text, "### `long`")
	assert.Contains(t, text, "## a (some tools not listed)")
	text = CatalogDescription(tools, 0)
	assert.Contains(t, text, "## a (tools not listed)")
	assert.Contains(t, text, "## b (tools not listed)")
	assert.NotContains(t, text, "### `")
}

func TestPrepareLoadoutModes(t *testing.T) {
	direct := agent.Tool{Name: "read-file", Description: " Read. "}
	indirect := agent.Tool{Name: "batch", Exposure: agent.ToolCodemode}
	deferred := agent.Tool{Name: "find", Exposure: agent.ToolDeferred}
	loadout := agent.ToolLoadout{Declared: []agent.Tool{direct, deferred}, Callable: []agent.Tool{direct, indirect, deferred}}
	on := PrepareLoadout(loadout, "on", 3000)
	assert.Equal(t, "Read.\n\nCodemode: `tools.read_file(args)` resolves to a string.", on.Descriptions[direct.Name])
	assert.Contains(t, on.Descriptions[deferred.Name], "Codemode:")
	assert.Contains(t, on.Descriptions[ToolName], "### `batch`")
	assert.NotContains(t, on.Descriptions[ToolName], "### `read_file`")
	assert.NotContains(t, on.Descriptions[ToolName], "### `find`")
	assert.Empty(t, on.HiddenDeclarations)
	only := PrepareLoadout(loadout, "only", 3000)
	assert.Equal(t, []string{"read-file"}, only.HiddenDeclarations)
	assert.Contains(t, only.Descriptions[ToolName], "### `read_file`")
	assert.NotContains(t, only.Descriptions[ToolName], "### `find`")
	assert.NotContains(t, only.Descriptions, "read-file")
	assert.Equal(t, Description, PrepareLoadout(loadout, "only", 0).Descriptions[ToolName])
}

func TestDescribeScriptOutput(t *testing.T) {
	for _, tc := range []struct {
		schema map[string]any
		want   string
	}{
		{nil, "a string"},
		{map[string]any{"type": "string"}, "a string"},
		{map[string]any{"type": "array", "items": map[string]any{"type": "number"}}, "`Array<number>`"},
		{map[string]any{"type": "object", "properties": map[string]any{"output": map[string]any{"type": "string"}, "info": map[string]any{"type": "object", "properties": map[string]any{"results": map[string]any{"type": "array"}}}}, "required": []string{"output"}}, "`{ info?: { results? }, output }`"},
	} {
		assert.Equal(t, tc.want, describeOutput(tc.schema))
	}
}
