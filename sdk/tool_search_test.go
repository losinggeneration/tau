package sdk

import (
	"context"
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/losinggeneration/tau/agent"
	"github.com/losinggeneration/tau/ai"
)

func TestToolSearchLoadsOnlyInactiveDiscoverableTools(t *testing.T) {
	a, err := agent.New(agent.NewConfig(agent.WithTools(
		agent.Tool{Name: "read"},
		agent.Tool{Name: "loaded", Exposure: agent.ToolDeferred},
		agent.Tool{Name: "docs_search", Description: "  Search manuals.\r\nSecond line.", Exposure: agent.ToolDeferred},
		agent.Tool{Name: "docs_write", Description: "Write manuals.", Exposure: agent.ToolCodemode},
		agent.Tool{Name: "hidden", Description: "manuals", Exposure: agent.ToolHidden},
		agent.Tool{Name: "model", Description: "manuals", Exposure: agent.ToolModelOnly, DefaultActive: new(false)},
	)))
	require.NoError(t, err)
	s := &agentSession{agent: a}
	require.NoError(t, a.SetActiveTools("read", "loaded"))
	tool := toolSearchTool(s.searchAndLoadTools)
	assert.Equal(t, agent.ToolModelOnly, tool.Exposure)
	require.NotNil(t, tool.DefaultActive)
	assert.False(t, *tool.DefaultActive)
	assert.Equal(t, []string{"all"}, tool.ReachesTools(nil))
	result, err := tool.Execute(agent.ToolContext{Context: t.Context(), Args: map[string]any{"query": "search manuals", "limit": float64(1)}})
	require.NoError(t, err)
	assert.Equal(t, "Loaded 1 tool. They are available from your next call:\n- docs_search: Search manuals.", result.Content)
	assert.Equal(t, map[string]any{"loaded": []string{"docs_search"}}, result.Details)
	assert.Equal(t, []string{"read", "loaded", "docs_search"}, a.ActiveTools())
	baseline := ai.CurrentSystemMessage(agent.ConvertMessagesToAI(a.Messages()))
	require.NotNil(t, baseline)
	assert.NotContains(t, baseline.ToolsAdded, ai.Tool{Name: "docs_search"}, "activation waits for the next request")
	assert.Len(t, baseline.ToolsAdded, 1)
	result, err = tool.Execute(agent.ToolContext{Context: t.Context(), Args: map[string]any{"query": "manuals"}})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"loaded": []string{"docs_write"}}, result.Details)
	result, err = tool.Execute(agent.ToolContext{Context: t.Context(), Args: map[string]any{"query": "manuals"}})
	require.NoError(t, err)
	assert.Equal(t, "No matching tools found.", result.Content)
	assert.Equal(t, map[string]any{"loaded": []string{}}, result.Details)
}

func TestToolSearchRejectsInvalidAndCancelledCalls(t *testing.T) {
	a, err := agent.New(agent.NewConfig())
	require.NoError(t, err)
	s := &agentSession{agent: a}
	for _, args := range []map[string]any{
		{"query": " "},
		{"query": 7},
		{"query": "docs", "limit": float64(0)},
		{"query": "docs", "limit": -1.0},
		{"query": "docs", "limit": 1.5},
		{"query": "docs", "limit": "8"},
		{"query": "docs", "limit": math.Inf(1)},
		{"query": "docs", "limit": math.NaN()},
	} {
		_, err := s.searchAndLoadTools(agent.ToolContext{Context: t.Context(), Args: args})
		require.Error(t, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.searchAndLoadTools(agent.ToolContext{Context: ctx, Args: map[string]any{"query": "docs"}})
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, a.ActiveTools())
}

func TestToolSearchRegistrationAndDefaultLimit(t *testing.T) {
	_, model := registerRecordingProvider(t, "reply")
	rt, err := NewRuntime(RuntimeOptions{CredentialStore: &mockCredStore{getCred: ai.NewAPIKeyCredential("key")}, ExplicitModel: model, NoSession: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close(t.Context())) })
	for _, selection := range [][]string{nil, {"tool_search"}, {"read"}, {}} {
		sess, err := rt.NewSession(t.Context(), SessionOptions{ToolAllowlist: selection, NoSession: true})
		require.NoError(t, err)
		s := sess.(*agentSession)
		registered := agent.DescribeTool(s.RegisteredTools(), toolSearchName)
		if selection == nil || slices.Contains(selection, toolSearchName) {
			require.NotNil(t, registered)
			assert.Equal(t, selection != nil, slices.Contains(s.ActiveTools(), toolSearchName))
		} else {
			assert.Nil(t, registered)
		}
		require.NoError(t, sess.Close(t.Context()))
	}
	a, err := agent.New(agent.NewConfig())
	require.NoError(t, err)
	for i := range 10 {
		a.AddTools(agent.Tool{Name: fmt.Sprintf("docs_%d", i), Exposure: agent.ToolDeferred})
	}
	s := &agentSession{agent: a}
	result, err := s.searchAndLoadTools(agent.ToolContext{Context: t.Context(), Args: map[string]any{"query": "docs"}})
	require.NoError(t, err)
	assert.Len(t, a.ActiveTools(), 8)
	assert.Contains(t, result.Content, "Loaded 8 tools.")
	result, err = s.searchAndLoadTools(agent.ToolContext{Context: t.Context(), Args: map[string]any{"query": "docs", "limit": 1e30}})
	require.NoError(t, err)
	assert.Contains(t, result.Content, "Loaded 2 tools.")
}
