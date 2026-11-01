package tui

import (
	"cmp"
	"encoding/base64"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/losinggeneration/rovel"

	editorkit "github.com/losinggeneration/tau/tui/editor"
)

// EditorCursorMoveReason describes why a prompt-editor cursor move happened.
type EditorCursorMoveReason = editorkit.CursorMoveReason

const (
	EditorCursorMoveNavigate = editorkit.CursorMoveNavigate
	EditorCursorMoveEdit     = editorkit.CursorMoveEdit
	EditorCursorMovePaste    = editorkit.CursorMovePaste
	EditorCursorMoveSetText  = editorkit.CursorMoveSetText
)

// EditorCursorMoveEvent is delivered when the prompt-editor cursor changes.
type EditorCursorMoveEvent = editorkit.CursorMoveEvent

// PromptEditor is Tau's retained-mode prompt editor component. It combines the
// top-level editor widget with the Pi-style prompt chrome used by the full app:
// horizontal rules above and below the multiline editor, no side rails, and no
// titled box. The component itself is focusable and text-input-mode; it owns the
// wrapped editor widget so callers do not need to assemble app-local frame
// wrappers around lower-level editor pieces.
type PromptEditor struct {
	inner                  *editorkit.Widget
	rect                   Rect
	paddingX               int
	inlineImages           map[string]*Image
	deletedImageIDs        []uint32
	inlineImageHits        []inlineImageHit
	inlineImagesSuppressed bool
	expandedMarker         string
	expandedImageID        uint32
	expandedRect           Rect

	// maxContentRows caps how tall the editor may grow. Zero means uncapped;
	// the owning app sets it from the terminal height (see SetMaxHeight).
	maxContentRows int

	// borderStyle overrides the theme's Border palette role for the rules. The
	// app uses it to signal session state — thinking level, and in hosts that
	// have one, shell-input mode.
	borderStyle    Style
	borderStyleSet bool
}

// Prompt editor sizing. The editor grows with its content between these bounds
// so short prompts stay out of the way and long ones stay readable without
// swallowing the transcript.
const (
	// PromptEditorMinContentRows is the floor: the editor never shows fewer
	// text rows than this, even when empty.
	PromptEditorMinContentRows = 5

	// PromptEditorMaxHeightPercent bounds the whole component (rules included)
	// as a share of the terminal height. On an 80x24 terminal this resolves to
	// exactly the minimum, so the editor is fixed there and only grows on
	// taller terminals.
	PromptEditorMaxHeightPercent = 30
)

// NewPromptEditor returns a prompt editor with a fresh editor model.
func NewPromptEditor() *PromptEditor {
	return NewPromptEditorWithWidget(editorkit.NewWidget())
}

// NewPromptEditorWithWidget wraps an existing editor widget in prompt chrome.
// It is primarily useful for tests and migration paths that already own a model.
func NewPromptEditorWithWidget(inner *editorkit.Widget) *PromptEditor {
	if inner == nil {
		inner = editorkit.NewWidget()
	}
	return &PromptEditor{inner: inner, paddingX: 1}
}

// SetPadding sets the horizontal editor content padding inside the prompt rules.
func (p *PromptEditor) SetPadding(horizontal int) {
	if p == nil {
		return
	}
	if horizontal < 0 {
		horizontal = 0
	}
	p.paddingX = horizontal
}

// Padding returns the horizontal content padding, so chrome rendered directly
// against the editor (autocomplete below the bottom rule) can align its content
// with the editor's text column.
func (p *PromptEditor) Padding() int {
	if p == nil {
		return 0
	}
	return p.paddingX
}

// SetMaxHeight caps the component's total height in rows, applying
// PromptEditorMaxHeightPercent. Callers pass the terminal height; the floor of
// PromptEditorMinContentRows always wins, so a very short terminal still gets a
// usable prompt rather than a sliver. Pass 0 to remove the cap.
func (p *PromptEditor) SetMaxHeight(terminalRows int) {
	if p == nil {
		return
	}
	if terminalRows <= 0 {
		p.maxContentRows = 0
		return
	}
	p.maxContentRows = max(PromptEditorMinContentRows, terminalRows*PromptEditorMaxHeightPercent/100-promptEditorRules)
}

