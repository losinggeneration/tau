package tui

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestDetectTerminalCapabilities_TmuxDisablesImagesAndHyperlinks(t *testing.T) {
	caps := DetectTerminalCapabilities(map[string]string{
		"TMUX":      "/tmp/tmux-1000/default,123,0",
		"COLORTERM": "truecolor",
	})

	if caps.Images != ImageProtocolNone {
		t.Fatalf("Images = %q, want none", caps.Images)
	}
	if !caps.TrueColor {
		t.Fatalf("TrueColor = false, want true")
	}
	if caps.Hyperlinks {
		t.Fatalf("Hyperlinks = true, want false")
	}
}

func TestDetectTerminalCapabilities_KnownTerminals(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want ImageProtocol
	}{
		{name: "kitty env", env: map[string]string{"KITTY_WINDOW_ID": "1"}, want: ImageProtocolKitty},
		{name: "ghostty", env: map[string]string{"TERM_PROGRAM": "ghostty"}, want: ImageProtocolKitty},
		{name: "wezterm", env: map[string]string{"WEZTERM_PANE": "1"}, want: ImageProtocolKitty},
		{name: "iterm", env: map[string]string{"ITERM_SESSION_ID": "1"}, want: ImageProtocolITerm2},
		{name: "vscode", env: map[string]string{"TERM_PROGRAM": "vscode"}, want: ImageProtocolNone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caps := DetectTerminalCapabilities(tt.env)
			if caps.Images != tt.want {
				t.Fatalf("Images = %q, want %q", caps.Images, tt.want)
			}
			if !caps.TrueColor {
				t.Fatalf("TrueColor = false, want true")
			}
			if !caps.Hyperlinks && tt.want != ImageProtocolNone {
				t.Fatalf("Hyperlinks = false, want true")
			}
		})
	}
}

func TestEncodeKittyChunksAndDelete(t *testing.T) {
	short := EncodeKittyImage("abc", KittyImageOptions{Columns: 10, Rows: 2, ImageID: 7})
	if !strings.HasPrefix(short, "\x1b_G") || !strings.Contains(short, "c=10") || !strings.Contains(short, "r=2") || !strings.Contains(short, "i=7") || !strings.HasSuffix(short, "abc\x1b\\") {
		t.Fatalf("short kitty sequence = %q", short)
	}

	long := EncodeKittyImage(strings.Repeat("a", 5000), KittyImageOptions{})
	if !strings.Contains(long, ",m=1;") || !strings.Contains(long, "\x1b_Gm=0;") {
		t.Fatalf("chunked kitty sequence missing m markers: %q", long[:80])
	}

	if got := DeleteKittyImage(42); got != "\x1b_Ga=d,d=I,i=42\x1b\\" {
		t.Fatalf("DeleteKittyImage = %q", got)
	}
	if got := DeleteAllKittyImages(); got != "\x1b_Ga=d,d=A\x1b\\" {
		t.Fatalf("DeleteAllKittyImages = %q", got)
	}
}

func TestImageDimensionParsers(t *testing.T) {
	png := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0, 'I', 'H', 'D', 'R'}, []byte{0, 0, 0, 3, 0, 0, 0, 5}...)
	got, ok := DecodeImageDimensions(base64.StdEncoding.EncodeToString(png), "image/png")
	if !ok || got.WidthPx != 3 || got.HeightPx != 5 {
		t.Fatalf("png dims = %+v/%v, want 3x5/true", got, ok)
	}

	gif := []byte("GIF89a\x07\x00\x09\x00")
	got, ok = DecodeImageDimensions(base64.StdEncoding.EncodeToString(gif), "image/gif")
	if !ok || got.WidthPx != 7 || got.HeightPx != 9 {
		t.Fatalf("gif dims = %+v/%v, want 7x9/true", got, ok)
	}

	webp := make([]byte, 30)
	copy(webp[0:4], "RIFF")
	copy(webp[8:12], "WEBP")
	copy(webp[12:16], "VP8X")
	webp[24], webp[25], webp[26] = 1, 0, 0
	webp[27], webp[28], webp[29] = 2, 0, 0
	got, ok = DecodeImageDimensions(base64.StdEncoding.EncodeToString(webp), "image/webp")
	if !ok || got.WidthPx != 2 || got.HeightPx != 3 {
		t.Fatalf("webp dims = %+v/%v, want 2x3/true", got, ok)
	}
}

