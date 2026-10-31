package codemode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/losinggeneration/tau/agent"
	"github.com/losinggeneration/tau/ai"
)

// ToolName is the name of the codemode tool.
const ToolName = "codemode"

const (
	defaultMaxOutputTokens = 10_000
	charsPerToken          = 4
)

var textOutputSchema = map[string]any{"type": "string"}

// Description is upstream's intro and globals list.
const Description = "Run JavaScript that calls other tools. The input is raw JavaScript (not JSON, no code fence), run as an async function body in a sandbox: top-level `await` and `return` work. No Node, file system, network, or timers.\n" +
	"- `await tools.<name>({ ...args })` resolves to a string, or an object if the tool's declaration says so, and rejects with an Error on failure. A tool whose declaration returns an object resolves on failure too when it returned that object; check its fields. Calls still running when the script ends are cancelled.\n" +
	"- Optional first line: `// @options: {\"max_output_tokens\": 10000, \"timeout_ms\": 60000}`\n\n" +
	"Globals:\n" +
	"- `text(value)`, `image(dataUrlOrImageBlock)`, `console.log(...)`, and top-level `return` add output; `exit()` ends the script. With several text items, each starts with a `==> text N/M <==` line, and `console` lines follow the other output in one `<console_output>` block. `image()` also saves the image to a temp file and the result names its path.\n" +
	"- `store(key, value)` and `load(key)` keep JSON values across codemode calls.\n" +
	"- `ALL_TOOLS`, `await searchTools(query, { limit?, namespace? })`, `await describeTool(name)`, `await describeNamespace(name)`: find unlisted tools, such as MCP tools."

var imageExtensions = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// StoreEntryType is the custom session entry type holding one script's
// store() writes.
const StoreEntryType = "codemode-store"

// Store keeps store() values across scripts: Load returns the values on the
// session's current branch and Save records a successful script's writes.
type Store struct {
	Load func(context.Context) (map[string]json.RawMessage, error)
	Save func(context.Context, StoreWrites) error
}

// ApplyStoreWrites folds codemode-store entry data, oldest first, into values.
func ApplyStoreWrites(values map[string]json.RawMessage, data json.RawMessage) error {
	var writes StoreWrites
	if err := json.Unmarshal(data, &writes); err != nil {
		return fmt.Errorf("decode codemode store entry: %w", err)
	}
	for key, value := range writes.Set {
		values[key] = value
	}
	for _, key := range writes.Delete {
		delete(values, key)
	}
	return nil
}

// NewTool returns the codemode tool. It is inactive by default and model-only,
// so scripts can't start other scripts. Without a store, store() writes last
// only for the current script.
func NewTool(store *Store) agent.Tool {
	inactive := false
	return agent.Tool{
		PrepareLoadout: func(_ context.Context, loadout agent.ToolLoadout) (agent.ToolLoadoutChanges, error) {
			return PrepareLoadout(loadout, "on", 3000), nil
		},
		Name:          ToolName,
		Label:         ToolName,
		Description:   Description,
		DefaultActive: &inactive,
		Exposure:      agent.ToolModelOnly,
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"code": map[string]any{"type": "string", "description": "Raw JavaScript source."}},
			"required":   []string{"code"},
		},
		ReachesTools: scriptReach,
		Execute:      func(tc agent.ToolContext) (*agent.ToolResult, error) { return execute(tc, store) },
	}
}

var (
	discoveryGlobals = regexp.MustCompile(`\b(searchTools|describeNamespace|describeTool|ALL_TOOLS)\b`)
	scriptWords      = regexp.MustCompile(`\w+`)
)

// scriptReach names every tool when the script uses a discovery global, else
// the words of its source, so tools["mcp__docs__" + name] reaches mcp__docs.
func scriptReach(args map[string]any) []string {
	code, _ := args["code"].(string)
	if discoveryGlobals.MatchString(code) {
		return []string{"all"}
	}
	return scriptWords.FindAllString(code, -1)
}