// SetBorderStyle overrides the style used for the top and bottom rules. Passing
// the zero Style restores the theme's Border palette role.
func (p *PromptEditor) SetBorderStyle(st Style) {
	if p == nil {
		return
	}
	p.borderStyle = st
	p.borderStyleSet = st != Style{}
}

// promptEditorRules is the row cost of the chrome: one rule above, one below.
const promptEditorRules = 2

// contentRows resolves how many text rows the editor should show: its wrapped
// content, floored at PromptEditorMinContentRows and capped by SetMaxHeight.
func (p *PromptEditor) contentRows() int {
	// Counting stops at the cap: a 10k-line paste costs the cap, not the paste.
	rows := p.ensure().VisualRowCount(p.contentWidth(), p.maxContentRows)
	rows = max(rows, PromptEditorMinContentRows)
	if p.maxContentRows > 0 {
		rows = min(rows, p.maxContentRows)
	}
	return rows
}

// contentWidth is the inner text width at the last laid-out rect. MinSize is
// asked for a height before the parent assigns a width, so the previous frame's
// width is the best available estimate; it only differs on the frame a resize
// lands, and the next layout pass corrects it.
//
// Before any layout there is no width at all. Falling back to 1 would wrap the
// text into one row per grapheme, so use a conventional terminal width instead
// — the first real layout replaces the estimate.
func (p *PromptEditor) contentWidth() int {
	if p.rect.W <= 0 {
		return promptEditorAssumedWidth
	}
	return max(p.rect.W-2*p.contentPadding(), 1)
}

func (p *PromptEditor) contentPadding() int {
	return min(p.paddingX, max(0, (p.rect.W-1)/2))
}

// promptEditorAssumedWidth is the width used to estimate height before the
// first layout assigns a real one.
const promptEditorAssumedWidth = 80

// Widget returns the underlying editor widget for narrow integration points that
// need editor-package-specific APIs. Most application code should use the
// PromptEditor methods directly.
func (p *PromptEditor) Widget() *editorkit.Widget {
	if p == nil {
		return nil
	}
	return p.ensure()
}

func (p *PromptEditor) ensure() *editorkit.Widget {
	if p.inner == nil {
		p.inner = editorkit.NewWidget()
	}
	return p.inner
}

func (p *PromptEditor) Model() *editorkit.Editor { return p.ensure().Model() }
func (p *PromptEditor) Text() string             { return p.ensure().Text() }

func (p *PromptEditor) SetInlineImagePreview(marker string, data []byte, mime string) string {
	if p == nil || marker == "" || len(data) == 0 {
		return marker
	}
	if p.inlineImages == nil {
		p.inlineImages = map[string]*Image{}
	}
	marker = uniqueInlineImageMarker(marker, p.inlineImages)
	p.inlineImages[marker] = NewImageData(base64.StdEncoding.EncodeToString(data), mime, ImageDimensions{}, ImageOptions{Filename: marker, MaxHeightCells: 2, ImageID: AllocateImageID()})
	return marker
}

func (p *PromptEditor) SetText(ctx *Ctx, s string) {
	p.ensure().SetText(ctx, s)
	p.pruneInlineImagePreviews()
}

// Reset discards the draft, undo state and inline image previews.
func (p *PromptEditor) Reset(ctx *Ctx) {
	p.ensure().Reset(ctx)
	p.pruneInlineImagePreviews()
}

func (p *PromptEditor) ClearInlineImagePreviews() {
	if p == nil {
		return
	}
	for _, img := range p.inlineImages {
		if id := img.ImageID(); id != 0 {
			p.deletedImageIDs = append(p.deletedImageIDs, id)
		}
	}
	clear(p.inlineImages)
}

func (p *PromptEditor) Paste(ctx *Ctx, s string) (string, int) {
	return p.ensure().Paste(ctx, s)
}
func (p *PromptEditor) SetReadOnly(readOnly bool) { p.ensure().SetReadOnly(readOnly) }
func (p *PromptEditor) SetOnChange(fn func(string, *Ctx)) {
	p.ensure().SetOnChange(fn)
}

func (p *PromptEditor) SetOnCursorMove(fn func(EditorCursorMoveEvent, *Ctx)) {
	p.ensure().SetOnCursorMove(fn)
}

func (p *PromptEditor) ID() ID { return p.ensure().ID() }

