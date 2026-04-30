package tui

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"os"
	"strings"
	"sync"
)

// ImageProtocol identifies a supported terminal image protocol.
type ImageProtocol string

const (
	// ImageProtocolNone means no terminal image protocol should be emitted.
	ImageProtocolNone ImageProtocol = ""
	// ImageProtocolKitty is the Kitty graphics protocol, also used by Ghostty and WezTerm.
	ImageProtocolKitty ImageProtocol = "kitty"
	// ImageProtocolITerm2 is iTerm2's OSC 1337 inline image protocol.
	ImageProtocolITerm2 ImageProtocol = "iterm2"
)

// TerminalCapabilities describes terminal rendering features relevant to the TUI.
type TerminalCapabilities struct {
	Images     ImageProtocol
	TrueColor  bool
	Hyperlinks bool
}

// CellDimensions stores terminal cell dimensions in pixels.
type CellDimensions struct {
	WidthPx  int
	HeightPx int
}

// ImageDimensions stores image dimensions in pixels.
type ImageDimensions struct {
	WidthPx  int
	HeightPx int
}

var terminalImageState = struct {
	sync.Mutex
	caps       *TerminalCapabilities
	cell       CellDimensions
	imageIDGen *rand.Rand
}{
	cell:       CellDimensions{WidthPx: 9, HeightPx: 18},
	imageIDGen: rand.New(rand.NewSource(1)),
}

// DetectTerminalCapabilities detects terminal image, truecolor, and hyperlink capabilities.
func DetectTerminalCapabilities(env map[string]string) TerminalCapabilities {
	get := func(key string) string {
		if env != nil {
			return env[key]
		}

		return os.Getenv(key)
	}

	termProgram := strings.ToLower(get("TERM_PROGRAM"))
	term := strings.ToLower(get("TERM"))
	colorTerm := strings.ToLower(get("COLORTERM"))
	trueColor := colorTerm == "truecolor" || colorTerm == "24bit"

	inTmuxOrScreen := get("TMUX") != "" || get("STY") != "" || strings.HasPrefix(term, "tmux") || strings.HasPrefix(term, "screen")
	if inTmuxOrScreen {
		return TerminalCapabilities{Images: ImageProtocolNone, TrueColor: trueColor, Hyperlinks: false}
	}

	if get("KITTY_WINDOW_ID") != "" || termProgram == "kitty" {
		return TerminalCapabilities{Images: ImageProtocolKitty, TrueColor: true, Hyperlinks: true}
	}
	if termProgram == "ghostty" || strings.Contains(term, "ghostty") || get("GHOSTTY_RESOURCES_DIR") != "" {
		return TerminalCapabilities{Images: ImageProtocolKitty, TrueColor: true, Hyperlinks: true}
	}
	if get("WEZTERM_PANE") != "" || termProgram == "wezterm" {
		return TerminalCapabilities{Images: ImageProtocolKitty, TrueColor: true, Hyperlinks: true}
	}
	if get("ITERM_SESSION_ID") != "" || termProgram == "iterm.app" {
		return TerminalCapabilities{Images: ImageProtocolITerm2, TrueColor: true, Hyperlinks: true}
	}
	if termProgram == "vscode" || termProgram == "alacritty" {
		return TerminalCapabilities{Images: ImageProtocolNone, TrueColor: true, Hyperlinks: true}
	}

	return TerminalCapabilities{Images: ImageProtocolNone, TrueColor: trueColor, Hyperlinks: false}
}

// GetTerminalCapabilities returns cached detected terminal capabilities.
func GetTerminalCapabilities() TerminalCapabilities {
	terminalImageState.Lock()
	defer terminalImageState.Unlock()
	if terminalImageState.caps != nil {
		return *terminalImageState.caps
	}
	caps := DetectTerminalCapabilities(nil)
	terminalImageState.caps = &caps

	return caps
}

// SetTerminalCapabilities overrides terminal capabilities, primarily for tests.
func SetTerminalCapabilities(caps TerminalCapabilities) {
	terminalImageState.Lock()
	defer terminalImageState.Unlock()
	terminalImageState.caps = &caps
}

// ResetTerminalCapabilities clears the cached/overridden capabilities.
func ResetTerminalCapabilities() {
	terminalImageState.Lock()
	defer terminalImageState.Unlock()
	terminalImageState.caps = nil
}

// GetCellDimensions returns current terminal cell dimensions.
func GetCellDimensions() CellDimensions {
	terminalImageState.Lock()
	defer terminalImageState.Unlock()

	return terminalImageState.cell
}

// SetCellDimensions stores terminal cell dimensions. Non-positive fields keep defaults.
func SetCellDimensions(dims CellDimensions) {
	if dims.WidthPx <= 0 {
		dims.WidthPx = 9
	}
	if dims.HeightPx <= 0 {
		dims.HeightPx = 18
	}
	terminalImageState.Lock()
	defer terminalImageState.Unlock()
	terminalImageState.cell = dims
}