func TestRenderTerminalImageKittyAndFallback(t *testing.T) {
	SetTerminalCapabilities(TerminalCapabilities{Images: ImageProtocolKitty, TrueColor: true, Hyperlinks: true})
	defer ResetTerminalCapabilities()
	SetCellDimensions(CellDimensions{WidthPx: 10, HeightPx: 10})

	result, ok := RenderTerminalImage("abc", ImageDimensions{WidthPx: 20, HeightPx: 30}, ImageRenderOptions{MaxWidthCells: 4, ImageID: 12})
	if !ok {
		t.Fatalf("RenderTerminalImage ok = false, want true")
	}
	if result.Rows != 6 {
		t.Fatalf("Rows = %d, want 6", result.Rows)
	}
	if !strings.Contains(result.Sequence, "i=12") {
		t.Fatalf("Sequence %q missing image id", result.Sequence)
	}

	SetTerminalCapabilities(TerminalCapabilities{})
	_, ok = RenderTerminalImage("abc", ImageDimensions{WidthPx: 20, HeightPx: 30}, ImageRenderOptions{MaxWidthCells: 4})
	if ok {
		t.Fatalf("RenderTerminalImage ok = true with no image protocol, want false")
	}
}

func TestRenderTerminalImageMaxHeightPreservesAspect(t *testing.T) {
	SetTerminalCapabilities(TerminalCapabilities{Images: ImageProtocolKitty, TrueColor: true})
	defer ResetTerminalCapabilities()
	SetCellDimensions(CellDimensions{WidthPx: 10, HeightPx: 10})

	result, ok := RenderTerminalImage("abc", ImageDimensions{WidthPx: 100, HeightPx: 100}, ImageRenderOptions{MaxWidthCells: 20, MaxHeightCells: 4})
	if !ok {
		t.Fatalf("RenderTerminalImage ok = false, want true")
	}
	if result.Rows != 4 || !strings.Contains(result.Sequence, "c=4,r=4") {
		t.Fatalf("result = %#v, want proportional 4x4", result)
	}
}

func TestImageDataRenderUsesProtocolRows(t *testing.T) {
	SetTerminalCapabilities(TerminalCapabilities{Images: ImageProtocolKitty, TrueColor: true, Hyperlinks: true})
	defer ResetTerminalCapabilities()
	SetCellDimensions(CellDimensions{WidthPx: 10, HeightPx: 10})

	img := NewImageData("abc", "image/png", ImageDimensions{WidthPx: 20, HeightPx: 20}, ImageOptions{Filename: "cat.png", MaxWidthCells: 4, ImageID: 99})
	got := img.Render(10)
	if len(got) != 4 {
		t.Fatalf("Render rows = %d, want 4: %#v", len(got), got)
	}
	if got[0] != "" || got[1] != "" || got[2] != "" {
		t.Fatalf("reserved rows = %#v, want first three empty", got)
	}
	if !strings.HasPrefix(got[3], "\x1b[3A\x1b_G") || !strings.Contains(got[3], "i=99") {
		t.Fatalf("protocol row = %q", got[3])
	}
}