func execute(tc agent.ToolContext, store *Store) (*agent.ToolResult, error) {
	started := time.Now()
	source, _ := tc.Args["code"].(string)
	code, sourceOpts, err := ParseSource(source)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(-1)
	if sourceOpts.TimeoutMs != nil {
		timeout = time.Duration(*sourceOpts.TimeoutMs) * time.Millisecond
	}
	opts := Options{Tools: scriptTools(tc), Timeout: timeout}
	opts.Globals = discoveryTools(tc, opts.Tools)
	if store != nil && store.Load != nil {
		if opts.Store, err = store.Load(tc.Context); err != nil {
			return nil, fmt.Errorf("load codemode store: %w", err)
		}
	}
	result, err := Execute(tc.Context, code, opts)
	if err != nil {
		return nil, err
	}
	if writes := result.StoreWrites; result.Error == nil && store != nil && store.Save != nil && (len(writes.Set) > 0 || len(writes.Delete) > 0) {
		if writes.Delete == nil {
			writes.Delete = []string{}
		}
		if err := store.Save(tc.Context, writes); err != nil {
			return nil, fmt.Errorf("save codemode store: %w", err)
		}
	}

	output := result.Output
	if result.Error == nil && result.Value != nil {
		output = append(output, OutputItem{Type: "text", Text: valueText(result.Value)})
	}
	items := formatOutput(output)
	if result.Error != nil {
		items = append(items, ai.ContentBlock{Type: ai.ContentBlockText, Text: "Script error:\n" + formatError(result)})
	}
	maxTokens := int64(defaultMaxOutputTokens)
	if sourceOpts.MaxOutputTokens != nil {
		maxTokens = *sourceOpts.MaxOutputTokens
	}
	items = joinAdjacentText(saveImages(truncateOutput(joinAdjacentText(items), maxTokens)))

	header := "Script completed"
	if result.Error != nil {
		header = "Script failed"
	}
	header = fmt.Sprintf("%s\nWall time %.1f seconds\nOutput:\n", header, time.Since(started).Seconds())
	return &agent.ToolResult{
		ContentBlocks: append([]ai.ContentBlock{{Type: ai.ContentBlockText, Text: header}}, items...),
		IsError:       result.Error != nil,
	}, nil
}

// scriptTools exposes every reachable tool except codemode itself. A tool
// with an output schema resolves to its structured content, also on error;
// others resolve to their text, and failures reject with it.
func scriptTools(tc agent.ToolContext) []Tool {
	var tools []Tool
	for _, tool := range tc.Tools {
		if tool.Name == ToolName || tc.ExecuteToolContext == nil {
			continue
		}
		var output any = textOutputSchema
		if tool.OutputSchema != nil {
			output = tool.OutputSchema
		}
		tools = append(tools, Tool{
			Name:        tool.Name,
			Description: ToolSample(tool.Name, tool.Description, tool.Parameters, output),
			Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
				if args == nil {
					args = json.RawMessage(`{}`)
				}
				res := tc.ExecuteToolContext(ctx, ai.ToolCall{Name: tool.Name, Arguments: args})
				if tool.OutputSchema != nil && res.StructuredContent != nil {
					return res.StructuredContent, nil
				}
				text := resultText(res)
				if res.IsError {
					if text == "" {
						text = fmt.Sprintf("Tool %q failed", tool.Name)
					}
					return nil, errors.New(text)
				}
				return text, nil
			},
		})
	}
	return tools
}

const defaultSearchLimit = 8

type toolEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// discoveryTools are searchTools(), describeTool() and describeNamespace()
// over the script's tools, whose descriptions are already their samples.
func discoveryTools(tc agent.ToolContext, scripts []Tool) []Tool {
	var callable []agent.Tool
	samples := map[string]string{}
	for _, tool := range scripts {
		samples[tool.Name] = tool.Description
	}
	for _, tool := range tc.Tools {
		if _, ok := samples[tool.Name]; ok {
			callable = append(callable, tool)
		}
	}
	entry := func(name string) toolEntry { return toolEntry{Name: Identifier(name), Description: samples[name]} }
	// Spread globals always receive a JSON array; missing arguments are nil.
	spread := func(args json.RawMessage) []json.RawMessage {
		var list []json.RawMessage
		if json.Unmarshal(args, &list) != nil {
			list = nil
		}
		return append(list, nil, nil)
	}
	stringArg := func(raw json.RawMessage) (string, bool) {
		var s string
		return s, raw != nil && json.Unmarshal(raw, &s) == nil
	}
	return []Tool{
		{Name: "searchTools", Spread: true, Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			list := spread(args)
			query, ok := stringArg(list[0])
			if !ok {
				return nil, errors.New("searchTools() expects a query string")
			}
			var opts struct {
				Limit     *float64        `json:"limit"`
				Namespace json.RawMessage `json:"namespace"`
			}
			if list[1] != nil && string(list[1]) != "null" && json.Unmarshal(list[1], &opts) != nil {
				return nil, errors.New("searchTools() options must be an object")
			}
			limit := defaultSearchLimit
			if opts.Limit != nil {
				if *opts.Limit <= 0 || *opts.Limit != float64(int(*opts.Limit)) {
					return nil, errors.New("searchTools() limit must be a positive integer")
				}
				limit = int(*opts.Limit)
			}
			var namespace string
			if opts.Namespace != nil && string(opts.Namespace) != "null" {
				if namespace, ok = stringArg(opts.Namespace); !ok {
					return nil, errors.New("searchTools() namespace must be a string")
				}
			}
			entries := []toolEntry{}
			if strings.TrimSpace(query) == "" {
				return entries, nil
			}
			// agent.SearchTools ranks only deferred and codemode tools; a script
			// searches everything it can call.
			searchable := make([]agent.Tool, len(callable))
			for i, tool := range callable {
				tool.Exposure = agent.ToolDeferred
				searchable[i] = tool
			}
			matches, err := agent.SearchTools(ctx, searchable, query, namespace, limit)
			if err != nil {
				return nil, err
			}
			for _, match := range matches {
				entries = append(entries, entry(match.Name))
			}
			return entries, nil
		}},
		{Name: "describeTool", Spread: true, Execute: func(_ context.Context, args json.RawMessage) (any, error) {
			name, ok := stringArg(spread(args)[0])
			if !ok {
				return nil, errors.New("describeTool() expects a tool name")
			}
			if tool := agent.DescribeTool(callable, name); tool != nil {
				return samples[tool.Name], nil
			}
			return nil, nil
		}},
		{Name: "describeNamespace", Spread: true, Execute: func(_ context.Context, args json.RawMessage) (any, error) {
			name, ok := stringArg(spread(args)[0])
			if !ok {
				return nil, errors.New("describeNamespace() expects a namespace name")
			}
			namespace := agent.DescribeNamespace(callable, name)
			if namespace == nil {
				return nil, nil
			}
			names := []string{}
			for _, tool := range callable {
				if tool.Namespace != nil && tool.Namespace.Name == namespace.Name {
					names = append(names, Identifier(tool.Name))
				}
			}
			return struct {
				Name         string   `json:"name"`
				Description  string   `json:"description,omitempty"`
				Instructions string   `json:"instructions,omitempty"`
				Tools        []string `json:"tools"`
			}{namespace.Name, namespace.Description, namespace.Instructions, names}, nil
		}},
	}
}