func (p *PromptEditor) Rect() Rect {
	if p == nil {
		return Rect{}
	}
	return p.rect
}

func (p *PromptEditor) Layout(r Rect) {
	if p == nil {
		return
	}
	p.rect = r
	inner := r
	if inner.H >= promptEditorRules {
		inner.Y++
		inner.H -= promptEditorRules
	} else {
		inner.H = 0
	}
	pad := p.contentPadding()
	inner.X += pad
	inner.W -= 2 * pad
	if inner.W < 0 {
		inner.W = 0
	}
	p.ensure().Layout(inner)
}

func (p *PromptEditor) MinSize() Size {
	if p == nil {
		return Size{W: 10, H: PromptEditorMinContentRows + promptEditorRules}
	}
	childMin := p.ensure().MinSize()
	return Size{W: childMin.W + 2*p.paddingX, H: p.contentRows() + promptEditorRules}
}

func (p *PromptEditor) Paint(d Drawer, ctx *Ctx) {
	if p == nil || ctx == nil || p.rect.W <= 0 || p.rect.H <= 0 {
		return
	}
	st := p.borderStyle
	if !p.borderStyleSet {
		st = ctx.Theme.Palette.Border
		if st == (Style{}) {
			st = ctx.Theme.Base
		}
	}
	d.FillRect(p.rect, ctx.Theme.Base)

	// Scroll indicators replace the middle of a rule when content continues
	// past it, so the rules do double duty as the editor's scrollbar.
	above, below := p.hiddenRows()
	p.drawRule(d, p.rect.Y, st, scrollIndicatorLabel("▲", above))
	bottomY := p.rect.Y + p.rect.H - 1
	if bottomY != p.rect.Y {
		p.drawRule(d, bottomY, st, scrollIndicatorLabel("▼", below))
	}
	if p.rect.H <= promptEditorRules {
		return
	}
	pad := p.contentPadding()
	inner := Rect{X: p.rect.X + pad, Y: p.rect.Y + 1, W: p.rect.W - 2*pad, H: p.rect.H - promptEditorRules}
	if inner.W < 0 {
		inner.W = 0
	}
	d.WithClip(inner, func(d Drawer) {
		p.ensure().Paint(d, ctx)
		p.paintInlineImages(d, ctx, inner)
	})
}

// syncScroll settles the inner editor's scroll offset for the current window.
// The rules are drawn before the editor paints itself, and they report the
// scroll position, so the offset has to be current before that — otherwise the
// indicators lag the cursor by one frame.
func (p *PromptEditor) syncScroll() {
	visible := p.rect.H - promptEditorRules
	if visible <= 0 {
		return
	}
	p.ensure().EnsureCursorVisible(visible, p.contentWidth())
}

// hiddenRows reports how many content rows sit above and below the visible
// window. Both are zero when the whole prompt fits.
func (p *PromptEditor) hiddenRows() (above, below int) {
	visible := p.rect.H - promptEditorRules
	if visible <= 0 {
		return 0, 0
	}
	p.syncScroll()
	// Unbounded here: the indicator reports how much is actually hidden, so a
	// saturated count would understate it.
	total := p.ensure().VisualRowCount(p.contentWidth(), 0)
	if total <= visible {
		return 0, 0
	}
	offset := min(max(p.ensure().ScrollOffset(), 0), total-visible)
	return offset, total - visible - offset
}

// scrollIndicatorLabel renders the marker a rule carries for n hidden rows, or
// "" when nothing is hidden in that direction.
func scrollIndicatorLabel(arrow string, hidden int) string {
	if hidden <= 0 {
		return ""
	}
	return " " + arrow + " " + strconv.Itoa(hidden) + " "
}

// drawRule fills one horizontal rule, centring label in it when the rule is
// wide enough to hold the label with at least one dash on each side.
func (p *PromptEditor) drawRule(d Drawer, y int, st Style, label string) {
	labelWidth := TextWidth(label)
	labelStart := -1
	if label != "" && labelWidth+2 <= p.rect.W {
		labelStart = (p.rect.W - labelWidth) / 2
	}
	for x := 0; x < p.rect.W; x++ {
		if labelStart >= 0 && x == labelStart {
			d.DrawText(Point{X: p.rect.X + x, Y: y}, label, st)
			x += labelWidth - 1
			continue
		}
		d.DrawText(Point{X: p.rect.X + x, Y: y}, "─", st)
	}
}

