package sdk

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttachmentStoreImportFileStoresBytes(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	store := NewAttachmentStore(filepath.Join(dir, "attachments"))
	record, err := store.ImportFile(src)
	if err != nil {
		t.Fatalf("import file: %v", err)
	}
	if record.Kind != "document" || record.Filename != "note.txt" || record.ExtractedText != "hello" {
		t.Fatalf("record = %#v", record)
	}
	data, err := os.ReadFile(record.Path)
	if err != nil {
		t.Fatalf("read stored file: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("stored = %q, want hello", data)
	}
}

func TestAttachmentStoreImportDataAndContentBlocks(t *testing.T) {
	store := NewAttachmentStore(t.TempDir())
	record, err := store.ImportData("note.txt", "text/plain", base64.StdEncoding.EncodeToString([]byte("body")))
	if err != nil {
		t.Fatalf("import data: %v", err)
	}
	blocks, err := store.ContentBlocks([]string{record.ID})
	if err != nil {
		t.Fatalf("content blocks: %v", err)
	}
	if len(blocks) != 1 || !strings.Contains(blocks[0].Text, "body") {
		t.Fatalf("blocks = %#v", blocks)
	}
}

func TestAttachmentStoreImportImageWritesPreview(t *testing.T) {
	store := NewAttachmentStore(t.TempDir())
	record, err := store.ImportData("image.png", "image/png", base64.StdEncoding.EncodeToString(testPNG(t, 512, 128)))
	if err != nil {
		t.Fatalf("import image: %v", err)
	}
	if record.Preview == "" {
		t.Fatal("preview path is empty")
	}
	f, err := os.Open(record.Preview)
	if err != nil {
		t.Fatalf("open preview: %v", err)
	}
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close preview: %v", err)
	}
	if cfg.Width > attachmentPreviewMax || cfg.Height > attachmentPreviewMax {
		t.Fatalf("preview size = %dx%d, exceeds %d", cfg.Width, cfg.Height, attachmentPreviewMax)
	}
}

func TestAttachmentStoreRejectsBadBase64(t *testing.T) {
	_, err := NewAttachmentStore(t.TempDir()).ImportData("bad.txt", "text/plain", "not base64")
	if err == nil {
		t.Fatal("ImportData succeeded with bad base64")
	}
}

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestAttachmentStoreReloadsRecords(t *testing.T) {
	dir := t.TempDir()
	store := NewAttachmentStore(dir)
	record, err := store.ImportData("note.txt", "text/plain", base64.StdEncoding.EncodeToString([]byte("body")))
	if err != nil {
		t.Fatalf("import data: %v", err)
	}

	reloaded := NewAttachmentStore(dir)
	blocks, err := reloaded.ContentBlocks([]string{record.ID})
	if err != nil {
		t.Fatalf("content blocks after reload: %v", err)
	}
	if len(blocks) != 1 || !strings.Contains(blocks[0].Text, "body") {
		t.Fatalf("blocks = %#v", blocks)
	}
}
