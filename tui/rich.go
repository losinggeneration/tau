package tui

import (
	"fmt"
	"strings"

	"github.com/losinggeneration/rovel/geom"
	"github.com/losinggeneration/rovel/style"

	"github.com/losinggeneration/tau/tui/ansi"
)

// StripANSI removes terminal control sequences from s.
func StripANSI(s string) string { return ansi.Strip(s) }

// TextWidth returns the visible terminal cell width of s.
func TextWidth(s string) int { return ansi.VisibleWidth(s) }

// Truncate returns s truncated to width visible cells. It preserves ANSI
// control sequences via rovel's ANSI-aware text helpers.
func Truncate(s string, width int, ellipsis bool) string {
	return ansi.Truncate(s, width, ellipsis)
}

// ANSIWrap wraps s to width visible cells while preserving ANSI styling.
func ANSIWrap(s string, width int) []string { return ansi.Wrap(s, width) }

// ANSITruncate returns s truncated to width visible cells while preserving ANSI
// styling and reset safety.
func ANSITruncate(s string, width int, ellipsis bool) string {
	return ansi.Truncate(s, width, ellipsis)
}

// ANSISliceStrict slices s by visible-cell interval, omitting wide graphemes
// that cross either boundary while preserving ANSI control sequences.
func ANSISliceStrict(s string, start, end int) string { return ansi.SliceStrict(s, start, end) }

// Wrap returns s wrapped to width visible cells as string rows.
func Wrap(s string, width int) []string {
	if width <= 0 {
		return nil
	}

	rows := normalizeRows(s)
	if len(rows) == 0 {
		return nil
	}

	out := []string{}
	for _, row := range rows {
		out = append(out, ANSIWrap(row, width)...)
	}

	return out
}

// Markdown renders markdown-ish text to terminal rows. It intentionally starts
// with a conservative, readable baseline rather than a full CommonMark parser.
type Markdown struct {
	id       ID
	text     string
	rect     Rect
	style    style.Style
	visible  bool
	paddingX int
	paddingY int

	// tableChrome encloses rendered tables with rounded top and bottom rules,
	// matching the app look's bordered chrome. Off by default; callers tie it
	// to their theme.
	tableChrome bool

	codeBlockIndent string
	theme           *MarkdownTheme
}

func NewMarkdown(s string) *Markdown {
	return &Markdown{id: NewID(), text: s, visible: true, codeBlockIndent: "  "}
}

func NewPaddedMarkdown(s string, paddingX, paddingY int) *Markdown {
	m := NewMarkdown(s)
	m.SetPadding(paddingX, paddingY)
	return m
}

func (m *Markdown) SetText(s string)       { m.text = s }
func (m *Markdown) SetStyle(s style.Style) { m.style = s }
func (m *Markdown) SetVisible(v bool)      { m.visible = v }
func (m *Markdown) Visible() bool          { return m.visible }

// SetTableChrome encloses rendered markdown tables with rounded top and
// bottom rules. Callers tie this to their theme: tau-style looks enable it,
// other looks keep tables as plain grids.
func (m *Markdown) SetTableChrome(enabled bool) { m.tableChrome = enabled }

// SetCodeBlockIndent sets the prefix of every code block line. It defaults
// to two spaces.
func (m *Markdown) SetCodeBlockIndent(indent string) { m.codeBlockIndent = indent }

// SetTheme supplies semantic foreground styles. Backgrounds remain owned by the
// surrounding message; omitting a theme preserves the standalone renderer look.
func (m *Markdown) SetTheme(theme MarkdownTheme) { m.theme = &theme }

func (m *Markdown) SetPadding(horizontal, vertical int) {
	if horizontal < 0 {
		horizontal = 0
	}
	if vertical < 0 {
		vertical = 0
	}
	m.paddingX = horizontal
	m.paddingY = vertical
}

// MarkdownRow is one rendered markdown row plus its wrap state. Cont marks a
// row that only exists because word wrapping split the previous row's source
// line, so copy/reflow consumers can join rows without inventing breaks.
type MarkdownRow struct {
	Text string
	Cont bool
}

