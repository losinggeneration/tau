package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/losinggeneration/tau/ai"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCLIShareSuccess verifies that CLI.Share succeeds end-to-end with a fake
// GitHub Gist endpoint, correct auth header, and returns the gist URL.
func TestNewCLISessionDirResolution(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", cfgDir)
	t.Setenv("TAU_SESSION_DIR", "")

	// Default: <configDir>/sessions.
	c, err := NewCLI("")
	if err != nil {
		t.Fatalf("NewCLI default: %v", err)
	}
	if want := filepath.Join(cfgDir, "sessions"); c.Storage.baseDir != want {
		t.Fatalf("default baseDir = %q, want %q", c.Storage.baseDir, want)
	}

	// settings.json sessionDir wins over the default.
	settingsDir := filepath.Join(t.TempDir(), "sessions-from-settings")
	settings := `{"sessionDir": "` + settingsDir + `"}`
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	c, err = NewCLI("")
	if err != nil {
		t.Fatalf("NewCLI settings: %v", err)
	}
	if c.Storage.baseDir != settingsDir {
		t.Fatalf("settings baseDir = %q, want %q", c.Storage.baseDir, settingsDir)
	}

	// TAU_SESSION_DIR wins over settings.
	envDir := filepath.Join(t.TempDir(), "sessions-from-env")
	t.Setenv("TAU_SESSION_DIR", envDir)
	c, err = NewCLI("")
	if err != nil {
		t.Fatalf("NewCLI env: %v", err)
	}
	if c.Storage.baseDir != envDir {
		t.Fatalf("env baseDir = %q, want %q", c.Storage.baseDir, envDir)
	}
}

