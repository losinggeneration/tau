package mcp

import (
	"errors"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/losinggeneration/tau/ai"
)

func stubSave(t *testing.T, err error) *[]string {
	t.Helper()
	var saved []string
	old := saveOutput
	saveOutput = func(data []byte, ext string) (string, error) {
		saved = append(saved, ext+":"+string(data[:min(len(data), 8)]))
		return "/tmp/out" + ext, err
	}
	t.Cleanup(func() { saveOutput = old })
	return &saved
}

func TestConvertResultMapsEveryContentKind(t *testing.T) {
	saved := stubSave(t, nil)
	size := int64(2048)
	result, err := convertResult("s", "t", &sdkmcp.CallToolResult{Content: []sdkmcp.Content{
		&sdkmcp.TextContent{Text: "a"},
		&sdkmcp.ImageContent{Data: []byte("png"), MIMEType: "image/png"},
		&sdkmcp.AudioContent{Data: []byte("x"), MIMEType: "audio/wav"},
		&sdkmcp.ResourceLink{URI: "file:///r.txt", Name: "r", MIMEType: "text/plain", Size: &size, Description: "a file"},
		&sdkmcp.EmbeddedResource{Resource: &sdkmcp.ResourceContents{URI: "file:///e", Text: "embedded"}},
		&sdkmcp.EmbeddedResource{Resource: &sdkmcp.ResourceContents{URI: "file:///i", MIMEType: "image/gif", Blob: []byte("gif")}},
		&sdkmcp.EmbeddedResource{Resource: &sdkmcp.ResourceContents{URI: "file:///j", MIMEType: "application/json; charset=utf-8", Blob: []byte(`{}`)}},
		&sdkmcp.EmbeddedResource{Resource: &sdkmcp.ResourceContents{URI: "https://x/data.tar.gz?v=1", MIMEType: "application/gzip", Blob: []byte("binary!!")}},
	}}, true)
	require.NoError(t, err)
	var texts []string
	var images []string
	for _, b := range result.ContentBlocks {
		if b.Type == ai.ContentBlockImage {
			images = append(images, b.MimeType+":"+b.Data)
		} else {
			texts = append(texts, b.Text)
		}
	}
	assert.Equal(t, []string{
		"a",
		"[audio audio/wav omitted]",
		`[Resource file:///r.txt "r" (text/plain, 2.0KB): a file. Read it with read_mcp_resource (server "s")]`,
		"embedded",
		"{}",
		"[Binary resource https://x/data.tar.gz?v=1 (application/gzip, 8B) saved to /tmp/out.gz]",
	}, texts)
	assert.Equal(t, []string{"image/png:cG5n", "image/gif:Z2lm"}, images)
	assert.Equal(t, []string{".gz:binary!!"}, *saved)
	assert.False(t, result.IsError)
}

func TestConvertResultCutsLongTextInTheMiddle(t *testing.T) {
	saved := stubSave(t, nil)
	long := strings.Repeat("é", maxOutputBytes) // two bytes each
	result, err := convertResult("s", "t", &sdkmcp.CallToolResult{Content: []sdkmcp.Content{
		&sdkmcp.TextContent{Text: long},
		&sdkmcp.ImageContent{Data: []byte("p"), MIMEType: "image/png"},
	}}, false)
	require.NoError(t, err)
	require.Len(t, result.ContentBlocks, 2, "images follow the cut text")
	head := result.ContentBlocks[0].Text
	assert.True(t, strings.HasPrefix(head, "Warning: truncated output (original token count: 10240)\nTotal output lines: 1\n\n"))
	assert.Contains(t, head, "…10240 chars truncated…")
	assert.True(t, strings.HasSuffix(head, "\n\n[Full output: /tmp/out.txt (read it with offset/limit)]"))
	assert.Equal(t, Details{Server: "s", Tool: "t", FullOutputPath: "/tmp/out.txt"}, result.Details)
	assert.Len(t, *saved, 1)
	assert.Contains(t, string(result.StructuredContent), long[:100], "scripts get the whole result")

	stubSave(t, errors.New("disk full"))
	result, err = convertResult("s", "t", &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: long}}}, false)
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(result.ContentBlocks[0].Text, "[Could not save the full output: disk full]"))
	assert.Equal(t, Details{Server: "s", Tool: "t"}, result.Details)
}

func TestConvertResultAcceptsAnyStructuredValue(t *testing.T) {
	for _, value := range []any{nil, []any{1}, "text", 3.5, false} {
		result, err := convertResult("s", "t", &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "x"}}, StructuredContent: value}, false)
		require.NoError(t, err)
		assert.Equal(t, "x", result.Content)
	}
	result, err := convertResult("s", "t", &sdkmcp.CallToolResult{StructuredContent: []any{"a"}}, false)
	require.NoError(t, err)
	assert.Equal(t, "[\n  \"a\"\n]", result.Content)
}