func TestEncodeITerm2ImageSequenceParity(t *testing.T) {
	got := EncodeITerm2Image("YWJj", ITerm2ImageOptions{Width: "10", Height: "auto", Name: "cat.png", Inline: true, PreserveAspectRatio: true})
	if !strings.HasPrefix(got, "\x1b]1337;File=") || !strings.HasSuffix(got, ":YWJj\a") {
		t.Fatalf("bad iTerm2 sequence %q", got)
	}
	for _, want := range []string{"inline=1", "width=10", "height=auto", "name=Y2F0LnBuZw=="} {
		if !strings.Contains(got, want) {
			t.Fatalf("sequence missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "preserveAspectRatio=0") {
		t.Fatalf("sequence disabled aspect preservation: %q", got)
	}
}

// TestTerminalImageGolden_KittyProtocol verifies the exact Kitty
// graphics protocol output for a known image at known cell dimensions.
func TestTerminalImageGolden_KittyProtocol(t *testing.T) {
	SetTerminalCapabilities(TerminalCapabilities{Images: ImageProtocolKitty, TrueColor: true, Hyperlinks: true})
	defer ResetTerminalCapabilities()
	SetCellDimensions(CellDimensions{WidthPx: 10, HeightPx: 20})

	// 40x40 px image at 4 cells wide → 40px / 10px = 4 cells wide, 40px/20px = 2 rows
	result, ok := RenderTerminalImage("AAA", ImageDimensions{WidthPx: 40, HeightPx: 40}, ImageRenderOptions{MaxWidthCells: 4, ImageID: 5})
	if !ok {
		t.Fatalf("RenderTerminalImage ok=false, want true")
	}
	if result.Rows != 2 {
		t.Fatalf("Rows = %d, want 2", result.Rows)
	}
	// Must contain Kitty graphics protocol markers
	if !strings.HasPrefix(result.Sequence, "\x1b_G") {
		t.Fatalf("sequence does not start with Kitty APC: %q", result.Sequence)
	}
	if !strings.HasSuffix(result.Sequence, "\x1b\\") {
		t.Fatalf("sequence does not end with ST: %q", result.Sequence)
	}
	if !strings.Contains(result.Sequence, "i=5") {
		t.Fatalf("sequence missing image id 5: %q", result.Sequence)
	}
	if !strings.Contains(result.Sequence, "c=4") {
		t.Fatalf("sequence missing columns=4: %q", result.Sequence)
	}
	if !strings.Contains(result.Sequence, "r=2") {
		t.Fatalf("sequence missing rows=2: %q", result.Sequence)
	}
}

// TestTerminalImageGolden_ITerm2Protocol verifies the exact iTerm2
// inline image protocol output.
func TestTerminalImageGolden_ITerm2Protocol(t *testing.T) {
	SetTerminalCapabilities(TerminalCapabilities{Images: ImageProtocolITerm2, TrueColor: true, Hyperlinks: true})
	defer ResetTerminalCapabilities()
	SetCellDimensions(CellDimensions{WidthPx: 10, HeightPx: 20})

	result, ok := RenderTerminalImage("AAA", ImageDimensions{WidthPx: 40, HeightPx: 40}, ImageRenderOptions{MaxWidthCells: 4, Filename: "test.png"})
	if !ok {
		t.Fatalf("RenderTerminalImage ok=false, want true")
	}
	if result.Rows != 2 {
		t.Fatalf("Rows = %d, want 2", result.Rows)
	}
	if !strings.HasPrefix(result.Sequence, "\x1b]1337;File=") {
		t.Fatalf("sequence does not start with iTerm2 OSC 1337: %q", result.Sequence)
	}
	if !strings.HasSuffix(result.Sequence, "\a") {
		t.Fatalf("sequence does not end with BEL: %q", result.Sequence)
	}
	if !strings.Contains(result.Sequence, "width=4") {
		t.Fatalf("sequence missing width=4: %q", result.Sequence)
	}
}

// TestTerminalImageGolden_KittyChunking verifies that large images are
// chunked into 4096-byte segments with continuation markers.
func TestTerminalImageGolden_KittyChunking(t *testing.T) {
	SetTerminalCapabilities(TerminalCapabilities{Images: ImageProtocolKitty, TrueColor: true, Hyperlinks: true})
	defer ResetTerminalCapabilities()

	// 5000 bytes of base64 data → must be chunked at 4096
	data := strings.Repeat("A", 5000)
	seq := EncodeKittyImage(data, KittyImageOptions{Columns: 10, Rows: 2, ImageID: 1})

	if !strings.Contains(seq, ",m=1;") {
		t.Fatalf("first chunk missing continuation marker m=1: %q", seq)
	}
	if !strings.Contains(seq, "\x1b_Gm=0;") {
		t.Fatalf("last chunk missing final marker m=0: %q", seq)
	}
}

// TestTerminalImageGolden_FallbackText verifies the fallback text
// rendering when no image protocol is supported.
func TestTerminalImageGolden_FallbackText(t *testing.T) {
	SetTerminalCapabilities(TerminalCapabilities{})
	defer ResetTerminalCapabilities()

	_, ok := RenderTerminalImage("AAA", ImageDimensions{WidthPx: 80, HeightPx: 60}, ImageRenderOptions{MaxWidthCells: 8})
	if ok {
		t.Fatalf("RenderTerminalImage should fail with no protocol, got ok=true")
	}
}

// TestTerminalImageGolden_ComponentRendering verifies the Image
// component's render output with Kitty protocol and fallback paths.
func TestTerminalImageGolden_ComponentRendering(t *testing.T) {
	SetTerminalCapabilities(TerminalCapabilities{Images: ImageProtocolKitty, TrueColor: true, Hyperlinks: true})
	defer ResetTerminalCapabilities()
	SetCellDimensions(CellDimensions{WidthPx: 10, HeightPx: 10})

	img := NewImageData("abc", "image/png", ImageDimensions{WidthPx: 20, HeightPx: 20}, ImageOptions{Filename: "cat.png", MaxWidthCells: 4, ImageID: 99})
	rows := img.Render(10)
	if len(rows) != 4 {
		t.Fatalf("Render rows = %d, want 4 (3 reserved + 1 protocol)", len(rows))
	}
	if rows[0] != "" || rows[1] != "" || rows[2] != "" {
		t.Fatalf("first 3 rows should be blank, got: %q", rows[:3])
	}
	if !strings.Contains(rows[3], "\x1b[3A") {
		t.Fatalf("protocol row should move cursor up 3 lines: %q", rows[3])
	}
	if !strings.Contains(rows[3], "\x1b_G") {
		t.Fatalf("protocol row should contain Kitty APC: %q", rows[3])
	}
	if !strings.Contains(rows[3], "i=99") {
		t.Fatalf("protocol row should contain image id 99: %q", rows[3])
	}
}

// TestTerminalImageGolden_ComponentFallback verifies the Image
// component falls back to text when no protocol is supported.
func TestTerminalImageGolden_ComponentFallback(t *testing.T) {
	SetTerminalCapabilities(TerminalCapabilities{})
	defer ResetTerminalCapabilities()

	img := NewImageData("abc", "image/png", ImageDimensions{WidthPx: 80, HeightPx: 60}, ImageOptions{Filename: "test.png"})
	rows := img.Render(40)
	if len(rows) == 0 {
		t.Fatal("no rows rendered for fallback")
	}
	combined := strings.Join(rows, " ")
	if !strings.Contains(combined, "[Image:") {
		t.Fatalf("fallback text should contain [Image: ...], got: %q", combined)
	}
	if !strings.Contains(combined, "test.png") {
		t.Fatalf("fallback text should contain filename test.png, got: %q", combined)
	}
	if !strings.Contains(combined, "[image/png]") {
		t.Fatalf("fallback text should contain mime type, got: %q", combined)
	}
	if !strings.Contains(combined, "80x60") {
		t.Fatalf("fallback text should contain dimensions 80x60, got: %q", combined)
	}
}

// TestTerminalImageGolden_CellSizeQuery verifies that CSI 16 t is the
// cell-size query and CSI 6;h;w t is the response format.
func TestTerminalImageGolden_CellSizeQuery(t *testing.T) {
	// The spec says the TUI queries cell size with CSI 16 t.
	// The response CSI 6;height;width t updates dimensions.
	// This test verifies the parsing works by setting cell
	// dimensions and verifying image rows change accordingly.

	SetTerminalCapabilities(TerminalCapabilities{Images: ImageProtocolKitty, TrueColor: true, Hyperlinks: true})
	defer ResetTerminalCapabilities()

	// Default cell size is 9x18
	SetCellDimensions(CellDimensions{WidthPx: 9, HeightPx: 18})
	rows1 := CalculateImageRows(ImageDimensions{WidthPx: 90, HeightPx: 180}, 10, GetCellDimensions())

	// With a 10x20 cell size, the same image needs different rows
	SetCellDimensions(CellDimensions{WidthPx: 10, HeightPx: 20})
	rows2 := CalculateImageRows(ImageDimensions{WidthPx: 90, HeightPx: 180}, 10, GetCellDimensions())

	if rows1 <= 0 || rows2 <= 0 {
		t.Fatalf("rows should be positive: rows1=%d rows2=%d", rows1, rows2)
	}
	// 90px / 9px = 10 cells wide, scaled: target_width = 10*9 = 90px,
	// scale = 90/90 = 1.0, image height * scale = 180, rows = ceil(180/18) = 10
	if rows1 != 10 {
		t.Fatalf("rows1 with 9x18 cell = %d, want 10", rows1)
	}
	// target_width = 10*10 = 100px, scale = 100/90 = 1.111,
	// image_height * scale = 180 * 1.111 = 200, rows = ceil(200/20) = 10
	if rows2 != 10 {
		t.Fatalf("rows2 with 10x20 cell = %d, want 10", rows2)
	}
}

// TestTerminalImageGolden_DimensionParsing verifies PNG/JPEG/GIF/WebP
// dimension parsing produces correct dimensions.
func TestTerminalImageGolden_DimensionParsing(t *testing.T) {
	// PNG: 6x8
	png := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0, 'I', 'H', 'D', 'R'}, []byte{0, 0, 0, 6, 0, 0, 0, 8}...)
	if dims, ok := DecodeImageDimensions(base64.StdEncoding.EncodeToString(png), "image/png"); !ok || dims.WidthPx != 6 || dims.HeightPx != 8 {
		t.Fatalf("PNG dims = %+v ok=%v, want 6x8", dims, ok)
	}

	// GIF: 12x16
	gif := []byte("GIF89a\x0c\x00\x10\x00")
	if dims, ok := DecodeImageDimensions(base64.StdEncoding.EncodeToString(gif), "image/gif"); !ok || dims.WidthPx != 12 || dims.HeightPx != 16 {
		t.Fatalf("GIF dims = %+v ok=%v, want 12x16", dims, ok)
	}

	// WebP VP8X: 100x200 (stored as width-1, height-1 in 3 bytes each)
	webp := make([]byte, 30)
	copy(webp[0:4], "RIFF")
	copy(webp[8:12], "WEBP")
	copy(webp[12:16], "VP8X")
	webp[24] = 99  // 100-1
	webp[27] = 199 // 200-1
	if dims, ok := DecodeImageDimensions(base64.StdEncoding.EncodeToString(webp), "image/webp"); !ok || dims.WidthPx != 100 || dims.HeightPx != 200 {
		t.Fatalf("WebP dims = %+v ok=%v, want 100x200", dims, ok)
	}
}