// RenderWrapped renders like Render but reports each row's soft-wrap state.
func (m *Markdown) RenderWrapped(width int) []MarkdownRow {
	if m == nil || width <= 0 || !m.visible {
		return nil
	}

	if isWhitespaceOnly(m.text) {
		return nil
	}

	contentW := width - 2*m.paddingX
	if contentW < 1 {
		contentW = 1
	}
	source := markdownPreprocess(m.text)
	rows := renderMarkdownASTWithOpts([]byte(source), contentW, m.renderOptions())
	prefix := repeatSpace(m.paddingX)

	out := []MarkdownRow{}
	for range m.paddingY {
		out = append(out, MarkdownRow{Text: repeatSpace(width)})
	}
	for _, row := range rows {
		switch {
		case row == markdownDivider:
			out = append(out, MarkdownRow{Text: padRightANSI(prefix+m.themeStyle(strings.Repeat("─", contentW), "hr")+prefix, width)})
		case strings.HasPrefix(row, "│ "):
			if TextWidth(row) <= contentW {
				out = append(out, MarkdownRow{Text: padRightANSI(prefix+row+prefix, width)})
				continue
			}
			wrapped := ansi.Wrap(strings.TrimPrefix(row, "│ "), max(1, contentW-2))
			for i, line := range wrapped {
				out = append(out, MarkdownRow{Text: padRightANSI(prefix+"│ "+line+prefix, width), Cont: i > 0})
			}
		case IsImageLine(row):
			if strings.Contains(row, "\x1b_G") && !strings.HasSuffix(row, "\\") {
				row += "\\"
			}
			out = append(out, MarkdownRow{Text: row})
		case strings.Contains(row, "\x1b_G"):
			out = append(out, MarkdownRow{Text: row + "\\"})
		case TextWidth(row) == 0 && row == "":
			out = append(out, MarkdownRow{Text: repeatSpace(width)})
		default:
			for i, line := range ansi.Wrap(row, contentW) {
				out = append(out, MarkdownRow{Text: padRightANSI(prefix+line+prefix, width), Cont: i > 0})
			}
		}
	}
	for range m.paddingY {
		out = append(out, MarkdownRow{Text: repeatSpace(width)})
	}

	return out
}

func (m *Markdown) renderOptions() markdownRenderOptions {
	return markdownRenderOptions{tableChrome: m.tableChrome, codeBlockIndent: m.codeBlockIndent, theme: m.theme}
}

func (m *Markdown) Render(width int) []string {
	rows := m.RenderWrapped(width)
	if rows == nil {
		return nil
	}
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row.Text
	}
	return out
}

func (m *Markdown) ID() ID                      { return m.id }
func (m *Markdown) Rect() Rect                  { return m.rect }
func (m *Markdown) Layout(r Rect)               { m.rect = r }
func (m *Markdown) Focusable() bool             { return false }
func (m *Markdown) Handle(_ Event, _ *Ctx) bool { return false }
func (m *Markdown) MinSize() Size {
	rows := renderMarkdownASTWithOpts([]byte(markdownPreprocess(m.text)), 0, m.renderOptions())
	w := 1
	for _, row := range rows {
		if rw := TextWidth(row); rw > w {
			w = rw
		}
	}
	if len(rows) == 0 {
		return Size{W: 1, H: 1}
	}

	return Size{W: w + 2*m.paddingX, H: len(rows) + 2*m.paddingY}
}

func (m *Markdown) Paint(d Drawer, _ *Ctx) {
	rows := m.Render(m.rect.W)
	for i, row := range rows {
		y := m.rect.Y + i
		if y >= m.rect.Y+m.rect.H {
			break
		}
		DrawANSIText(d, geom.Point{X: m.rect.X, Y: y}, row, m.style)
	}
}

const markdownDivider = "\x00tau-markdown-divider"

func padRightANSI(s string, width int) string {
	if width <= 0 {
		return ""
	}
	w := ansi.VisibleWidth(s)
	if w > width {
		return ansi.Truncate(s, width, true)
	}
	return s + repeatSpace(width-w)
}

func orderedListItem(s string) bool {
	marker, rest := splitOrderedList(s)
	return marker != "" && strings.TrimSpace(rest) != ""
}

func splitOrderedList(s string) (string, string) {
	for i, r := range s {
		if r < '0' || r > '9' {
			if r == '.' && i > 0 && i+1 < len(s) && s[i+1] == ' ' {
				return s[:i+1], s[i+2:]
			}
			return "", ""
		}
	}
	return "", ""
}

