package tui

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/losinggeneration/tau/tui/clipboard"
)

func TestSavePastedImageMarkerResolvesAtSend(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "session-data")
	cwd := t.TempDir()
	a := newTestApp(t)
	a.sess.(*fakeSession).stateCWD = cwd
	a.sess.(*fakeSession).stateDataDir = dataDir

	png, err := base64.StdEncoding.DecodeString(testPNGBase64)
	if err != nil {
		t.Fatal(err)
	}
	marker, ok := a.rewriteClipboardImage(clipboard.Image{MIME: "image/png", Data: png})
	if !ok {
		t.Fatal("rewriteClipboardImage failed")
	}

	// The marker must be usable as an @reference in a submitted prompt:
	// expansion resolves it against the prompt root (cwd), so a bare
	// filename that only exists in the hidden session data dir sends as
	// literal text with no image attached.
	_, _, attachmentIDs, err := a.expandInlineAttachmentReferences("describe " + marker)
	if err != nil {
		t.Fatal(err)
	}
	if len(attachmentIDs) != 1 {
		t.Fatalf("pasted image marker %q did not resolve to an attachment; got ids %v", marker, attachmentIDs)
	}
}

func TestSavePastedImageUsesSessionDataDir(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "session-data")
	a := newTestApp(t)
	a.sess.(*fakeSession).stateCWD = t.TempDir()
	a.sess.(*fakeSession).stateDataDir = dataDir

	marker, err := a.savePastedImage(clipboard.Image{MIME: "image/png", Data: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	if marker != "pasted-image.png" {
		t.Fatalf("marker = %q, want bare pasted-image.png", marker)
	}
	if _, err := os.Stat(filepath.Join(dataDir, marker)); err != nil {
		t.Fatalf("image not saved in session data dir: %v", err)
	}
	full := a.resolveAttachmentPath(marker, a.fileSelectorRoot())
	if _, err := os.Stat(full); err != nil {
		t.Fatalf("marker %q unresolvable at send time: %v", marker, err)
	}
}

// A working-tree file that happens to share the pasted image's name must
// not shadow the image the user actually pasted: minted paste markers
// resolve against the session data dir before the prompt root.
func TestPastedImageMarkerNotShadowedByWorkingTreeFile(t *testing.T) {
	cwd := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "session-data")
	a := newTestApp(t)
	fs := a.sess.(*fakeSession)
	fs.stateCWD = cwd
	fs.stateDataDir = dataDir
	decoy := []byte("working-tree file with the same name")
	if err := os.WriteFile(filepath.Join(cwd, "pasted-image.png"), decoy, 0o644); err != nil {
		t.Fatal(err)
	}

	png, err := base64.StdEncoding.DecodeString(testPNGBase64)
	if err != nil {
		t.Fatal(err)
	}
	marker, ok := a.rewriteClipboardImage(clipboard.Image{MIME: "image/png", Data: png})
	if !ok {
		t.Fatal("rewriteClipboardImage failed")
	}
	if marker != "@pasted-image.png" {
		t.Fatalf("marker = %q, want @pasted-image.png", marker)
	}

	_, _, ids, err := a.expandInlineAttachmentReferences("describe " + marker)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("attachment ids = %v, want the pasted image", ids)
	}
	record, ok := fs.attachments[ids[0]]
	if !ok {
		t.Fatalf("attachment %q not imported", ids[0])
	}
	stored, err := os.ReadFile(record.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(png) {
		t.Fatal("imported attachment is the working-tree decoy, not the pasted image")
	}
}