// IsImageLine reports whether a rendered row contains a terminal image protocol sequence.
func IsImageLine(line string) bool {
	return strings.Contains(line, "\x1b_G") || strings.Contains(line, "\x1b]1337;File=")
}

// AllocateImageID returns a pseudo-random Kitty image id in [1, 0xffffffff].
func AllocateImageID() uint32 {
	terminalImageState.Lock()
	defer terminalImageState.Unlock()

	return uint32(terminalImageState.imageIDGen.Int31()) + 1
}

// KittyImageOptions controls Kitty graphics encoding.
type KittyImageOptions struct {
	Columns int
	Rows    int
	ImageID uint32
}

// EncodeKittyImage encodes base64 image data using Kitty graphics protocol.
func EncodeKittyImage(base64Data string, options KittyImageOptions) string {
	const chunkSize = 4096

	params := []string{"a=T", "f=100", "q=2"}
	if options.Columns > 0 {
		params = append(params, fmt.Sprintf("c=%d", options.Columns))
	}
	if options.Rows > 0 {
		params = append(params, fmt.Sprintf("r=%d", options.Rows))
	}
	if options.ImageID > 0 {
		params = append(params, fmt.Sprintf("i=%d", options.ImageID))
	}

	if len(base64Data) <= chunkSize {
		return "\x1b_G" + strings.Join(params, ",") + ";" + base64Data + "\x1b\\"
	}

	var b strings.Builder
	for offset, first := 0, true; offset < len(base64Data); first = false {
		end := offset + chunkSize
		if end > len(base64Data) {
			end = len(base64Data)
		}
		chunk := base64Data[offset:end]
		last := end >= len(base64Data)
		switch {
		case first:
			b.WriteString("\x1b_G" + strings.Join(params, ",") + ",m=1;" + chunk + "\x1b\\")
		case last:
			b.WriteString("\x1b_Gm=0;" + chunk + "\x1b\\")
		default:
			b.WriteString("\x1b_Gm=1;" + chunk + "\x1b\\")
		}
		offset = end
	}

	return b.String()
}

// DeleteKittyImage returns the sequence to delete one Kitty image and free its data.
func DeleteKittyImage(imageID uint32) string {
	return fmt.Sprintf("\x1b_Ga=d,d=I,i=%d\x1b\\", imageID)
}

// DeleteAllKittyImages returns the sequence to delete all visible Kitty images.
func DeleteAllKittyImages() string { return "\x1b_Ga=d,d=A\x1b\\" }

// ITerm2ImageOptions controls iTerm2 inline image encoding.
type ITerm2ImageOptions struct {
	Width               string
	Height              string
	Name                string
	Inline              bool
	PreserveAspectRatio bool
	NoPreserveAspect    bool
}

// EncodeITerm2Image encodes base64 image data using iTerm2's inline image protocol.
func EncodeITerm2Image(base64Data string, options ITerm2ImageOptions) string {
	params := []string{"inline=1"}
	if !options.Inline {
		params[0] = "inline=0"
	}
	if options.Width != "" {
		params = append(params, "width="+options.Width)
	}
	if options.Height != "" {
		params = append(params, "height="+options.Height)
	}
	if options.Name != "" {
		params = append(params, "name="+base64.StdEncoding.EncodeToString([]byte(options.Name)))
	}
	if options.NoPreserveAspect || !options.PreserveAspectRatio {
		params = append(params, "preserveAspectRatio=0")
	}

	return "\x1b]1337;File=" + strings.Join(params, ";") + ":" + base64Data + "\a"
}

// CalculateImageRows returns the rows needed for an image at targetWidthCells.
func CalculateImageRows(imageDimensions ImageDimensions, targetWidthCells int, cellDimensions CellDimensions) int {
	if targetWidthCells <= 0 || imageDimensions.WidthPx <= 0 || imageDimensions.HeightPx <= 0 {
		return 1
	}
	if cellDimensions.WidthPx <= 0 {
		cellDimensions.WidthPx = 9
	}
	if cellDimensions.HeightPx <= 0 {
		cellDimensions.HeightPx = 18
	}
	targetWidthPx := targetWidthCells * cellDimensions.WidthPx
	scale := float64(targetWidthPx) / float64(imageDimensions.WidthPx)
	rows := int(math.Ceil(float64(imageDimensions.HeightPx) * scale / float64(cellDimensions.HeightPx)))
	if rows < 1 {
		return 1
	}

	return rows
}

// DecodeImageDimensions decodes dimensions for PNG, JPEG, GIF, and WebP base64 images.
func DecodeImageDimensions(base64Data, mimeType string) (ImageDimensions, bool) {
	data, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		return ImageDimensions{}, false
	}
	switch mimeType {
	case "image/png":
		return pngDimensions(data)
	case "image/jpeg", "image/jpg":
		return jpegDimensions(data)
	case "image/gif":
		return gifDimensions(data)
	case "image/webp":
		return webpDimensions(data)
	default:
		return ImageDimensions{}, false
	}
}