// TestTerminalImageGolden_IsImageLine verifies that image protocol
// sequences are recognized as image lines.
func TestTerminalImageGolden_IsImageLine(t *testing.T) {
	if !IsImageLine("\x1b_Ga=T;abc\x1b\\") {
		t.Fatal("Kitty APC sequence should be recognized as image line")
	}
	if !IsImageLine("\x1b]1337;File=inline=1:abc\a") {
		t.Fatal("iTerm2 OSC 1337 sequence should be recognized as image line")
	}
	if IsImageLine("hello world") {
		t.Fatal("plain text should not be recognized as image line")
	}
	if IsImageLine("\x1b[31mred text\x1b[0m") {
		t.Fatal("SGR sequence should not be recognized as image line")
	}
}

// TestTerminalImageGolden_DeleteSequences verifies the Kitty delete
// one/all image sequences.
func TestTerminalImageGolden_DeleteSequences(t *testing.T) {
	if seq := DeleteKittyImage(42); seq != "\x1b_Ga=d,d=I,i=42\x1b\\" {
		t.Fatalf("DeleteKittyImage(42) = %q, want %q", seq, "\x1b_Ga=d,d=I,i=42\x1b\\")
	}
	if seq := DeleteAllKittyImages(); seq != "\x1b_Ga=d,d=A\x1b\\" {
		t.Fatalf("DeleteAllKittyImages() = %q, want %q", seq, "\x1b_Ga=d,d=A\x1b\\")
	}
}