func (p *PromptEditor) paintInlineImages(d Drawer, ctx *Ctx, inner Rect) {
	rd, _ := d.(interface{ DrawRaw(Point, string) })
	p.flushDeletedInlineImages(rd, inner)
	if len(p.inlineImages) == 0 || GetTerminalCapabilities().Images != ImageProtocolKitty {
		return
	}
	text := p.Text()
	for marker, img := range p.inlineImages {
		if !strings.Contains(text, marker) {
			if rd != nil && img.ImageID() != 0 {
				rd.DrawRaw(Point{X: inner.X, Y: inner.Y}, DeleteKittyImage(img.ImageID()))
			}
			delete(p.inlineImages, marker)
		}
	}
	p.inlineImageHits = p.inlineImageHits[:0]
	if p.inlineImagesSuppressed {
		return
	}
	y := inner.Y - p.ensure().ScrollOffset()
	for _, line := range strings.Split(text, "\n") {
		images := p.inlineImagesForLine(line)
		if len(images) == 0 {
			y++
			continue
		}
		for _, preview := range images {
			if preview.marker == p.expandedMarker {
				p.inlineImageHits = append(p.inlineImageHits, inlineImageHit{marker: preview.marker, rect: Rect{X: inner.X + preview.x, Y: y, W: TextWidth(preview.marker), H: 1}})
				continue
			}
			x := inner.X + preview.x
			if x >= inner.X+inner.W {
				continue
			}
			if rd != nil && preview.img.ImageID() != 0 {
				rd.DrawRaw(Point{X: x, Y: y}, DeleteKittyImage(preview.img.ImageID()))
			}
			preview.img.Invalidate()
			rows := preview.img.Render(inner.W - preview.x)
			cols, hitRows := imageCellsForBounds(ImageDimensions{WidthPx: preview.img.Width, HeightPx: preview.img.Height}, inner.W-preview.x-2, 2)
			p.inlineImageHits = append(p.inlineImageHits, inlineImageHit{marker: preview.marker, rect: Rect{X: x, Y: y, W: cols, H: max(1, hitRows)}})
			for i, row := range rows {
				if y+i < inner.Y || y+i >= inner.Y+inner.H {
					continue
				}
				if rd != nil {
					rd.DrawRaw(Point{X: x, Y: y + i}, row)
				} else {
					d.DrawText(Point{X: x, Y: y + i}, row, ctx.Theme.Base)
				}
			}
		}
		y++
	}
}

type inlineImagePreview struct {
	marker string
	x      int
	img    *Image
}

type inlineImageHit struct {
	marker string
	rect   Rect
}

func (p *PromptEditor) inlineImagesForLine(line string) []inlineImagePreview {
	if len(p.inlineImages) == 0 {
		return nil
	}
	var out []inlineImagePreview
	for marker, img := range p.inlineImages {
		idx := strings.Index(line, marker)
		if idx < 0 {
			continue
		}
		out = append(out, inlineImagePreview{marker: marker, x: TextWidth(line[:idx]), img: img})
	}
	slices.SortFunc(out, func(a, b inlineImagePreview) int { return cmp.Compare(a.x, b.x) })
	return out
}

func (p *PromptEditor) pruneInlineImagePreviews() {
	if len(p.inlineImages) == 0 {
		return
	}
	text := p.Text()
	for marker, img := range p.inlineImages {
		if !strings.Contains(text, marker) {
			if id := img.ImageID(); id != 0 {
				p.deletedImageIDs = append(p.deletedImageIDs, id)
			}
			delete(p.inlineImages, marker)
		}
	}
}

func (p *PromptEditor) flushDeletedInlineImages(rd interface{ DrawRaw(Point, string) }, inner Rect) {
	if rd == nil || len(p.deletedImageIDs) == 0 {
		return
	}
	for _, id := range p.deletedImageIDs {
		rd.DrawRaw(Point{X: inner.X, Y: inner.Y}, DeleteKittyImage(id))
	}
	p.deletedImageIDs = nil
}