func pngDimensions(data []byte) (ImageDimensions, bool) {
	if len(data) < 24 || data[0] != 0x89 || string(data[1:4]) != "PNG" {
		return ImageDimensions{}, false
	}

	return ImageDimensions{WidthPx: int(binary.BigEndian.Uint32(data[16:20])), HeightPx: int(binary.BigEndian.Uint32(data[20:24]))}, true
}

func gifDimensions(data []byte) (ImageDimensions, bool) {
	if len(data) < 10 || (string(data[:6]) != "GIF87a" && string(data[:6]) != "GIF89a") {
		return ImageDimensions{}, false
	}

	return ImageDimensions{WidthPx: int(binary.LittleEndian.Uint16(data[6:8])), HeightPx: int(binary.LittleEndian.Uint16(data[8:10]))}, true
}

func jpegDimensions(data []byte) (ImageDimensions, bool) {
	if len(data) < 2 || data[0] != 0xff || data[1] != 0xd8 {
		return ImageDimensions{}, false
	}
	for offset := 2; offset < len(data)-9; {
		if data[offset] != 0xff {
			offset++
			continue
		}
		marker := data[offset+1]
		if marker >= 0xc0 && marker <= 0xc2 {
			return ImageDimensions{HeightPx: int(binary.BigEndian.Uint16(data[offset+5 : offset+7])), WidthPx: int(binary.BigEndian.Uint16(data[offset+7 : offset+9]))}, true
		}
		if offset+3 >= len(data) {
			return ImageDimensions{}, false
		}
		length := int(binary.BigEndian.Uint16(data[offset+2 : offset+4]))
		if length < 2 {
			return ImageDimensions{}, false
		}
		offset += 2 + length
	}

	return ImageDimensions{}, false
}

func webpDimensions(data []byte) (ImageDimensions, bool) {
	if len(data) < 30 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return ImageDimensions{}, false
	}
	switch string(data[12:16]) {
	case "VP8 ":
		return ImageDimensions{WidthPx: int(binary.LittleEndian.Uint16(data[26:28]) & 0x3fff), HeightPx: int(binary.LittleEndian.Uint16(data[28:30]) & 0x3fff)}, true
	case "VP8L":
		if len(data) < 25 {
			return ImageDimensions{}, false
		}
		bits := binary.LittleEndian.Uint32(data[21:25])
		return ImageDimensions{WidthPx: int(bits&0x3fff) + 1, HeightPx: int((bits>>14)&0x3fff) + 1}, true
	case "VP8X":
		width := int(data[24]) | int(data[25])<<8 | int(data[26])<<16
		height := int(data[27]) | int(data[28])<<8 | int(data[29])<<16
		return ImageDimensions{WidthPx: width + 1, HeightPx: height + 1}, true
	default:
		return ImageDimensions{}, false
	}
}

// ImageRenderOptions controls terminal image rendering.
type ImageRenderOptions struct {
	MaxWidthCells    int
	MaxHeightCells   int
	PreserveAspect   bool
	NoPreserveAspect bool
	ImageID          uint32
	Filename         string
}

// TerminalImageResult is a rendered terminal image protocol sequence and reserved row count.
type TerminalImageResult struct {
	Sequence string
	Rows     int
	ImageID  uint32
}

// RenderTerminalImage renders an image protocol sequence for the active terminal.
func RenderTerminalImage(base64Data string, dimensions ImageDimensions, options ImageRenderOptions) (TerminalImageResult, bool) {
	caps := GetTerminalCapabilities()
	if caps.Images == ImageProtocolNone {
		return TerminalImageResult{}, false
	}
	maxWidth := options.MaxWidthCells
	if maxWidth <= 0 {
		maxWidth = 80
	}
	rows := CalculateImageRows(dimensions, maxWidth, GetCellDimensions())
	if options.MaxHeightCells > 0 && rows > options.MaxHeightCells {
		maxWidth = max(1, maxWidth*options.MaxHeightCells/rows)
		rows = CalculateImageRows(dimensions, maxWidth, GetCellDimensions())
		if rows > options.MaxHeightCells {
			rows = options.MaxHeightCells
		}
	}

	switch caps.Images {
	case ImageProtocolKitty:
		seq := EncodeKittyImage(base64Data, KittyImageOptions{Columns: maxWidth, Rows: rows, ImageID: options.ImageID})
		return TerminalImageResult{Sequence: seq, Rows: rows, ImageID: options.ImageID}, true
	case ImageProtocolITerm2:
		seq := EncodeITerm2Image(base64Data, ITerm2ImageOptions{Width: fmt.Sprintf("%d", maxWidth), Height: "auto", Name: options.Filename, Inline: true, PreserveAspectRatio: !options.NoPreserveAspect})
		return TerminalImageResult{Sequence: seq, Rows: rows}, true
	default:
		return TerminalImageResult{}, false
	}
}

// Hyperlink wraps text with OSC 8 hyperlink markup.
func Hyperlink(label, url string) string {
	if !GetTerminalCapabilities().Hyperlinks {
		if label == url {
			return label
		}
		return label + " (" + url + ")"
	}

	return "\x1b]8;;" + url + "\x1b\\" + label + "\x1b]8;;\x1b\\"
}
