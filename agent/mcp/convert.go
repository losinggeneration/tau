package mcp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/losinggeneration/tau/agent"
	"github.com/losinggeneration/tau/ai"
)

// maxOutputBytes is where model-facing text gets cut in the middle.
const maxOutputBytes = 20 * 1024

// ReadResourceTool names the tool resource links point at.
const ReadResourceTool = "read_mcp_resource"

// Details is the ToolResult detail of an MCP call.
type Details struct {
	Server         string `json:"server"`
	Tool           string `json:"tool"`
	FullOutputPath string `json:"fullOutputPath,omitempty"`
}

// saveOutput writes a truncated text or binary resource to a temp file; tests
// replace it.
var saveOutput = func(data []byte, ext string) (string, error) {
	f, err := os.CreateTemp("", "tau-mcp-*"+ext)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		return "", fmt.Errorf("write %s: %w", f.Name(), err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close %s: %w", f.Name(), err)
	}
	return f.Name(), nil
}

func convertResult(server, tool string, result *sdkmcp.CallToolResult, readableResources bool) (*agent.ToolResult, error) {
	var blocks []ai.ContentBlock
	for _, content := range result.Content {
		blocks = append(blocks, blockContent(server, content, readableResources))
	}
	if len(blocks) == 0 && result.StructuredContent != nil {
		data, err := json.MarshalIndent(result.StructuredContent, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("encode structured content: %w", err)
		}
		blocks = append(blocks, text(string(data)))
	}
	if result.IsError && textOf(blocks) == "" {
		blocks = append(blocks, text(fmt.Sprintf("MCP tool %s/%s returned an error", server, tool)))
	}
	details := Details{Server: server, Tool: tool}
	blocks, details.FullOutputPath = limitContent(blocks)

	// Scripts get the whole result without _meta, never truncated.
	content := result.Content
	if content == nil {
		content = []sdkmcp.Content{}
	}
	structured, err := json.Marshal(struct {
		Content           []sdkmcp.Content `json:"content"`
		StructuredContent any              `json:"structuredContent,omitempty"`
		IsError           bool             `json:"isError,omitempty"`
	}{content, result.StructuredContent, result.IsError})
	if err != nil {
		return nil, fmt.Errorf("encode MCP result: %w", err)
	}
	return &agent.ToolResult{
		ContentBlocks:     blocks,
		Content:           textOf(blocks),
		StructuredContent: structured,
		IsError:           result.IsError,
		Details:           details,
	}, nil
}

func text(s string) ai.ContentBlock { return ai.ContentBlock{Type: ai.ContentBlockText, Text: s} }

func image(data []byte, mimeType string) ai.ContentBlock {
	return ai.ContentBlock{Type: ai.ContentBlockImage, Data: base64.StdEncoding.EncodeToString(data), MimeType: mimeType}
}

func blockContent(server string, content sdkmcp.Content, readableResources bool) ai.ContentBlock {
	switch c := content.(type) {
	case *sdkmcp.TextContent:
		return text(c.Text)
	case *sdkmcp.ImageContent:
		return image(c.Data, c.MIMEType)
	case *sdkmcp.AudioContent:
		return text(fmt.Sprintf("[audio %s omitted]", c.MIMEType))
	case *sdkmcp.ResourceLink:
		var details []string
		if c.MIMEType != "" {
			details = append(details, c.MIMEType)
		}
		if c.Size != nil {
			details = append(details, formatSize(*c.Size))
		}
		label := c.Title
		if label == "" {
			label = c.Name
		}
		s := fmt.Sprintf("[Resource %s %q", c.URI, label)
		if len(details) > 0 {
			s += " (" + strings.Join(details, ", ") + ")"
		}
		if c.Description != "" {
			s += ": " + c.Description
		}
		if readableResources {
			s += fmt.Sprintf(". Read it with %s (server %q)", ReadResourceTool, server)
		}
		return text(s + "]")
	case *sdkmcp.EmbeddedResource:
		r := c.Resource
		if r == nil {
			return text("[empty MCP resource]")
		}
		if r.Blob == nil {
			return text(r.Text)
		}
		if strings.HasPrefix(r.MIMEType, "image/") {
			return image(r.Blob, r.MIMEType)
		}
		if isTextMIME(r.MIMEType) {
			return text(string(r.Blob))
		}
		kind := r.MIMEType
		if kind == "" {
			kind = "unknown type"
		}
		kind += ", " + formatSize(int64(len(r.Blob)))
		saved, err := saveOutput(r.Blob, extensionOf(r.URI))
		if err != nil {
			return text(fmt.Sprintf("[Binary resource %s (%s) could not be saved: %v]", r.URI, kind, err))
		}
		return text(fmt.Sprintf("[Binary resource %s (%s) saved to %s]", r.URI, kind, saved))
	}
	return text(fmt.Sprintf("[unsupported MCP content %T]", content))
}

func isTextMIME(mimeType string) bool {
	t, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		return false
	}
	return strings.HasPrefix(t, "text/") || t == "application/json" || strings.HasSuffix(t, "+json") || strings.HasSuffix(t, "+xml")
}

var extensionRe = regexp.MustCompile(`\.[A-Za-z0-9]{1,8}$`)

func extensionOf(uri string) string {
	p := uri
	if u, err := url.Parse(uri); err == nil && u.Path != "" {
		p = u.Path
	}
	if ext := extensionRe.FindString(path.Base(p)); ext != "" {
		return ext
	}
	return ".bin"
}

func formatSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	}
}

func textOf(blocks []ai.ContentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == ai.ContentBlockText {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// limitContent keeps model-facing text within maxOutputBytes. Longer text
// becomes one block cut in the middle and naming a file with the full text;
// images follow it.
func limitContent(blocks []ai.ContentBlock) ([]ai.ContentBlock, string) {
	combined := textOf(blocks)
	if len(combined) <= maxOutputBytes {
		return blocks, ""
	}
	cut := truncateMiddle(combined, maxOutputBytes)
	saved, err := saveOutput([]byte(combined), ".txt")
	where := fmt.Sprintf("[Full output: %s (read it with offset/limit)]", saved)
	if err != nil {
		where, saved = fmt.Sprintf("[Could not save the full output: %v]", err), ""
	}
	lines := strings.Count(combined, "\n") + 1
	if strings.HasSuffix(combined, "\n") {
		lines--
	}
	out := []ai.ContentBlock{text(fmt.Sprintf("Warning: truncated output (original token count: %d)\nTotal output lines: %d\n\n%s\n\n%s",
		(len(combined)+3)/4, lines, cut, where))}
	for _, b := range blocks {
		if b.Type == ai.ContentBlockImage {
			out = append(out, b)
		}
	}
	return out, saved
}

func truncateMiddle(s string, maxBytes int) string {
	headEnd := maxBytes / 2
	for headEnd > 0 && !utf8.RuneStart(s[headEnd]) {
		headEnd--
	}
	tailStart := len(s) - (maxBytes - maxBytes/2)
	for tailStart < len(s) && !utf8.RuneStart(s[tailStart]) {
		tailStart++
	}
	removed := utf8.RuneCountInString(s[headEnd:tailStart])
	return fmt.Sprintf("%s…%d chars truncated…%s", s[:headEnd], removed, s[tailStart:])
}
