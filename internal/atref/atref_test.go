package atref

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/losinggeneration/tau/sdk"
)

func TestReplaceFindsTokensAtBoundaries(t *testing.T) {
	var seen []string
	out, err := Replace(`see @a.go, (@b/c) mail@x.com \@d @ @e`, func(path string) (string, error) {
		seen = append(seen, path)
		return "<" + path + ">", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.go", "b/c)", "e"}; !slices.Equal(seen, want) {
		t.Fatalf("tokens = %q, want %q", seen, want)
	}
	if want := `see <a.go>, (<b/c)> mail@x.com \@d @ <e>`; out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

func TestReplaceStopsOnError(t *testing.T) {
	boom := errors.New("boom")
	if _, err := Replace("@x", func(string) (string, error) { return "", boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
}

func TestResolve(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, root, want string }{
		{"a/b", "/root", filepath.Join("/root", "a", "b")},
		{"../x", "/root/sub", "/root/x"},
		{"/etc/hosts", "/root", "/etc/hosts"},
		{"~/notes", "/root", filepath.Join(home, "notes")},
		{"a", "", "a"},
		{"", "/root", "/root"},
	} {
		if got := Resolve(tc.path, tc.root); got != tc.want {
			t.Errorf("Resolve(%q, %q) = %q, want %q", tc.path, tc.root, got, tc.want)
		}
	}
}

func TestAttachImportsFilesAndNamesDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	store := sdk.NewAttachmentStore(t.TempDir())
	var imported []string
	importFile := func(full string) (sdk.Attachment, error) {
		imported = append(imported, full)
		return store.ImportFile(full)
	}
	resolve := func(path string) string { return Resolve(path, root) }

	out, records, err := Attach("read @note.txt in @dir but not @missing.go", resolve, importFile)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(imported, []string{filepath.Join(root, "note.txt")}) {
		t.Fatalf("imported %q", imported)
	}
	if len(records) != 1 || records[0].ExtractedText != "hello" {
		t.Fatalf("records = %+v", records)
	}
	want := `read <file name="note.txt">[attached]</file> in <directory name="` + filepath.Join(root, "dir") + `">` + "\n</directory> but not @missing.go"
	if out != want {
		t.Fatalf("out = %q\nwant %q", out, want)
	}
}

func TestAttachMarksImages(t *testing.T) {
	out, records, err := Attach("@shot.png", func(string) string { return writeFile(t, "shot.png") }, func(string) (sdk.Attachment, error) {
		return sdk.Attachment{ID: "img", Kind: "image", MimeType: "image/png"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != `<file name="shot.png">[image content]</file>` || len(records) != 1 || records[0].ID != "img" {
		t.Fatalf("out = %q, records = %+v", out, records)
	}
}

func TestAttachRejectsUnsupportedFiles(t *testing.T) {
	_, _, err := Attach("@blob.bin", func(string) string { return writeFile(t, "blob.bin") }, func(string) (sdk.Attachment, error) {
		return sdk.Attachment{ID: "b", Kind: "document", MimeType: "application/octet-stream"}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported attachment type application/octet-stream") {
		t.Fatalf("err = %v", err)
	}
}

func writeFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
