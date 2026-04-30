package tui

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/losinggeneration/tau/agent/session"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/sdk"
	sdkpkg "github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
)

func TestPopulateChatFromSessionLoadsMessages(t *testing.T) {
	sess := &fakeSession{messages: []sdkpkg.Message{
		{Role: "user", Text: "hello"},
		{Role: "assistant", Text: "hi there"},
		{Role: "system", Text: "note"},
	}}
	a := &App{conv: NewConversationModel()}

	a.populateChatFromSession(sess)

	got := a.conv.Messages()
	if len(got) != 3 {
		t.Fatalf("messages len = %d, want 3", len(got))
	}
	wantRoles := []Role{RoleUser, RoleAssistant, RoleSystem}
	wantTexts := []string{"hello", "hi there", "note"}
	for i := range got {
		if got[i].Role != wantRoles[i] || got[i].Text != wantTexts[i] || got[i].Streaming {
			t.Fatalf("message[%d] = %#v, want role=%q text=%q non-streaming", i, got[i], wantRoles[i], wantTexts[i])
		}
	}
}

func TestPopulateChatFromSessionFormatsStructuredMessages(t *testing.T) {
	sess := &fakeSession{messages: []sdkpkg.Message{
		{Role: "user", Text: "run ls"},
		{Role: "assistant", Content: []ai.ContentBlock{
			{Type: ai.ContentBlockText, Text: "I'll check.\n"},
			{Type: ai.ContentBlockToolCall, Name: "bash", Arguments: json.RawMessage(`{"command":"ls -la"}`)},
		}},
		{Role: "tool", Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "file-a\nfile-b"}}, ToolName: "bash"},
		{Role: "assistant", Text: "done"},
	}}
	a := &App{conv: NewConversationModel()}

	a.populateChatFromSession(sess)

	got := a.conv.Messages()
	if len(got) != 4 {
		t.Fatalf("messages len = %d, want 4: %#v", len(got), got)
	}
	checks := []struct {
		idx      int
		role     Role
		text     string
		toolName string
		toolArgs string
	}{
		{idx: 0, role: RoleUser, text: "run ls"},
		{idx: 1, role: RoleAssistant, text: "I'll check."},
		{idx: 2, role: RoleToolCall, text: "bash {\"command\":\"ls -la\"}\nfile-a\nfile-b", toolName: "bash", toolArgs: `{"command":"ls -la"}`},
		{idx: 3, role: RoleAssistant, text: "done"},
	}
	for _, check := range checks {
		msg := got[check.idx]
		if msg.Role != check.role || msg.Text != check.text || msg.ToolName != check.toolName || msg.ToolArgs != check.toolArgs {
			t.Fatalf("message[%d] = %#v, want role=%q text=%q toolName=%q toolArgs=%q", check.idx, msg, check.role, check.text, check.toolName, check.toolArgs)
		}
	}
}

func TestPopulateChatFromSessionDoesNotRestoreThinkingAsActiveStream(t *testing.T) {
	sess := &fakeSession{messages: []sdkpkg.Message{
		{Role: "assistant", Content: []ai.ContentBlock{
			{Type: ai.ContentBlockThinking, Thinking: "private chain of thought"},
			{Type: ai.ContentBlockText, Text: "visible answer"},
		}},
	}}
	a := &App{conv: NewConversationModel()}

	a.populateChatFromSession(sess)

	got := a.conv.Messages()
	if len(got) != 1 {
		t.Fatalf("messages len = %d, want 1: %#v", len(got), got)
	}
	if got[0].Role != RoleAssistant || got[0].Text != "visible answer" || got[0].Streaming {
		t.Fatalf("message[0] = %#v, want committed assistant visible text", got[0])
	}
	for i, msg := range got {
		if msg.Role == RoleThinking || msg.Streaming {
			t.Fatalf("message[%d] = %#v, historical thinking must not restore as active streaming UI", i, msg)
		}
	}
}

// TestSeedChatFromSessionLoadsWholeTranscript pins that resuming installs the
// entire transcript. This replaced a tail-plus-lazy-paging scheme whose sizing
// depended on predicting rendered heights; the prediction drifted from the
// renderer and the cost it deferred turned out to be milliseconds.
func TestSeedChatFromSessionLoadsWholeTranscript(t *testing.T) {
	messages := make([]sdkpkg.Message, 250)
	for i := range messages {
		messages[i] = sdkpkg.Message{Role: "assistant", Text: strings.Repeat("line ", 40)}
	}
	a := &App{conv: NewConversationModel()}

	a.seedChatFromSession(&fakeSession{messages: messages}, "")

	if got, want := len(a.conv.Messages()), len(messages); got != want {
		t.Fatalf("loaded %d messages, want the whole transcript (%d)", got, want)
	}
}

func TestSeedChatFromSessionAppendsNotice(t *testing.T) {
	messages := []sdkpkg.Message{{Role: "user", Text: "hello"}, {Role: "assistant", Text: "hi"}}
	a := &App{conv: NewConversationModel()}

	a.seedChatFromSession(&fakeSession{messages: messages}, "Resumed session abc")

	got := a.conv.Messages()
	if len(got) != 3 {
		t.Fatalf("messages = %d, want 2 transcript + 1 notice", len(got))
	}
	if last := got[len(got)-1]; last.Role != RoleSystem || last.Text != "Resumed session abc" {
		t.Fatalf("last message = %#v, want the resume notice", last)
	}
}

