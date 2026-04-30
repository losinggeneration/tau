package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/clipboard"
)

func TestSavePastedImageUsesSessionDataDir(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "session-data")
	a := newTestApp(t)
	a.sess.(*fakeSession).stateCWD = t.TempDir()
	a.sess.(*fakeSession).stateDataDir = dataDir

	marker, err := a.savePastedImage(clipboard.Image{MIME: "image/png", Data: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(marker, "session-data") {
		t.Fatalf("marker = %q, want cwd-relative fallback for hidden data dir", marker)
	}
	if _, err := os.Stat(filepath.Join(dataDir, marker)); err != nil {
		t.Fatalf("image not saved in session data dir: %v", err)
	}
}

func TestQuitRemovesUnsentSessionImages(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "session-data")
	a := newTestApp(t)
	a.app = &tauui.App{}
	a.sess.(*fakeSession).stateDataDir = dataDir
	a.cancelRun = func() {}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "pasted-image.png"), []byte{1}, 0o644); err != nil {
		t.Fatal(err)
	}

	a.quit()

	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("data dir still exists after quitting unsent session: %v", err)
	}
}
