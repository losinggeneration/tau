package sdk

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/losinggeneration/tau/agent"
)

const toolSearchName = "tool_search"

const toolSearchDescription = "# Tool discovery\n\nSearches over deferred tool metadata with BM25 and exposes matching tools for the next model call.\n\nSome of the tools, such as tools of MCP servers, may not have been provided to you upfront, and you should use this tool (`tool_search`) to search for the required tools. For MCP tool discovery, always use `tool_search`."

func toolSearchTool(execute agent.ToolFunc) agent.Tool {
	return agent.Tool{
		Name: toolSearchName, Label: toolSearchName, Description: toolSearchDescription,
		DefaultActive: new(false), Exposure: agent.ToolModelOnly,
		Parameters: map[string]any{
			"type": "object", "required": []string{"query"},
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Search query for deferred tools."},
				"limit": map[string]any{"type": "number", "description": "Maximum number of tools to return. Defaults to 8."},
			},
		},
		ReachesTools: func(map[string]any) []string { return []string{"all"} },
		Execute:      execute,
	}
}

func (s *agentSession) searchAndLoadTools(tc agent.ToolContext) (*agent.ToolResult, error) {
	query, _ := tc.Args["query"].(string)
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("query must not be empty")
	}
	limit := 8
	if value, ok := tc.Args["limit"]; ok {
		number, ok := value.(float64)
		if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number <= 0 || math.Trunc(number) != number {
			return nil, errors.New("limit must be a positive integer")
		}
		limit = math.MaxInt
		if number < float64(math.MaxInt) {
			limit = int(number)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("session closed")
	}
	active := s.agent.ActiveTools()
	candidates := slices.DeleteFunc(s.agent.State().Tools, func(tool agent.Tool) bool { return slices.Contains(active, tool.Name) })
	matches, err := agent.SearchTools(tc.Context, candidates, query, "", limit)
	if err != nil {
		return nil, err
	}
	loaded := make([]string, 0, len(matches))
	lines := make([]string, 0, len(matches))
	for _, tool := range matches {
		loaded = append(loaded, tool.Name)
		description, _, _ := strings.Cut(strings.TrimSpace(tool.Description), "\n")
		lines = append(lines, "- "+tool.Name+": "+strings.TrimSuffix(description, "\r"))
	}
	text := "No matching tools found."
	if len(matches) > 0 {
		if err := s.agent.SetActiveTools(append(active, loaded...)...); err != nil {
			return nil, fmt.Errorf("load tools: %w", err)
		}
		plural := "s"
		if len(matches) == 1 {
			plural = ""
		}
		text = fmt.Sprintf("Loaded %d tool%s. They are available from your next call:\n%s", len(matches), plural, strings.Join(lines, "\n"))
	}
	return &agent.ToolResult{Content: text, Details: map[string]any{"loaded": loaded}}, nil
}