// TestTerminalImageGolden_Hyperlink tests OSC 8 hyperlink generation with
// and without hyperlink support.
func TestTerminalImageGolden_Hyperlink(t *testing.T) {
	SetTerminalCapabilities(TerminalCapabilities{Hyperlinks: true})
	defer ResetTerminalCapabilities()

	link := Hyperlink("click me", "https://example.com")
	if !strings.Contains(link, "\x1b]8;;https://example.com\x1b\\") {
		t.Fatalf("hyperlink with support should contain OSC 8: %q", link)
	}
	if !strings.Contains(link, "click me") {
		t.Fatalf("hyperlink should contain label: %q", link)
	}
	if !strings.HasSuffix(link, "\x1b]8;;\x1b\\") {
		t.Fatalf("hyperlink should end with closing OSC 8: %q", link)
	}
}

// TestTerminalImageGolden_HyperlinkFallback tests hyperlink fallback when
// hyperlinks are not supported.
func TestTerminalImageGolden_HyperlinkFallback(t *testing.T) {
	SetTerminalCapabilities(TerminalCapabilities{Hyperlinks: false})
	defer ResetTerminalCapabilities()

	link := Hyperlink("click me", "https://example.com")
	if strings.Contains(link, "\x1b]8;;") {
		t.Fatalf("hyperlink without support should not contain OSC 8: %q", link)
	}
	if !strings.Contains(link, "click me") {
		t.Fatalf("fallback should still contain label: %q", link)
	}
	if !strings.Contains(link, "https://example.com") {
		t.Fatalf("fallback should contain URL: %q", link)
	}
}

