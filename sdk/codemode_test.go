package sdk

import (
	"encoding/json"
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