func TestCLICanceledSummariesLeaveHistoryUnchanged(t *testing.T) {
	for _, operation := range []string{"compact", "branch"} {
		t.Run(operation, func(t *testing.T) {
			storage, err := NewStorageAt(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			header := NewHeader("/test", nil, nil)
			writer, err := storage.Create(header)
			if err != nil {
				t.Fatal(err)
			}
			entry := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"hello"}`), nil)
			if err := writer.Append(entry); err != nil {
				t.Fatal(err)
			}
			path := writer.Path()
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := ReleaseOwner(path); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			cli := &CLI{Storage: storage}
			if operation == "compact" {
				err = cli.Compact(ctx, header.ID)
			} else {
				err = cli.SummarizeBranch(ctx, header.ID, entry.ID)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled summary error=%v", err)
			}
			reader, err := ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(reader.Entries()) != 1 {
				t.Fatalf("canceled summary mutated history: %+v", reader.Entries())
			}
		})
	}
}

func TestBranchNaiveSummaryUsesContextContent(t *testing.T) {
	entries := []Entry{
		{Type: EntryTypeMessage, Message: json.RawMessage(`{"role":"assistant","content":[{"type":"thinking","text":"private reasoning"},{"type":"text","text":"visible answer"},{"type":"toolCall","name":"lookup","arguments":{"query":"needle"}}]}`)},
		{Type: EntryTypeMessage, Message: json.RawMessage(`{"role":"user","content":"excluded secret","excludeFromContext":true}`)},
		{Type: EntryTypeCustomMessage, Message: json.RawMessage(`{"role":"user","content":"custom context"}`)},
		NewBranchSummaryEntry("old", "earlier decisions", false),
	}
	got := buildBranchNaiveSummary(entries)
	for _, want := range []string{"visible answer", `lookup({"query":"needle"})`, "custom context", "earlier decisions"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q: %s", want, got)
		}
	}
	for _, secret := range []string{"private reasoning", "excluded secret"} {
		if strings.Contains(got, secret) {
			t.Errorf("summary contains %q: %s", secret, got)
		}
	}
}

func TestCLIBranchSummaryStopsAtSelectedBranch(t *testing.T) {
	storage, err := NewStorageAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	header := NewHeader("/test", nil, nil)
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatal(err)
	}
	root := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"shared baseline"}`), nil)
	abandoned := NewMessageEntry(&root.ID, json.RawMessage(`{"role":"assistant","content":"abandoned work"}`), nil)
	selected := NewMessageEntry(&root.ID, json.RawMessage(`{"role":"assistant","content":"selected work"}`), nil)
	for _, entry := range []Entry{root, abandoned, selected} {
		if err := writer.Append(entry); err != nil {
			t.Fatal(err)
		}
	}
	path := writer.Path()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseOwner(path); err != nil {
		t.Fatal(err)
	}
	cli := &CLI{Storage: storage}
	if err := cli.SummarizeBranch(t.Context(), header.ID, abandoned.ID[:len(abandoned.ID)-1]); err != nil {
		t.Fatal(err)
	}
	reader, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	summaries := reader.EntriesByType(EntryTypeBranchSummary)
	if len(summaries) != 1 || summaries[0].Summary == nil {
		t.Fatalf("summaries=%+v", summaries)
	}
	if summaries[0].FromID == nil || *summaries[0].FromID != abandoned.ID {
		t.Fatalf("summary source = %v, want %s", summaries[0].FromID, abandoned.ID)
	}
	summary := *summaries[0].Summary
	if !strings.Contains(summary, "abandoned work") || strings.Contains(summary, "shared baseline") || strings.Contains(summary, "selected work") {
		t.Fatalf("summary includes shared or selected history: %s", summary)
	}
}

func TestCLIModelSummariesHonorCancellationBeforeClientCreation(t *testing.T) {
	cli := &CLI{model: &ai.Model{API: "unavailable-summary-provider"}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for name, summarize := range map[string]func(context.Context, string) (string, ai.Usage, error){"compaction": cli.completeSummary, "branch": cli.callModelForBranchSummary} {
		t.Run(name, func(t *testing.T) {
			_, _, err := summarize(ctx, "history")
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("expected cancellation, got %v", err)
			}
		})
	}
}

func TestCLIModelSummariesRetainAllTextBlocks(t *testing.T) {
	provider := ai.RegisterFauxProvider(ai.WithFauxAPI("session-summary-text"))
	t.Cleanup(provider.Unregister)
	response := ai.FauxAssistantMessage([]ai.ContentBlock{
		ai.FauxThinking("private reasoning"),
		ai.FauxText("first section\n"),
		ai.FauxText("last section"),
	})
	provider.SetResponseSteps([]ai.FauxResponseStep{response, response})
	cli := &CLI{model: provider.GetModel()}
	for name, summarize := range map[string]func(context.Context, string) (string, ai.Usage, error){"compaction": cli.completeSummary, "branch": cli.callModelForBranchSummary} {
		t.Run(name, func(t *testing.T) {
			got, _, err := summarize(t.Context(), "history")
			if err != nil {
				t.Fatal(err)
			}
			if got != "first section\nlast section" {
				t.Fatalf("summary = %q", got)
			}
		})
	}
}

func TestSummaryResponseTextRejectsMissingText(t *testing.T) {
	for _, message := range []*ai.AssistantMessage{nil, {}, {Content: []ai.ContentBlock{ai.FauxThinking("private")}}} {
		if _, err := summaryResponseText(message); err == nil {
			t.Fatal("expected missing text error")
		}
	}
}

func TestSummaryResponseTextRejectsIncompleteResponses(t *testing.T) {
	for _, reason := range []ai.StopReason{ai.StopReasonAborted, ai.StopReasonError, ai.StopReasonLength} {
		t.Run(string(reason), func(t *testing.T) {
			message := ai.FauxAssistantMessage("partial summary", ai.WithFauxStopReason(reason), ai.WithFauxErrorMessage("provider failure"))
			_, err := summaryResponseText(&message)
			if err == nil {
				t.Fatal("accepted incomplete summary")
			}
			if reason == ai.StopReasonAborted && !errors.Is(err, context.Canceled) {
				t.Fatalf("expected cancellation, got %v", err)
			}
		})
	}
	message := ai.FauxAssistantMessage(" \n\t")
	if _, err := summaryResponseText(&message); err == nil {
		t.Fatal("accepted blank summary")
	}
}

func TestSummaryResponseTextRejectsToolCalls(t *testing.T) {
	for _, reason := range []ai.StopReason{ai.StopReasonStop, ai.StopReasonToolUse} {
		t.Run(string(reason), func(t *testing.T) {
			message := ai.FauxAssistantMessage([]ai.ContentBlock{ai.FauxText("I will inspect the files"), ai.FauxToolCall("read", json.RawMessage(`{"path":"main.go"}`))}, ai.WithFauxStopReason(reason))
			if _, err := summaryResponseText(&message); err == nil {
				t.Fatal("accepted tool request as summary")
			}
		})
	}
}

func TestCLIBranchSummaryRejectsAmbiguousPrefix(t *testing.T) {
	storage, err := NewStorageAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	header := NewHeader("/test", nil, nil)
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"shared-one", "shared-two"} {
		entry := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"history"}`), nil)
		entry.ID = id
		if err := writer.Append(entry); err != nil {
			t.Fatal(err)
		}
	}
	path := writer.Path()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseOwner(path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cli := &CLI{Storage: storage}
	for _, prefix := range []string{"shared-", ""} {
		if err := cli.SummarizeBranch(t.Context(), header.ID, prefix); err == nil {
			t.Fatalf("accepted ambiguous or empty branch prefix %q", prefix)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("ambiguous prefix changed session history")
	}
}

func TestCLIBranchSummarySkipsSelectedHistory(t *testing.T) {
	storage, err := NewStorageAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	header := NewHeader("/test", nil, nil)
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatal(err)
	}
	root := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"shared history"}`), nil)
	selected := NewMessageEntry(&root.ID, json.RawMessage(`{"role":"assistant","content":"selected work"}`), nil)
	for _, entry := range []Entry{root, selected} {
		if err := writer.Append(entry); err != nil {
			t.Fatal(err)
		}
	}
	path := writer.Path()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseOwner(path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cli := &CLI{Storage: storage, model: &ai.Model{API: "unavailable-summary-provider"}}
	if err := cli.SummarizeBranch(t.Context(), header.ID, ""); err == nil {
		t.Fatal("accepted empty branch tip")
	}
	for _, id := range []string{selected.ID, root.ID} {
		if err := cli.SummarizeBranch(t.Context(), header.ID, id); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Fatal("selected history acquired an empty branch summary")
		}
	}
}

