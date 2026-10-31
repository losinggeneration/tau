package sdk

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/losinggeneration/tau/agent"
	"github.com/losinggeneration/tau/agent/codemode"
	"github.com/losinggeneration/tau/ai"
)

func TestCodemodeStoreFollowsTheSessionBranch(t *testing.T) {
	_, model := registerRecordingProvider(t, "reply")
	for _, persisted := range []bool{true, false} {
		opts := RuntimeOptions{CredentialStore: &mockCredStore{getCred: ai.NewAPIKeyCredential("key")}, ExplicitModel: model, NoSession: !persisted}
		if persisted {
			opts.SessionsDir = t.TempDir()
		}
		rt, err := NewRuntime(opts)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, rt.Close(testContext(t))) })
		sess, err := rt.NewSession(testContext(t), SessionOptions{NoSession: !persisted})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, sess.Close(testContext(t))) })
		s := sess.(*agentSession)
		var tool agent.Tool
		for _, candidate := range s.agent.State().Tools {
			if candidate.Name == codemode.ToolName {
				tool = candidate
			}
		}
		require.NotNil(t, tool.Execute)
		run := func(code string) string {
			t.Helper()
			args, err := json.Marshal(map[string]string{"code": code})
			require.NoError(t, err)
			res := agent.RunToolCall(testContext(t), &agent.AgentContext{Tools: []agent.Tool{tool}}, nil, ai.ToolCall{ID: "cm", Name: codemode.ToolName, Arguments: args}, agent.AgentLoopConfig{})
			require.False(t, res.IsError, res.Content)
			blocks := res.NormalizedContent()
			if len(blocks) < 2 {
				return ""
			}
			return blocks[1].Text
		}

		sendAndDrain(t, sess, "one")
		s.mu.Lock()
		first := s.parentMsgID
		s.mu.Unlock()
		run(`store("a", 1)`)
		sendAndDrain(t, sess, "two")
		run(`store("b", 2); store("a", undefined)`)
		assert.Equal(t, `[null,2]`, run(`text(JSON.stringify([load("a"), load("b")]))`))
		if !persisted {
			continue
		}
		s.mu.Lock()
		s.parentMsgID = first
		s.mu.Unlock()
		assert.Equal(t, `[1,null]`, run(`text(JSON.stringify([load("a"), load("b")]))`), "values from a later position leaked")
	}
}

func TestCodemodeRequestLoadoutUsesCurrentSettingsAndTools(t *testing.T) {
	provider, model := registerRecordingProvider(t, "reply")
	rt, err := NewRuntime(RuntimeOptions{Settings: &ai.Settings{Codemode: ai.CodemodeSettings{Mode: "only"}}, CredentialStore: &mockCredStore{getCred: ai.NewAPIKeyCredential("key")}, ExplicitModel: model, NoSession: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close(t.Context())) })
	sess, err := rt.NewSession(t.Context(), SessionOptions{NoSession: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sess.Close(t.Context())) })
	s := sess.(*agentSession)
	tools := s.agent.State().Tools
	tools = append(tools, agent.Tool{Name: "nested", Description: "Nested.", Exposure: agent.ToolCodemode, Namespace: &agent.ToolNamespace{Name: "server"}}, agent.Tool{Name: "late", Exposure: agent.ToolDeferred}, agent.Tool{Name: "ping", Parameters: map[string]any{"type": "object"}, Execute: func(agent.ToolContext) (*agent.ToolResult, error) { return &agent.ToolResult{Content: "pong"}, nil }})
	s.agent.SetTools(tools...)
	require.NoError(t, s.agent.SetActiveTools("read", "ping", "codemode"))
	sendAndDrain(t, sess, "one")
	prompt := provider.LastPrompt()
	require.Len(t, prompt.Tools, 1)
	assert.Equal(t, "codemode", prompt.Tools[0].Name)
	assert.Contains(t, prompt.Tools[0].Description, "### `read`")
	assert.Contains(t, prompt.Tools[0].Description, "## server")
	assert.NotContains(t, prompt.Tools[0].Description, "### `late`")
	assert.Contains(t, s.agent.ActiveTools(), "read")
	assert.True(t, slices.ContainsFunc(agent.CallableTools(&agent.AgentContext{Tools: s.agent.State().Tools, Messages: s.agent.Messages()}), func(tool agent.Tool) bool { return tool.Name == "read" }))
	result := agent.RunToolCall(t.Context(), &agent.AgentContext{Tools: s.agent.State().Tools, Messages: s.agent.Messages()}, nil, ai.ToolCall{ID: "cm", Name: "codemode", Arguments: json.RawMessage(`{"code":"text(await tools.ping({}))"}`)}, agent.AgentLoopConfig{})
	require.False(t, result.IsError, result.Content)
	require.GreaterOrEqual(t, len(result.NormalizedContent()), 2)
	assert.Contains(t, result.NormalizedContent()[1].Text, "pong")
	tools = append(tools, agent.Tool{Name: "late2", Exposure: agent.ToolDeferred})
	s.agent.SetTools(tools...)
	before := prompt.Tools[0].Description
	sendAndDrain(t, sess, "still only")
	assert.Equal(t, before, provider.LastPrompt().Tools[0].Description, "deferred registration must not alter the catalog")
	s.mu.Lock()
	s.settings.Codemode = ai.CodemodeSettings{Mode: "on", InlineBudget: new(0)}
	s.mu.Unlock()
	sendAndDrain(t, sess, "two")
	prompt = provider.LastPrompt()
	require.Len(t, prompt.Tools, 3)
	for _, tool := range prompt.Tools {
		if tool.Name == "read" {
			assert.Contains(t, tool.Description, "Codemode: `tools.read(args)`")
		}
		if tool.Name == "codemode" {
			assert.Contains(t, tool.Description, "## server (tools not listed)")
			assert.NotContains(t, tool.Description, "### `read`")
		}
	}
}