// Image renders a terminal image protocol when supported and otherwise renders
// a fallback placeholder.
type Image struct {
	id       ID
	rect     Rect
	Filename string
	MIME     string
	Width    int
	Height   int
	visible  bool

	base64Data string
	options    ImageRenderOptions
	cachedW    int
	cachedRows []string
}

func NewImage(filename, mime string, width, height int) *Image {
	return &Image{id: NewID(), Filename: filename, MIME: mime, Width: width, Height: height, visible: true, cachedW: -1}
}

// ImageOptions configures NewImageData.
type ImageOptions = ImageRenderOptions

// NewImageData returns an image component backed by base64 data. If dimensions
// are zero, supported image formats are decoded from base64 data; otherwise the
// Pi-compatible 800x600 default is used.
func NewImageData(base64Data, mime string, dimensions ImageDimensions, options ImageOptions) *Image {
	if dimensions.WidthPx <= 0 || dimensions.HeightPx <= 0 {
		if decoded, ok := DecodeImageDimensions(base64Data, mime); ok {
			dimensions = decoded
		} else {
			dimensions = ImageDimensions{WidthPx: 800, HeightPx: 600}
		}
	}

	return &Image{
		id:         NewID(),
		Filename:   options.Filename,
		MIME:       mime,
		Width:      dimensions.WidthPx,
		Height:     dimensions.HeightPx,
		visible:    true,
		base64Data: base64Data,
		options:    options,
		cachedW:    -1,
	}
}

func (i *Image) SetVisible(v bool) { i.visible = v }
func (i *Image) Visible() bool     { return i.visible }

// Invalidate clears cached rendered image rows.
func (i *Image) Invalidate() {
	if i == nil {
		return
	}
	i.cachedW = -1
	i.cachedRows = nil
}

// ImageID returns the configured Kitty image id, if any.
func (i *Image) ImageID() uint32 {
	if i == nil {
		return 0
	}

	return i.options.ImageID
}

func (i *Image) FallbackText() string {
	name := i.Filename
	mime := i.MIME
	if mime == "" {
		mime = "unknown"
	}
	parts := []string{}
	if name != "" {
		parts = append(parts, name)
	}
	parts = append(parts, fmt.Sprintf("[%s]", mime))
	if i.Width > 0 && i.Height > 0 {
		parts = append(parts, fmt.Sprintf("%dx%d", i.Width, i.Height))
	}

	return fmt.Sprintf("[Image: %s]", strings.Join(parts, " "))
}

func (i *Image) Render(width int) []string {
	if i == nil || width <= 0 || !i.visible {
		return nil
	}
	if i.cachedW == width && i.cachedRows != nil {
		out := make([]string, len(i.cachedRows))
		copy(out, i.cachedRows)

		return out
	}

	var out []string
	if i.base64Data != "" {
		opts := i.options
		if opts.MaxWidthCells <= 0 {
			opts.MaxWidthCells = min(width-2, 60)
			if opts.MaxWidthCells < 1 {
				opts.MaxWidthCells = 1
			}
		}
		result, ok := RenderTerminalImage(i.base64Data, ImageDimensions{WidthPx: i.Width, HeightPx: i.Height}, opts)
		if ok {
			out = make([]string, 0, result.Rows)
			for range result.Rows - 1 {
				out = append(out, "")
			}
			moveUp := ""
			if result.Rows > 1 {
				moveUp = fmt.Sprintf("\x1b[%dA", result.Rows-1)
			}
			out = append(out, moveUp+result.Sequence)
		}
	}
	if out == nil {
		out = padRows(wrapRow(i.FallbackText(), width), width)
	}

	i.cachedW = width
	i.cachedRows = append([]string(nil), out...)

	return out
}

func (i *Image) ID() ID                      { return i.id }
func (i *Image) Rect() Rect                  { return i.rect }
func (i *Image) Layout(r Rect)               { i.rect = r }
func (i *Image) Focusable() bool             { return false }
func (i *Image) Handle(_ Event, _ *Ctx) bool { return false }
func (i *Image) MinSize() Size {
	w := TextWidth(i.FallbackText())
	if w < 1 {
		w = 1
	}

	return Size{W: w, H: 1}
}

func (i *Image) Paint(d Drawer, _ *Ctx) {
	rows := i.Render(i.rect.W)
	for n, row := range rows {
		y := i.rect.Y + n
		if y >= i.rect.Y+i.rect.H {
			break
		}
		d.DrawText(geom.Point{X: i.rect.X, Y: y}, row, style.Style{})
	}
}