func TestCLIBranchSummaryRejectsEmptyTipWithOneEntry(t *testing.T) {
	storage, err := NewStorageAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	header := NewHeader("/test", nil, nil)
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"history"}`), nil)); err != nil {
		t.Fatal(err)
	}
	path := writer.Path()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseOwner(path); err != nil {
		t.Fatal(err)
	}
	cli := &CLI{Storage: storage}
	if err := cli.SummarizeBranch(t.Context(), header.ID, ""); err == nil {
		t.Fatal("accepted empty branch tip")
	}
}

func TestCLIBranchSummaryWithoutConversationContent(t *testing.T) {
	storage, err := NewStorageAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	header := NewHeader("/test", nil, nil)
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatal(err)
	}
	metadata := NewModelChangeEntry("provider", "model")
	selected := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"selected"}`), nil)
	for _, entry := range []Entry{metadata, selected} {
		if err := writer.Append(entry); err != nil {
			t.Fatal(err)
		}
	}
	path := writer.Path()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseOwner(path); err != nil {
		t.Fatal(err)
	}
	cli := &CLI{Storage: storage, model: &ai.Model{API: "unavailable-summary-provider"}}
	if err := cli.SummarizeBranch(t.Context(), header.ID, metadata.ID); err != nil {
		t.Fatal(err)
	}
	reader, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	summaries := reader.EntriesByType(EntryTypeBranchSummary)
	if len(summaries) != 1 || summaries[0].Summary == nil || *summaries[0].Summary != "No content to summarize" {
		t.Fatalf("metadata branch summary = %+v", summaries)
	}
}

func TestCLIForkHonorsCancellation(t *testing.T) {
	storage, err := NewStorageAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	header := NewHeader("/test", nil, nil)
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cli := &CLI{Storage: storage}
	id, err := cli.Fork(ctx, header.ID, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("fork ID=%q error=%v, want cancellation", id, err)
	}
	sessions, err := storage.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("canceled fork left %d sessions, want 1", len(sessions))
	}
}

func TestCLIRestoreHonorsCancellation(t *testing.T) {
	storage, err := NewStorageAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	header := NewHeader("/test", nil, nil)
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatal(err)
	}
	path := writer.Path()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := storage.Delete(header.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cli := &CLI{Storage: storage}
	if err := cli.Restore(ctx, header.ID); !errors.Is(err, context.Canceled) {
		t.Errorf("restore error=%v, want cancellation", err)
	}
	if _, err := os.Stat(filepath.Join(storage.TrashDir(), filepath.Base(path))); err != nil {
		t.Fatalf("canceled restore changed trash: %v", err)
	}
}

