package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/losinggeneration/tau/ai"
)

func modelDeclarable(tool Tool) bool {
	switch tool.Exposure {
	case "", ToolDirect, ToolModelOnly, ToolCodemode, ToolDeferred:
		return true
	default:
		return false
	}
}

// SetActiveTools stages an exact model loadout for the next request boundary.
func (a *Agent) SetActiveTools(names ...string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, name := range names {
		if !slices.ContainsFunc(a.state.Tools, func(tool Tool) bool { return tool.Name == name && modelDeclarable(tool) }) {
			return fmt.Errorf("tool %q cannot be activated", name)
		}
	}
	a.pendingActiveTools = slices.Clone(names)
	a.activeToolsChanged = true
	return nil
}

// CallableTools returns the registry authorized for ordinary nested execution.
func CallableTools(current *AgentContext) []Tool {
	baseline := ai.CurrentSystemMessage(ConvertMessagesToAI(current.Messages))
	var tools []Tool
	for _, tool := range current.Tools {
		active := !hasToolDeclarationHistory(current.Messages) && (tool.DefaultActive == nil || *tool.DefaultActive)
		if baseline != nil {
			active = slices.ContainsFunc(baseline.ToolsAdded, func(declaration ai.Tool) bool { return declaration.Name == tool.Name })
		}
		if tool.Exposure == ToolCodemode || tool.Exposure == ToolDeferred || ((tool.Exposure == "" || tool.Exposure == ToolDirect) && active) {
			tools = append(tools, tool)
		}
	}
	return tools
}

// ReachableTools intersects caller reach with the nested execution loadout.
// A single "*" word denotes all callable tools.
func ReachableTools(current *AgentContext, caller Tool, args map[string]any) []Tool {
	if caller.ReachesTools == nil {
		return nil
	}
	words := caller.ReachesTools(args)
	return reachableTools(current, words)
}

// ReachableToolsContext evaluates cancellable plugin reach callbacks.
func ReachableToolsContext(ctx context.Context, current *AgentContext, caller Tool, args map[string]any) ([]Tool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if caller.ReachTools == nil {
		return ReachableTools(current, caller, args), nil
	}
	words, err := caller.ReachTools(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("evaluate tool reach: %w", err)
	}
	return reachableTools(current, words), nil
}

func reachableTools(current *AgentContext, words []string) []Tool {
	return slices.DeleteFunc(CallableTools(current), func(tool Tool) bool {
		for _, word := range words {
			if (word == "*" || word == "all") || strings.Contains(word, tool.Name) {
				return false
			}
			if tool.Namespace != nil && strings.Contains(word, tool.Namespace.Name) {
				return false
			}
		}
		return true
	})
}

func toolIdentifier(name string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return '_'
	}, name)
}

// DescribeTool resolves a registered name or its script identifier.
func DescribeTool(tools []Tool, name string) *Tool {
	for _, tool := range tools {
		if tool.Name == name || toolIdentifier(tool.Name) == toolIdentifier(name) {
			return &tool
		}
	}
	return nil
}

// DescribeNamespace resolves namespace names and aliases in the supplied registry.
func DescribeNamespace(tools []Tool, name string) *ToolNamespace {
	for _, tool := range tools {
		if tool.Namespace == nil {
			continue
		}
		for _, alias := range append([]string{tool.Namespace.Name}, tool.Namespace.Aliases...) {
			if toolIdentifier(alias) == toolIdentifier(name) {
				return tool.Namespace
			}
		}
	}
	return nil
}

