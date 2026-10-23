package clipboard

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestParseDataURLAndSave(t *testing.T) {
	data := []byte{1, 2, 3}
	url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
	img, ok := ParseDataURL(url)
	if !ok || img.MIME != "image/png" || len(img.Data) != 3 {
		t.Fatalf("img=%+v ok=%v", img, ok)
	}
	path, err := SaveImage(t.TempDir(), img)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("got=%v", got)
	}
	if AttachmentMarkdown(path) != "@"+path {
		t.Fatalf("attachment")
	}
}

func TestSaveImageUsesUniqueNames(t *testing.T) {
	dir := t.TempDir()
	img := Image{MIME: "image/png", Data: []byte{1}}
	first, err := SaveImage(dir, img)
	if err != nil {
		t.Fatal(err)
	}
	second, err := SaveImage(dir, img)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("SaveImage reused path %q", first)
	}
	if got := filepath.Base(second); got != "pasted-image-2.png" {
		t.Fatalf("second pasted image name = %q", got)
	}
}

func TestSaveImageCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session-data")
	path, err := SaveImage(dir, Image{MIME: "image/png", Data: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("path dir = %q, want %q", filepath.Dir(path), dir)
	}
}

func TestParseDataURLRejectsInvalid(t *testing.T) {
	if _, ok := ParseDataURL("nope"); ok {
		t.Fatal("accepted")
	}
	if _, ok := ParseDataURL("data:image/png,abc"); ok {
		t.Fatal("accepted nonbase64")
	}
}

func TestReadImageNoHelper(t *testing.T) {
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", "")
	img, ok, err := ReadImage(context.Background())
	t.Setenv("PATH", oldPath)
	if err != nil {
		t.Fatalf("ReadImage without helpers should not error: %v", err)
	}
	if ok || img.MIME != "" || len(img.Data) != 0 {
		t.Fatalf("ReadImage = (%+v, %v), want empty false", img, ok)
	}
}

func TestFirstImageMIME(t *testing.T) {
	if got := firstImageMIME("text/plain\nimage/jpeg\nimage/png"); got != "image/png" {
		t.Fatalf("firstImageMIME preferred %q, want image/png", got)
	}
	if got := firstImageMIME("text/plain"); got != "" {
		t.Fatalf("firstImageMIME = %q, want empty", got)
	}
}

func TestSaveImageRejectsEscapingFilename(t *testing.T) {
	for _, name := range []string{"../escape.png", "sub/escape.png", "..", "/tmp/escape.png", `sub\escape.png`} {
		if _, err := SaveImage(t.TempDir(), Image{MIME: "image/png", Data: []byte{1}, Filename: name}); err == nil {
			t.Errorf("accepted filename %q", name)
		}
	}
}

func TestSaveImageConcurrentWritesDoNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	const count = 32
	start := make(chan struct{})
	paths := make(chan string, count)
	var workers sync.WaitGroup
	for i := range count {
		workers.Go(func() {
			<-start
			path, err := SaveImage(dir, Image{Filename: "image.png", Data: []byte{byte(i)}})
			if err != nil {
				t.Error(err)
				return
			}
			paths <- path
		})
	}
	close(start)
	workers.Wait()
	close(paths)
	seen := map[string]bool{}
	for path := range paths {
		if seen[path] {
			t.Errorf("reused image path %q", path)
		}
		seen[path] = true
	}
	if len(seen) != count {
		t.Fatalf("unique files=%d, want %d", len(seen), count)
	}
}