func TestSeedChatFromSessionPopulatesEditorHistory(t *testing.T) {
	messages := []sdkpkg.Message{
		{Role: "system", Text: "setup"},
		{Role: "user", Text: "first prompt"},
		{Role: "assistant", Text: "answer"},
		{Role: "user", Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "second"}, {Type: ai.ContentBlockImage, Name: "ignored.png"}, {Type: ai.ContentBlockText, Text: " prompt\n"}}},
	}
	a := &App{conv: NewConversationModel(), editor: newPromptEditorForTest()}

	a.seedChatFromSession(&fakeSession{messages: messages}, "")

	a.editor.Model().HistoryPrev()
	if got := a.editor.Text(); got != "second prompt" {
		t.Fatalf("latest history = %q, want structured user text", got)
	}
	a.editor.Model().HistoryPrev()
	if got := a.editor.Text(); got != "first prompt" {
		t.Fatalf("previous history = %q, want first user prompt", got)
	}
}

func TestCmdResumeLoadsOnlyActiveBranchMessagesIntoChatlog(t *testing.T) {
	dir := t.TempDir()
	storage, err := session.NewStorageAt(dir)
	if err != nil {
		t.Fatalf("NewStorageAt: %v", err)
	}
	header := session.NewHeader("/test", nil, nil)
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	root := session.NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"root"}`), nil)
	if err := writer.Append(root); err != nil {
		t.Fatalf("Append root: %v", err)
	}
	shared := session.NewMessageEntry(&root.ID, json.RawMessage(`{"role":"assistant","content":"shared"}`), nil)
	if err := writer.Append(shared); err != nil {
		t.Fatalf("Append shared: %v", err)
	}
	abandoned := session.NewMessageEntry(&shared.ID, json.RawMessage(`{"role":"user","content":"abandoned branch"}`), nil)
	if err := writer.Append(abandoned); err != nil {
		t.Fatalf("Append abandoned: %v", err)
	}
	positionChange := session.NewPositionChangeEntry(shared.ID)
	if err := writer.Append(positionChange); err != nil {
		t.Fatalf("Append position change: %v", err)
	}
	active := session.NewMessageEntry(&shared.ID, json.RawMessage(`{"role":"user","content":"active branch"}`), nil)
	if err := writer.Append(active); err != nil {
		t.Fatalf("Append active: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	runtime, err := sdkpkg.NewRuntime(sdkpkg.RuntimeOptions{SessionsDir: dir})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	newSess, err := runtime.NewSession(context.Background(), sdkpkg.SessionOptions{SessionID: header.ID, CWD: "/test"})
	if err != nil {
		t.Fatalf("NewSession resume: %v", err)
	}
	t.Cleanup(func() {
		if err := newSess.Close(context.Background()); err != nil {
			t.Fatalf("Close resumed session: %v", err)
		}
	})

	a := &App{
		conv:      NewConversationModel(),
		sess:      &fakeSession{stateID: "old"},
		cancelRun: func() {},
		sessionFactory: func(_ context.Context, opts sdkpkg.SessionOptions) (sdkpkg.Session, error) {
			if opts.SessionID != header.ID {
				t.Fatalf("SessionID = %q, want %q", opts.SessionID, header.ID)
			}
			return newSess, nil
		},
	}

	cmdResume(a, header.ID)

	got := a.conv.Messages()
	var texts []string
	for _, msg := range got {
		texts = append(texts, msg.Text)
	}
	for _, want := range []string{"root", "shared", "active branch", "Resumed session " + header.ID} {
		if !containsString(texts, want) {
			t.Fatalf("resumed messages %q missing active branch text %q", texts, want)
		}
	}
	if containsString(texts, "abandoned branch") {
		t.Fatalf("resumed messages include abandoned branch: %q", texts)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestResumeSelectorItemsSortedNewestFirst(t *testing.T) {
	a := newAppWithSelector(t)
	a.sessionLister = func(context.Context) ([]sdk.SessionInfo, error) {
		return []sdk.SessionInfo{
			{ID: "old-session", Name: "old", Created: "2024-01-01T00:00:00Z"},
			{ID: "new-session", Name: "new", Created: "2024-03-01T00:00:00Z"},
			{ID: "mid-session", Name: "mid", Created: "2024-02-01T00:00:00Z"},
		}, nil
	}

	a.refreshSelector("/resume ", nil)
	if !a.selectorVisible() {
		t.Fatal("resume selector not visible")
	}
	if a.selectorMode != selectorModeSessions {
		t.Fatalf("selector mode = %d, want sessions", a.selectorMode)
	}

	items := a.selector.Items()
	got := make([]string, 0, len(items))
	for _, item := range items {
		got = append(got, item.Value)
	}
	want := []string{"new-session", "mid-session", "old-session"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("session order = %#v, want %#v", got, want)
	}
}

func TestResumeSelectorSelectionFillsAndCommitsSessionID(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()

	var resumed string
	a.sessionFactory = func(_ context.Context, opts sdk.SessionOptions) (sdk.Session, error) {
		resumed = opts.SessionID
		return &fakeSession{stateID: opts.SessionID}, nil
	}
	a.sessionLister = func(context.Context) ([]sdk.SessionInfo, error) {
		return []sdk.SessionInfo{{ID: "session-123", Name: "picked", Created: "2024-01-01T00:00:00Z"}}, nil
	}

	a.refreshSelector("/resume ", stubCtx())
	a.applySelector(0, tauui.SelectorItem{Name: "session-123"})
	if got := a.editor.Text(); got != "/resume session-123" {
		t.Fatalf("editor text after selecting session = %q", got)
	}

	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	if !a.trySubmit(ctx) {
		t.Fatal("trySubmit returned false")
	}
	if resumed != "session-123" {
		t.Fatalf("resumed session = %q, want session-123", resumed)
	}
	if got := a.editor.Text(); got != "" {
		t.Fatalf("editor text after commit = %q, want empty", got)
	}
}