// SearchTools searches discoverable metadata. Callers may run it asynchronously with cancellation.
func SearchTools(ctx context.Context, tools []Tool, query, namespace string, limit int) ([]Tool, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("search tools: %w", err)
	}
	if strings.TrimSpace(query) == "" || limit <= 0 {
		return nil, fmt.Errorf("search requires a query and positive limit")
	}
	var selectedNamespace *ToolNamespace
	if namespace != "" {
		selectedNamespace = DescribeNamespace(tools, namespace)
		if selectedNamespace == nil {
			return nil, nil
		}
	}
	type match struct {
		tool  Tool
		score float64
	}
	var matches []match
	var counts []map[string]int
	var lengths []int
	var totalLength int
	for _, tool := range tools {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("search tools: %w", err)
		}
		if tool.Exposure != ToolDeferred && tool.Exposure != ToolCodemode {
			continue
		}
		if selectedNamespace != nil && (tool.Namespace == nil || tool.Namespace.Name != selectedNamespace.Name) {
			continue
		}
		text := tool.Name + " " + tool.Description
		if tool.Parameters != nil {
			schema, err := json.Marshal(tool.Parameters)
			if err != nil {
				return nil, fmt.Errorf("search tool %s schema: %w", tool.Name, err)
			}
			text += " " + string(schema)
		}
		if tool.Namespace != nil {
			text += " " + tool.Namespace.Name + " " + tool.Namespace.Description + " " + tool.Namespace.Instructions + " " + strings.Join(tool.Namespace.Aliases, " ")
		}
		terms := searchTerms(text)
		frequencies := make(map[string]int)
		for _, term := range terms {
			frequencies[term]++
		}
		counts = append(counts, frequencies)
		lengths = append(lengths, len(terms))
		totalLength += len(terms)
		matches = append(matches, match{tool: tool})
	}
	if len(matches) == 0 {
		return nil, nil
	}
	average := max(1, float64(totalLength)/float64(len(matches)))
	queryTerms := make(map[string]bool)
	for _, term := range searchTerms(query) {
		queryTerms[term] = true
	}
	for _, term := range slices.Sorted(maps.Keys(queryTerms)) {
		var frequency int
		for _, count := range counts {
			if count[term] > 0 {
				frequency++
			}
		}
		idf := math.Log(1 + (float64(len(matches)-frequency)+0.5)/(float64(frequency)+0.5))
		for i, count := range counts {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("rank tools: %w", err)
			}
			tf := float64(count[term])
			norm := 1.2 * (0.25 + 0.75*float64(lengths[i])/average)
			matches[i].score += idf * tf * 2.2 / (tf + norm)
		}
	}
	matches = slices.DeleteFunc(matches, func(m match) bool { return m.score == 0 })
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].score > matches[j].score })
	var result []Tool
	for _, match := range matches[:min(limit, len(matches))] {
		result = append(result, match.tool)
	}
	return result, nil
}

func requestToolDeclarations(tools []Tool, messages []*Message) ([]ai.Tool, error) {
	declarations, err := visibleToolDeclarations(tools)
	if err != nil {
		return nil, err
	}
	baseline := ai.CurrentSystemMessage(ConvertMessagesToAI(messages))
	if baseline == nil {
		return declarations, nil
	}
	for _, tool := range tools {
		if !modelDeclarable(tool) || slices.ContainsFunc(declarations, func(t ai.Tool) bool { return t.Name == tool.Name }) {
			continue
		}
		if !slices.ContainsFunc(baseline.ToolsAdded, func(t ai.Tool) bool { return t.Name == tool.Name }) {
			continue
		}
		declaration, err := agentToolToAITool(tool)
		if err != nil {
			return nil, fmt.Errorf("loaded tool %s: %w", tool.Name, err)
		}
		declarations = append(declarations, declaration)
	}
	return declarations, nil
}

// ActiveTools returns the pending loadout, or the transcript-owned loadout.
func (a *Agent) ActiveTools() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.activeToolsChanged {
		return slices.Clone(a.pendingActiveTools)
	}
	var names []string
	baseline := ai.CurrentSystemMessage(ConvertMessagesToAI(a.state.Messages))
	for _, tool := range a.state.Tools {
		if !modelDeclarable(tool) {
			continue
		}
		active := (tool.Exposure == "" || tool.Exposure == ToolDirect || tool.Exposure == ToolModelOnly) && (tool.DefaultActive == nil || *tool.DefaultActive)
		if hasToolDeclarationHistory(a.state.Messages) {
			active = baseline != nil && slices.ContainsFunc(baseline.ToolsAdded, func(t ai.Tool) bool { return t.Name == tool.Name })
		}
		if active {
			names = append(names, tool.Name)
		}
	}
	return names
}

func searchTerms(text string) []string {
	runes := []rune(text)
	var split strings.Builder
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1]) || (i+1 < len(runes) && unicode.IsUpper(runes[i-1]) && unicode.IsLower(runes[i+1]))) {
			split.WriteByte(' ')
		}
		split.WriteRune(unicode.ToLower(r))
	}
	terms := strings.FieldsFunc(split.String(), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	var result []string
	for _, term := range terms {
		if strings.Contains(" a an and are as at be by for from in is it of on or that the this to with ", " "+term+" ") {
			continue
		}
		switch {
		case len(term) > 4 && strings.HasSuffix(term, "ies"):
			term = strings.TrimSuffix(term, "ies") + "y"
		case len(term) > 4 && (strings.HasSuffix(term, "ches") || strings.HasSuffix(term, "shes") || strings.HasSuffix(term, "sses") || strings.HasSuffix(term, "xes") || strings.HasSuffix(term, "zes")):
			term = term[:len(term)-2]
		case len(term) > 3 && strings.HasSuffix(term, "s") && !strings.HasSuffix(term, "ss"):
			term = term[:len(term)-1]
		}
		result = append(result, term)
	}
	return result
}
