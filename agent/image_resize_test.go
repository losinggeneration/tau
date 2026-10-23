package agent

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/losinggeneration/tau/ai"
)

// makeTestPNG builds an in-memory PNG of the given dimensions filled with
// a solid color. Useful for testing resize without binary fixtures.
func makeTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 128, G: 64, B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

// makeTestJPEG builds an in-memory JPEG of the given dimensions.
func makeTestJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 50, G: 100, B: 150, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

func TestResizeImageData_SmallImage_NoOp(t *testing.T) {
	// 100x100 PNG is below the 200x200 max — no resize, no note.
	data := makeTestPNG(t, 100, 100)
	r := resizeImageData(data, "image/png", 200, 200)
	if r.Data != nil {
		t.Errorf("Data should be nil for already-small image, got %d bytes", len(r.Data))
	}
	if r.OrigW != 100 || r.OrigH != 100 {
		t.Errorf("OrigW/OrigH = %d/%d, want 100/100", r.OrigW, r.OrigH)
	}
	if r.NewW != 100 || r.NewH != 100 {
		t.Errorf("NewW/NewH = %d/%d, want 100/100 (unchanged)", r.NewW, r.NewH)
	}
	if r.Note != "" {
		t.Errorf("Note = %q, want empty for no-op", r.Note)
	}
}

func TestResizeImageData_LargePNG_Resized(t *testing.T) {
	data := makeTestPNG(t, 4000, 3000)
	r := resizeImageData(data, "image/png", 2000, 2000)
	if r.Data == nil {
		t.Fatal("Data should be non-nil for large image")
	}
	if r.OrigW != 4000 || r.OrigH != 3000 {
		t.Errorf("OrigW/OrigH = %d/%d, want 4000/3000", r.OrigW, r.OrigH)
	}
	if r.NewW > 2000 || r.NewH > 2000 {
		t.Errorf("NewW/NewH = %d/%d, want <= 2000 in both dims", r.NewW, r.NewH)
	}
	if r.NewW == r.OrigW || r.NewH == r.OrigH {
		t.Error("New dimensions should differ from original for a large image")
	}
	if !strings.Contains(r.Note, "resized from 4000x3000") {
		t.Errorf("Note = %q, want substring 'resized from 4000x3000'", r.Note)
	}

	// Re-decode the result and verify the dimensions match NewW/NewH.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(r.Data))
	if err != nil {
		t.Fatalf("re-decode image: %v", err)
	}
	if cfg.Width != r.NewW || cfg.Height != r.NewH {
		t.Errorf("re-decoded %dx%d, want %dx%d", cfg.Width, cfg.Height, r.NewW, r.NewH)
	}
}

func TestResizeImageData_LargeJPEG_Resized(t *testing.T) {
	data := makeTestJPEG(t, 3840, 2160)
	r := resizeImageData(data, "image/jpeg", 2000, 2000)
	if r.Data == nil {
		t.Fatal("Data should be non-nil for large image")
	}
	if r.OrigW != 3840 || r.OrigH != 2160 {
		t.Errorf("OrigW/OrigH = %d/%d, want 3840/2160", r.OrigW, r.OrigH)
	}
	if r.NewW > 2000 || r.NewH > 2000 {
		t.Errorf("NewW/NewH = %d/%d, want <= 2000 in both dims", r.NewW, r.NewH)
	}

	// Verify the re-encoded data is still a valid JPEG.
	cfg, format, err := image.DecodeConfig(bytes.NewReader(r.Data))
	if err != nil {
		t.Fatalf("re-decode jpeg: %v", err)
	}
	if format != "jpeg" {
		t.Errorf("re-encoded format = %q, want jpeg", format)
	}
	if cfg.Width != r.NewW || cfg.Height != r.NewH {
		t.Errorf("re-decoded %dx%d, want %dx%d", cfg.Width, cfg.Height, r.NewW, r.NewH)
	}
}

func TestResizeImageData_UnsupportedFormat(t *testing.T) {
	// WebP is unsupported by the stdlib; resize should return zero value.
	r := resizeImageData([]byte("not really webp"), "image/webp", 200, 200)
	if r.Data != nil {
		t.Error("Data should be nil for unsupported mime type")
	}
	if r.OrigW != 0 || r.OrigH != 0 {
		t.Errorf("OrigW/OrigH = %d/%d, want 0/0 (cannot decode webp)", r.OrigW, r.OrigH)
	}
	if r.Note != "" {
		t.Errorf("Note = %q, want empty for unsupported format", r.Note)
	}
}

