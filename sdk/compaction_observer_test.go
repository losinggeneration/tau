package sdk

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/losinggeneration/tau/ai"
)

// compactionObserverSession stands up a session over a stub provider, with no
// plugin manager. That is the case that matters: compaction events used to go
// to the plugin manager alone, so a frontend without plugins could not observe
// compaction at all.
func compactionObserverSession(t *testing.T) Session {
	t.Helper()
	ai.Clear()
	ai.Init()

	provider := &recordingProvider{
		api:          "test-compaction-observer",
		responseText: "a summary of the session",
		caps:         ai.Capabilities{Tools: true, Streaming: true},
	}
	ai.Register("test-compaction-observer", provider)
	ai.RegisterModel(&ai.Model{
		ID:       "compaction-observer-model",
		Provider: "test-compaction-observer",
		API:      "test-compaction-observer",
	})

	rt, err := NewRuntime(RuntimeOptions{
		CredentialStore: &mockCredStore{getCred: ai.NewAPIKeyCredential("sk-test-key")},
		SessionsDir:     t.TempDir(),
		ExplicitModel:   "test-compaction-observer/default/compaction-observer-model",
	})
	require.NoError(t, err)

	sess, err := rt.NewSession(testContext(t), SessionOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.Close(testContext(t)) })
	return sess
}

// app.md §9 needs the frontend to see compaction begin and end. Before this the
// only dispatch was to the plugin manager, so the TUI could not show an
// indicator even in principle.
func TestCompactionReachesTheEventObserver(t *testing.T) {
	sess := compactionObserverSession(t)

	observer, ok := sess.(EventObserver)
	require.True(t, ok, "agentSession should implement EventObserver")

	var seen []Event
	observer.SetEventObserver(func(e Event) { seen = append(seen, e) })

	require.NoError(t, sess.Compact(testContext(t)))

	var types []string
	for _, e := range seen {
		types = append(types, e.Type)
	}
	assert.Equal(t, []string{"compaction_start", "compaction_end"}, types)
}

// compaction_end carries the summary and the reclaimed token count: the UI
// commits a summary message and has no other way to learn what it said.
func TestCompactionEndCarriesTheSummary(t *testing.T) {
	sess := compactionObserverSession(t)

	var end Event
	sess.(EventObserver).SetEventObserver(func(e Event) {
		if e.Type == "compaction_end" {
			end = e
		}
	})
	require.NoError(t, sess.Compact(testContext(t)))

	payload, ok := end.Payload.(map[string]any)
	require.True(t, ok, "compaction_end payload should be a map, got %T", end.Payload)
	assert.NotEmpty(t, payload["summary"], "compaction_end must carry the summary")
	assert.Contains(t, payload, "tokens_before")
	assert.Equal(t, sess.ID(), payload["session_id"])
}

// Events carry the session's monotonic sequence so a frontend can order them
// against the ones a Send channel delivers.
func TestCompactionEventsAreSequenced(t *testing.T) {
	sess := compactionObserverSession(t)

	var seen []Event
	sess.(EventObserver).SetEventObserver(func(e Event) { seen = append(seen, e) })
	require.NoError(t, sess.Compact(testContext(t)))

	require.Len(t, seen, 2)
	assert.Less(t, seen[0].Seq, seen[1].Seq, "sequence should advance across events")
	for _, e := range seen {
		assert.NotEmpty(t, e.Timestamp, "event %s has no timestamp", e.Type)
		assert.Equal(t, sess.ID(), e.SessionID)
	}
}

// Compaction must work unobserved; the observer is optional and clearable.
func TestCompactionWithoutAnObserver(t *testing.T) {
	sess := compactionObserverSession(t)
	require.NoError(t, sess.Compact(testContext(t)), "compaction with no observer")

	var calls int
	observer := sess.(EventObserver)
	observer.SetEventObserver(func(Event) { calls++ })
	require.NoError(t, sess.Compact(testContext(t)))
	require.Positive(t, calls)

	observer.SetEventObserver(nil)
	before := calls
	require.NoError(t, sess.Compact(testContext(t)))
	assert.Equal(t, before, calls, "cleared observer still received events")
}