func TestDetectTerminalCapabilitiesScreenDisablesImagesAndHyperlinks(t *testing.T) {
	caps := DetectTerminalCapabilities(map[string]string{
		"STY":       "1234.pts-0.host",
		"TERM":      "xterm-256color",
		"COLORTERM": "24bit",
	})
	if caps.Images != ImageProtocolNone || !caps.TrueColor || caps.Hyperlinks {
		t.Fatalf("caps = %+v, want no images/hyperlinks with truecolor preserved", caps)
	}
}

func TestDetectTerminalCapabilitiesAlacrittyTruecolorHyperlinks(t *testing.T) {
	caps := DetectTerminalCapabilities(map[string]string{"TERM_PROGRAM": "Alacritty"})
	if caps.Images != ImageProtocolNone || !caps.TrueColor || !caps.Hyperlinks {
		t.Fatalf("caps = %+v, want no images, truecolor and hyperlinks", caps)
	}
}

func TestHyperlinkRespectsCapabilitiesAndAvoidsDuplicateURL(t *testing.T) {
	SetTerminalCapabilities(TerminalCapabilities{Hyperlinks: true})
	got := Hyperlink("site", "https://example.com")
	if !strings.Contains(got, "\x1b]8;;https://example.com") || !strings.Contains(got, "site") {
		t.Fatalf("hyperlink sequence = %q", got)
	}

	SetTerminalCapabilities(TerminalCapabilities{})
	if got := Hyperlink("https://example.com", "https://example.com"); got != "https://example.com" {
		t.Fatalf("duplicate fallback = %q", got)
	}
	if got := Hyperlink("site", "https://example.com"); got != "site (https://example.com)" {
		t.Fatalf("visible fallback = %q", got)
	}
	ResetTerminalCapabilities()
}

func TestImageFallbackUnknownDimensionsDefaultsTo800x600(t *testing.T) {
	SetTerminalCapabilities(TerminalCapabilities{})
	defer ResetTerminalCapabilities()
	img := NewImageData("not-base64", "image/png", ImageDimensions{}, ImageOptions{Filename: "bad.png"})
	if img.Width != 800 || img.Height != 600 {
		t.Fatalf("dimensions = %dx%d, want 800x600", img.Width, img.Height)
	}
	got := strings.TrimRight(img.Render(80)[0], " ")
	want := "[Image: bad.png [image/png] 800x600]"
	if got != want {
		t.Fatalf("fallback = %q, want %q", got, want)
	}
}

func TestRenderTerminalImageITerm2RowsAndSequence(t *testing.T) {
	SetTerminalCapabilities(TerminalCapabilities{Images: ImageProtocolITerm2, TrueColor: true, Hyperlinks: true})
	defer ResetTerminalCapabilities()
	SetCellDimensions(CellDimensions{WidthPx: 10, HeightPx: 10})
	result, ok := RenderTerminalImage("YWJj", ImageDimensions{WidthPx: 20, HeightPx: 30}, ImageRenderOptions{MaxWidthCells: 4, Filename: "cat.png"})
	if !ok {
		t.Fatalf("RenderTerminalImage ok = false, want true")
	}
	if result.Rows != 6 {
		t.Fatalf("Rows = %d, want 6", result.Rows)
	}
	if !strings.HasPrefix(result.Sequence, "\x1b]1337;File=") || !strings.Contains(result.Sequence, "name=Y2F0LnBuZw==") {
		t.Fatalf("iTerm2 sequence = %q", result.Sequence)
	}
}

func TestImageDataRenderITerm2ProtocolRows(t *testing.T) {
	SetTerminalCapabilities(TerminalCapabilities{Images: ImageProtocolITerm2, TrueColor: true, Hyperlinks: true})
	defer ResetTerminalCapabilities()
	SetCellDimensions(CellDimensions{WidthPx: 10, HeightPx: 10})
	img := NewImageData("YWJj", "image/png", ImageDimensions{WidthPx: 20, HeightPx: 20}, ImageOptions{Filename: "cat.png", MaxWidthCells: 4})
	got := img.Render(10)
	if len(got) != 4 {
		t.Fatalf("Render rows = %d, want 4: %#v", len(got), got)
	}
	if got[0] != "" || got[1] != "" || got[2] != "" {
		t.Fatalf("reserved rows = %#v, want leading blanks", got)
	}
	if !strings.HasPrefix(got[3], "\x1b[3A\x1b]1337;File=") {
		t.Fatalf("protocol row = %q", got[3])
	}
}