func (p *PromptEditor) suppressInlineImagePreviews(ctx *Ctx) []uint32 {
	if p == nil || p.inlineImagesSuppressed {
		return nil
	}
	p.inlineImagesSuppressed = true
	ids := make([]uint32, 0, len(p.inlineImages))
	for _, img := range p.inlineImages {
		if id := img.ImageID(); id != 0 {
			ids = append(ids, id)
		}
	}
	if ctx != nil && ctx.InvalidateAll != nil {
		ctx.InvalidateAll()
	}
	return ids
}

func (p *PromptEditor) restoreInlineImagePreviews(ctx *Ctx) {
	if p == nil || !p.inlineImagesSuppressed {
		return
	}
	p.inlineImagesSuppressed = false
	for _, img := range p.inlineImages {
		img.Invalidate()
	}
	if ctx != nil && ctx.InvalidateAll != nil {
		ctx.InvalidateAll()
	}
}

func uniqueInlineImageMarker(marker string, existing map[string]*Image) string {
	if existing[marker] == nil {
		return marker
	}
	ext := filepath.Ext(marker)
	base := strings.TrimSuffix(marker, ext)
	for n := 2; ; n++ {
		candidate := base + "-" + strconv.Itoa(n) + ext
		if existing[candidate] == nil {
			return candidate
		}
	}
}

func (p *PromptEditor) PaintExpandedInlineImage(d Drawer, bounds Rect) {
	if p.expandedMarker == "" || p.expandedImageID == 0 || GetTerminalCapabilities().Images != ImageProtocolKitty {
		return
	}
	img := p.inlineImages[p.expandedMarker]
	if img == nil {
		p.deletedImageIDs = append(p.deletedImageIDs, p.expandedImageID)
		p.expandedMarker = ""
		p.expandedImageID = 0
		p.expandedRect = Rect{}
		return
	}
	rd, ok := d.(interface{ DrawRaw(Point, string) })
	if !ok || bounds.W <= 0 || bounds.H <= 0 {
		return
	}
	cols, rows := imageCellsForBounds(ImageDimensions{WidthPx: img.Width, HeightPx: img.Height}, bounds.W-4, bounds.H-4)
	x := bounds.X + max(0, (bounds.W-cols)/2)
	y := bounds.Y + max(0, (bounds.H-rows)/2)
	p.expandedRect = Rect{X: x, Y: y, W: cols, H: rows}
	rd.DrawRaw(Point{X: x, Y: y}, DeleteKittyImage(p.expandedImageID))
	seq, ok := RenderTerminalImage(img.base64Data, ImageDimensions{WidthPx: img.Width, HeightPx: img.Height}, ImageRenderOptions{MaxWidthCells: cols, MaxHeightCells: rows, ImageID: p.expandedImageID, Filename: img.Filename})
	if ok {
		rd.DrawRaw(Point{X: x, Y: y}, seq.Sequence)
	}
}

func imageCellsForBounds(dim ImageDimensions, maxCols, maxRows int) (int, int) {
	maxCols = max(1, maxCols)
	maxRows = max(1, maxRows)
	cell := GetCellDimensions()
	cols := min(maxCols, max(1, (dim.WidthPx+cell.WidthPx-1)/cell.WidthPx))
	rows := CalculateImageRows(dim, cols, cell)
	if rows > maxRows {
		cols = max(1, cols*maxRows/rows)
		rows = CalculateImageRows(dim, cols, cell)
		if rows > maxRows {
			rows = maxRows
		}
	}
	return cols, rows
}

func (p *PromptEditor) Handle(e Event, ctx *Ctx) bool {
	if p == nil {
		return false
	}
	if me, ok := e.(MouseEvent); ok && p.ToggleInlineImageAt(me, ctx) {
		return true
	}
	return p.ensure().Handle(e, ctx)
}

func (p *PromptEditor) ExpandedInlineImageShown() bool {
	return p != nil && p.expandedMarker != ""
}

func (p *PromptEditor) CloseExpandedInlineImage(ctx *Ctx) bool {
	if p == nil || p.expandedMarker == "" {
		return false
	}
	if p.expandedImageID != 0 {
		p.deletedImageIDs = append(p.deletedImageIDs, p.expandedImageID)
	}
	p.expandedMarker = ""
	p.expandedImageID = 0
	p.expandedRect = Rect{}
	if ctx != nil && ctx.InvalidateAll != nil {
		ctx.InvalidateAll()
	}
	return true
}