func TestCLIMutationsHonorCancellation(t *testing.T) {
	for _, operation := range []string{"delete", "rename", "move cwd", "resume"} {
		t.Run(operation, func(t *testing.T) {
			storage, err := NewStorageAt(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			header := NewHeader("/test", nil, nil)
			writer, err := storage.Create(header)
			if err != nil {
				t.Fatal(err)
			}
			path := writer.Path()
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := ReleaseOwner(path); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			cli := &CLI{Storage: storage}
			switch operation {
			case "delete":
				err = cli.Delete(ctx, header.ID)
			case "rename":
				err = cli.Rename(ctx, header.ID, "changed")
			case "move cwd":
				_, err = cli.MoveCWD(ctx, header.ID, t.TempDir())
			case "resume":
				var resumed *Writer
				resumed, err = cli.Resume(ctx, header.ID)
				if resumed != nil {
					if closeErr := resumed.Close(); closeErr != nil {
						t.Fatal(closeErr)
					}
				}
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("error=%v, want cancellation", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatal("canceled operation changed session")
			}
			if _, err := os.Stat(path + ".owner"); !os.IsNotExist(err) {
				t.Fatalf("canceled operation claimed ownership: %v", err)
			}
		})
	}
}

func TestCLISummariesRecordBilledUsageOnTheirBranch(t *testing.T) {
	for _, operation := range []string{"compact", "branch"} {
		t.Run(operation, func(t *testing.T) {
			provider := ai.RegisterFauxProvider(ai.WithFauxAPI("session-summary-usage-" + operation))
			t.Cleanup(provider.Unregister)
			truncated := ai.FauxAssistantMessage("partial", ai.WithFauxStopReason(ai.StopReasonLength))
			provider.SetResponses([]ai.AssistantMessage{truncated})
			storage, err := NewStorageAt(t.TempDir())
			require.NoError(t, err)
			header := NewHeader("/test", nil, nil)
			writer, err := storage.Create(header)
			require.NoError(t, err)
			root := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"shared baseline"}`), nil)
			abandoned := NewMessageEntry(&root.ID, json.RawMessage(`{"role":"assistant","content":"abandoned work"}`), nil)
			selected := NewMessageEntry(&root.ID, json.RawMessage(`{"role":"assistant","content":"selected work"}`), nil)
			for _, entry := range []Entry{root, abandoned, selected} {
				require.NoError(t, writer.Append(entry))
			}
			path := writer.Path()
			require.NoError(t, writer.Close())
			require.NoError(t, ReleaseOwner(path))
			cli := &CLI{Storage: storage, model: provider.GetModel()}
			if operation == "compact" {
				require.NoError(t, cli.Compact(t.Context(), header.ID))
			} else {
				require.NoError(t, cli.SummarizeBranch(t.Context(), header.ID, abandoned.ID))
			}
			reader, err := ReadFile(path)
			require.NoError(t, err)
			var recorded []Entry
			for _, entry := range reader.EntriesByType(EntryTypeCustom) {
				if entry.CustomType != nil && *entry.CustomType == SummaryUsageType {
					recorded = append(recorded, entry)
				}
			}
			require.Len(t, recorded, 1, "the rejected summary attempt was billed")
			var usage ai.Usage
			require.NoError(t, json.Unmarshal(recorded[0].Data, &usage))
			assert.Positive(t, usage.TotalTokens)
			require.NotNil(t, recorded[0].ParentID)
			assert.Equal(t, selected.ID, *recorded[0].ParentID)
		})
	}
}

func TestCLIModelSummariesRejectInvalidProviderRouting(t *testing.T) {
	for _, invalid := range []string{"image", "classifier", "api mismatch"} {
		t.Run(invalid, func(t *testing.T) {
			provider := ai.RegisterFauxProvider(ai.WithFauxAPI("session-summary-routing"))
			t.Cleanup(provider.Unregister)
			provider.SetResponses([]ai.AssistantMessage{ai.FauxAssistantMessage("must not be called")})
			model := *provider.GetModel()
			if invalid == "api mismatch" {
				model.API = "session-summary-wrong-api"
				ai.Register(model.API, provider)
				t.Cleanup(func() { ai.Unregister(model.API) })
			} else {
				model.Type = invalid
			}
			cli := &CLI{model: &model}
			for name, summarize := range map[string]func(context.Context, string) (string, ai.Usage, error){"compaction": cli.completeSummary, "branch": cli.callModelForBranchSummary} {
				t.Run(name, func(t *testing.T) {
					_, _, err := summarize(t.Context(), "history")
					require.ErrorContains(t, err, invalid)
					assert.Zero(t, provider.CallCount())
				})
			}
		})
	}
}