func TestResizeImageData_CorruptData(t *testing.T) {
	// Random bytes that don't decode as a valid image.
	r := resizeImageData([]byte("not a real image"), "image/png", 200, 200)
	if r.Data != nil {
		t.Error("Data should be nil for corrupt input")
	}
	if r.Note != "" {
		t.Errorf("Note = %q, want empty for corrupt input", r.Note)
	}
}

func TestResizeImageData_PreservesAspectRatio(t *testing.T) {
	// 4000x1000 (4:1) resized to 2000x2000 max should keep 4:1 aspect.
	data := makeTestPNG(t, 4000, 1000)
	r := resizeImageData(data, "image/png", 2000, 2000)
	if r.Data == nil {
		t.Fatal("Data should be non-nil")
	}
	// Width-constrained: 4000 -> 2000, height scales proportionally: 1000 -> 500.
	if r.NewW != 2000 || r.NewH != 500 {
		t.Errorf("NewW/NewH = %d/%d, want 2000/500 (4:1 aspect preserved)", r.NewW, r.NewH)
	}
}

func TestResizeImageData_ZeroMaxDimensions(t *testing.T) {
	data := makeTestPNG(t, 100, 100)
	r := resizeImageData(data, "image/png", 0, 0)
	if r.Data != nil {
		t.Error("Data should be nil when max dimensions are zero/negative")
	}
}

func TestComputeFitDimensions(t *testing.T) {
	tests := []struct {
		name         string
		srcW, srcH   int
		maxW, maxH   int
		wantW, wantH int
	}{
		{"no scaling needed", 100, 100, 200, 200, 100, 100},
		{"width-constrained", 4000, 1000, 2000, 2000, 2000, 500},
		{"height-constrained", 1000, 4000, 2000, 2000, 500, 2000},
		{"both constrained equally", 4000, 2000, 2000, 1000, 2000, 1000},
		{"minimum size 1px", 1, 1, 100, 100, 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotW, gotH := computeFitDimensions(tt.srcW, tt.srcH, tt.maxW, tt.maxH)
			if gotW != tt.wantW || gotH != tt.wantH {
				t.Errorf("got %dx%d, want %dx%d", gotW, gotH, tt.wantW, tt.wantH)
			}
		})
	}
}

func TestIsSupportedImageMime(t *testing.T) {
	supported := []string{"image/png", "image/jpeg", "image/gif", "IMAGE/PNG"}
	unsupported := []string{"image/webp", "image/bmp", "image/tiff", "text/plain", ""}
	for _, m := range supported {
		if !isSupportedImageMime(m) {
			t.Errorf("isSupportedImageMime(%q) = false, want true", m)
		}
	}
	for _, m := range unsupported {
		if isSupportedImageMime(m) {
			t.Errorf("isSupportedImageMime(%q) = true, want false", m)
		}
	}
}

func TestResizeImageBlockRejectsInvalidPayload(t *testing.T) {
	profile := &ai.ModelImageResizeOptions{MaxBytes: 1}
	if _, err := ResizeImageBlock(ai.ContentBlock{Type: ai.ContentBlockImage, Data: "not-base64"}, profile); err == nil {
		t.Fatal("invalid base64 accepted")
	}
	if _, err := ResizeImageBlock(ai.ContentBlock{Type: ai.ContentBlockImage, Data: base64.StdEncoding.EncodeToString([]byte("oversized"))}, profile); err == nil {
		t.Fatal("oversized payload accepted")
	}
}

func TestResizeImageBlockUsesJPEGQuality(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for y := range 32 {
		for x := range 32 {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 7), G: uint8(y * 7), B: uint8(x * y), A: 255})
		}
	}
	data, err := encodeImage(img, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	block := ai.ContentBlock{Type: ai.ContentBlockImage, MimeType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(data)}
	low, err := ResizeImageBlock(block, &ai.ModelImageResizeOptions{MaxWidth: 16, MaxHeight: 16, JPEGQuality: 10})
	if err != nil {
		t.Fatal(err)
	}
	high, err := ResizeImageBlock(block, &ai.ModelImageResizeOptions{MaxWidth: 16, MaxHeight: 16, JPEGQuality: 95})
	if err != nil {
		t.Fatal(err)
	}
	if len(low.Data) >= len(high.Data) {
		t.Fatal("JPEG quality ignored")
	}
	if _, err := ResizeImageBlock(block, &ai.ModelImageResizeOptions{JPEGQuality: 101}); err == nil {
		t.Fatal("invalid JPEG quality accepted")
	}
}