func (p *PromptEditor) ToggleInlineImageAt(me MouseEvent, ctx *Ctx) bool {
	if me.Button != MouseButtonLeft || me.Action != MousePress {
		return false
	}
	return p.toggleInlineImageExpansion(me.X, me.Y, ctx)
}

func (p *PromptEditor) toggleInlineImageExpansion(x, y int, ctx *Ctx) bool {
	if p.expandedMarker != "" {
		return p.CloseExpandedInlineImage(ctx)
	}
	for _, hit := range p.inlineImageHits {
		if !pointInRect(x, y, hit.rect) {
			continue
		}
		if p.openInlineImageOverlay(hit.marker, ctx) {
			return true
		}
		return false
	}
	return false
}

func (p *PromptEditor) openInlineImageOverlay(marker string, ctx *Ctx) bool {
	if ctx == nil || ctx.ShowOverlay == nil {
		return false
	}
	img := p.inlineImages[marker]
	if img == nil {
		return false
	}
	overlay := newPromptImageOverlay(img)
	overlay.hiddenImageIDs = p.suppressInlineImagePreviews(ctx)
	ctx.ShowOverlay(rovel.OverlayOpts{
		Root:  overlay,
		Modal: true,
		Place: EdgeAnchored{Anchor: AnchorCenter, Width: Percent(100), Height: Percent(100)},
		OnDismiss: func() {
			p.deletedImageIDs = append(p.deletedImageIDs, overlay.imageID)
			p.restoreInlineImagePreviews(ctx)
		},
	})
	return true
}

type promptImageOverlay struct {
	id             ID
	rect           Rect
	image          *Image
	imageID        uint32
	hiddenImageIDs []uint32
}

func newPromptImageOverlay(img *Image) *promptImageOverlay {
	return &promptImageOverlay{id: NewID(), image: img, imageID: AllocateImageID()}
}

func (p *promptImageOverlay) ID() ID        { return p.id }
func (p *promptImageOverlay) Rect() Rect    { return p.rect }
func (p *promptImageOverlay) Layout(r Rect) { p.rect = r }
func (p *promptImageOverlay) MinSize() Size { return Size{W: 1, H: 1} }

func (p *promptImageOverlay) Paint(d Drawer, _ *Ctx) {
	if p == nil || p.image == nil || p.rect.W <= 0 || p.rect.H <= 0 {
		return
	}
	rd, ok := d.(interface{ DrawRaw(Point, string) })
	if !ok {
		return
	}
	cols, rows := imageCellsForBounds(ImageDimensions{WidthPx: p.image.Width, HeightPx: p.image.Height}, p.rect.W, p.rect.H)
	x := p.rect.X + max(0, (p.rect.W-cols)/2)
	y := p.rect.Y + max(0, (p.rect.H-rows)/2)
	for _, id := range p.hiddenImageIDs {
		rd.DrawRaw(Point{X: x, Y: y}, DeleteKittyImage(id))
	}
	p.hiddenImageIDs = nil
	rd.DrawRaw(Point{X: x, Y: y}, DeleteKittyImage(p.imageID))
	seq, ok := RenderTerminalImage(p.image.base64Data, ImageDimensions{WidthPx: p.image.Width, HeightPx: p.image.Height}, ImageRenderOptions{MaxWidthCells: cols, MaxHeightCells: rows, ImageID: p.imageID, Filename: p.image.Filename})
	if ok {
		rd.DrawRaw(Point{X: x, Y: y}, seq.Sequence)
	}
}

func (p *promptImageOverlay) Handle(e Event, ctx *Ctx) bool {
	me, ok := e.(MouseEvent)
	if !ok || me.Button != MouseButtonLeft || me.Action != MousePress {
		return false
	}
	if ctx != nil && ctx.DismissOverlay != nil {
		ctx.DismissOverlay()
	}
	return true
}

func pointInRect(x, y int, r Rect) bool {
	return x >= r.X && x < r.X+r.W && y >= r.Y && y < r.Y+r.H
}

func (p *PromptEditor) HandleAction(act Action, ctx *Ctx) bool {
	if p == nil {
		return false
	}
	return p.ensure().HandleAction(act, ctx)
}

func (p *PromptEditor) Focusable() bool { return true }
func (p *PromptEditor) IsTextInputMode() bool {
	if p == nil {
		return false
	}
	return p.ensure().IsTextInputMode()
}
