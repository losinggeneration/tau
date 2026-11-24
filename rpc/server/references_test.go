package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/losinggeneration/tau/sdk"
)

func referenceWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("remember"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func runReferenceCommand(t *testing.T, s *Server, output *bytes.Buffer, typ, args string) Response {
	t.Helper()
	output.Reset()
	if err := s.handleCommand(t.Context(), &Command{ID: "c", Type: typ, Args: json.RawMessage(args)}); err != nil {
		t.Fatal(err)
	}
	s.promptWG.Wait()
	var reply Response
	if err := json.Unmarshal(lastResponseLine(output.Bytes()), &reply); err != nil {
		t.Fatalf("decode %s: %v", output, err)
	}
	return reply
}

func TestMessagesExpandReferencesInWorkerCWD(t *testing.T) {
	root := referenceWorkspace(t)
	const text = "read @notes.md near @src and @missing.go"
	want := `read <file name="notes.md">[attached]</file> near <directory name="` + filepath.Join(root, "src") + `">` + "\n</directory> and @missing.go"
	for _, tc := range []struct {
		typ  string
		sent func(*mockSession) []sdk.Message
	}{
		{"send_message", func(m *mockSession) []sdk.Message { return m.sent }},
		{"follow_up", func(m *mockSession) []sdk.Message { return m.followUps }},
		{"steer", func(m *mockSession) []sdk.Message { return m.steers }},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			sess := newMockSession("s", root)
			var output bytes.Buffer
			s := &Server{writer: &output, session: sess, coordinated: true}
			reply := runReferenceCommand(t, s, &output, tc.typ, `{"text":"`+text+`","expand_refs":true,"expected_session_id":"s"}`)
			msgs := tc.sent(sess)
			if !reply.OK || len(msgs) != 1 {
				t.Fatalf("reply = %s, messages = %+v", &output, msgs)
			}
			if msgs[0].Text != want {
				t.Fatalf("text = %q\nwant %q", msgs[0].Text, want)
			}
			if len(msgs[0].AttachmentIDs) != 1 || sess.attachments[msgs[0].AttachmentIDs[0]].ExtractedText != "remember" {
				t.Fatalf("attachments = %v, store = %+v", msgs[0].AttachmentIDs, sess.attachments)
			}
		})
	}
}

func TestMessagesKeepReferencesWithoutExpandRefs(t *testing.T) {
	root := referenceWorkspace(t)
	sess := newMockSession("s", root)
	var output bytes.Buffer
	s := &Server{writer: &output, session: sess, coordinated: true}
	if reply := runReferenceCommand(t, s, &output, "send_message", `{"text":"read @notes.md"}`); !reply.OK {
		t.Fatalf("reply = %s", &output)
	}
	if len(sess.sent) != 1 || sess.sent[0].Text != "read @notes.md" || len(sess.attachments) != 0 {
		t.Fatalf("sent = %+v, attachments = %+v", sess.sent, sess.attachments)
	}
}

func TestExpandRefsRejectsUnsupportedFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "blob.bin"), []byte{0, 1, 2, 0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	sess := newMockSession("s", root)
	var output bytes.Buffer
	s := &Server{writer: &output, session: sess, coordinated: true}
	reply := runReferenceCommand(t, s, &output, "send_message", `{"text":"see @blob.bin","expand_refs":true}`)
	if reply.OK || len(sess.sent) != 0 {
		t.Fatalf("reply = %s, sent = %+v", &output, sess.sent)
	}
}

func TestCompletePathSuggestsFromWorkerCWD(t *testing.T) {
	root := referenceWorkspace(t)
	sess := newMockSession("s", root)
	var output bytes.Buffer
	s := &Server{writer: &output, session: sess, coordinated: true}

	reply := runReferenceCommand(t, s, &output, "complete_path", `{"text":"look at @no","cursor":11}`)
	var got completePathResult
	requireOK(t, reply)
	decodeResult(t, reply, &got)
	if got.Prefix != "@no" || !slices.ContainsFunc(got.Items, func(item completePathItem) bool { return item.Value == "@notes.md" }) {
		t.Fatalf("result = %+v", got)
	}

	reply = runReferenceCommand(t, s, &output, "complete_path", `{"text":"plain words","cursor":5}`)
	got = completePathResult{}
	requireOK(t, reply)
	decodeResult(t, reply, &got)
	if got.Prefix != "" || len(got.Items) != 0 {
		t.Fatalf("plain text result = %+v", got)
	}
}

func TestCompletePathUsesProjectCWDWithoutSession(t *testing.T) {
	root := referenceWorkspace(t)
	var output bytes.Buffer
	s := &Server{writer: &output, projectCWD: root, coordinated: true}
	reply := runReferenceCommand(t, s, &output, "complete_path", `{"text":"@sr","cursor":99}`)
	var got completePathResult
	requireOK(t, reply)
	decodeResult(t, reply, &got)
	if len(got.Items) == 0 || got.Items[0].Value != "@src/" {
		t.Fatalf("result = %+v", got)
	}
}

func requireOK(t *testing.T, reply Response) {
	t.Helper()
	if !reply.OK {
		t.Fatalf("reply failed: %+v", reply.Error)
	}
}