func resultText(res *agent.ToolResult) string {
	var parts []string
	for _, block := range res.NormalizedContent() {
		if block.Type == ai.ContentBlockText {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func valueText(value json.RawMessage) string {
	var text string
	if json.Unmarshal(value, &text) == nil {
		return text
	}
	return string(value)
}

// formatOutput numbers text items when there are several and gathers console
// lines into one block after the rest of the output.
func formatOutput(output []OutputItem) []ai.ContentBlock {
	total := 0
	for _, item := range output {
		if item.Type == "text" && !item.Console {
			total++
		}
	}
	var items []ai.ContentBlock
	var console []string
	index := 0
	for _, item := range output {
		switch {
		case item.Type == "image":
			items = append(items, ai.ContentBlock{Type: ai.ContentBlockImage, Data: item.Data, MimeType: item.MIMEType})
		case item.Console:
			console = append(console, item.Text)
		default:
			index++
			text := item.Text
			if total > 1 {
				text = fmt.Sprintf("==> text %d/%d <==\n%s", index, total, text)
			}
			items = append(items, ai.ContentBlock{Type: ai.ContentBlockText, Text: text})
		}
	}
	if len(console) > 0 {
		items = append(items, ai.ContentBlock{Type: ai.ContentBlockText, Text: "<console_output>\n" + strings.Join(console, "\n") + "\n</console_output>"})
	}
	return items
}

// joinAdjacentText merges neighboring text items, each part on its own line.
func joinAdjacentText(items []ai.ContentBlock) []ai.ContentBlock {
	var joined []ai.ContentBlock
	var text strings.Builder
	var parts int
	last := ""
	flush := func() {
		if parts > 0 {
			joined = append(joined, ai.ContentBlock{Type: ai.ContentBlockText, Text: text.String()})
		}
		text.Reset()
		parts, last = 0, ""
	}
	for _, item := range items {
		if item.Type != ai.ContentBlockText {
			flush()
			joined = append(joined, item)
			continue
		}
		if last != "" && !strings.HasSuffix(last, "\n") {
			text.WriteString("\n")
			last = "\n"
		}
		text.WriteString(item.Text)
		parts++
		if item.Text != "" {
			last = item.Text
		}
	}
	flush()
	return joined
}

func formatError(result Result) string {
	e := result.Error
	var head string
	switch e.Kind {
	case ErrorScript:
		head = e.Stack
		if head == "" {
			name := e.Name
			if name == "" {
				name = "Error"
			}
			head = name + ": " + e.Message
		}
	case ErrorTimeout:
		head = "Script timed out: " + e.Message
	case ErrorAborted:
		head = "Script aborted: " + e.Message
	default:
		head = "Script sandbox failed: " + e.Message
	}
	summary := "No tool calls were made."
	if len(result.Calls) > 0 {
		calls := make([]string, len(result.Calls))
		for i, call := range result.Calls {
			calls[i] = fmt.Sprintf("%s (%s)", call.Name, call.Status)
		}
		summary = "Tool calls made before the failure (they are not undone): " + strings.Join(calls, ", ")
	}
	return head + "\n\n" + summary
}

// truncateOutput keeps the start and end of text over the token budget, in
// one item followed by the images, and writes the full text to a temp file.
func truncateOutput(items []ai.ContentBlock, maxTokens int64) []ai.ContentBlock {
	var texts []string
	var images []ai.ContentBlock
	for _, item := range items {
		if item.Type == ai.ContentBlockText {
			texts = append(texts, item.Text)
		} else {
			images = append(images, item)
		}
	}
	combined := strings.Join(texts, "\n")
	runes := []rune(combined)
	budget := int(maxTokens) * charsPerToken
	if len(texts) == 0 || len(runes) <= budget {
		return items
	}
	headChars := budget / 2
	tailChars := budget - headChars
	removed := len(runes) - headChars - tailChars
	text := fmt.Sprintf("Warning: truncated output (original token count: %d)\nTotal output lines: %d\n\n%s…%d tokens truncated…%s",
		ceilDiv(len(runes), charsPerToken), strings.Count(combined, "\n")+1,
		string(runes[:headChars]), ceilDiv(removed, charsPerToken), string(runes[len(runes)-tailChars:]))
	if path, err := writeOutputFile(".txt", []byte(combined)); err == nil {
		text += fmt.Sprintf("\n\n[Full output: %s (read with offset/limit)]", path)
	} else {
		text += fmt.Sprintf("\n\n[Could not save the full output: %v]", err)
	}
	return append([]ai.ContentBlock{{Type: ai.ContentBlockText, Text: text}}, images...)
}

// saveImages writes each image to a temp file and labels it with the path,
// since the model has no other way to reach the bytes.
func saveImages(items []ai.ContentBlock) []ai.ContentBlock {
	labels := map[string]string{}
	var out []ai.ContentBlock
	for _, item := range items {
		if item.Type != ai.ContentBlockImage {
			out = append(out, item)
			continue
		}
		label, ok := labels[item.Data]
		if !ok {
			label = imageLabel(item)
			labels[item.Data] = label
		}
		out = append(out, ai.ContentBlock{Type: ai.ContentBlockText, Text: label}, item)
	}
	return out
}

func imageLabel(image ai.ContentBlock) string {
	data, err := base64.StdEncoding.DecodeString(image.Data)
	if err != nil {
		return fmt.Sprintf("[Image (%s) could not be saved: %v]", image.MimeType, err)
	}
	kind := fmt.Sprintf("%s, %s", image.MimeType, formatSize(len(data)))
	path, err := writeOutputFile(imageExtensions[image.MimeType], data)
	if err != nil {
		return fmt.Sprintf("[Image (%s) could not be saved: %v]", kind, err)
	}
	return fmt.Sprintf("[Image saved to %s (%s)]", path, kind)
}

func writeOutputFile(ext string, data []byte) (path string, err error) {
	f, err := os.CreateTemp("", "tau-codemode-*"+ext)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	_, err = f.Write(data)
	return f.Name(), err
}

func formatSize(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	}
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }
