package tui

// This file re-exports the underlying tui library's core types so consumers
// of github.com/losinggeneration/tau/tui don't need to import the underlying
// library directly. The library's API is treated as implementation detail;
// the tau/tui surface is the stable contract.

import (
	"github.com/losinggeneration/rovel"
	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/geom"
	"github.com/losinggeneration/rovel/style"
	"github.com/losinggeneration/rovel/text"
)

// View is the interface every component implements.
type View = rovel.View

// ID uniquely identifies a view within an app's tree.
type ID = rovel.ID

// NewID returns a fresh unique ID.
var NewID = rovel.NewID

// Rect is a screen rectangle in cells.
type Rect = geom.Rect

// Point is a screen position in cells.
type Point = geom.Point

// Size is a width/height pair in cells.
type Size = geom.Size

// TerminalMode controls process terminal input behavior.
type TerminalMode = rovel.TerminalMode

const (
	// ModeRaw puts the terminal into full raw mode for interactive TUIs.
	ModeRaw = rovel.ModeRaw

	// ModeCBreak puts the terminal into cbreak mode for inline components.
	ModeCBreak = rovel.ModeCBreak
)

// InputOpts describes terminal input features requested by an app.
type InputOpts struct {
	Mouse          bool
	BracketedPaste bool
	ModifiedKeys   bool
}

// RovelInputOpts converts the top-level input options to the underlying
// toolkit representation.
func (o InputOpts) RovelInputOpts() rovel.InputOpts {
	return rovel.InputOpts{
		Mouse:          o.Mouse,
		BracketedPaste: o.BracketedPaste,
		ModifiedKeys:   o.ModifiedKeys,
	}
}

// Event is the interface for all input events.
type Event = rovel.Event

// Key represents a normalized key.
type Key = event.Key

// ModMask represents keyboard modifiers.
type ModMask = event.ModMask

// KeyEvent is a key press/release event.
type KeyEvent = event.KeyEvent

// ResizeEvent is a terminal resize event.
type ResizeEvent = event.ResizeEvent

// MouseEvent is a mouse press/release/move event.
type MouseEvent = event.MouseEvent

// MouseAction is the kind of mouse interaction.
type MouseAction = event.MouseAction

// MouseButton identifies which mouse button was pressed.
type MouseButton = event.MouseButton

// PasteEvent is a bracketed-paste payload.
type PasteEvent = event.PasteEvent

// ClipboardResponseEvent is a terminal clipboard-read response.
type ClipboardResponseEvent = event.ClipboardResponseEvent

// Text measurement and wrapping helpers from the underlying toolkit.
// Re-exported so application code does not import rovel/text directly.
type (
	WrappedLine = text.WrappedLine
	WrapOptions = text.WrapOptions
	WrapMode    = text.WrapMode
)

const (
	WrapCluster = text.WrapCluster
	WrapNone    = text.WrapNone
	WrapWord    = text.WrapWord
)

var (
	TextTruncate = text.Truncate
	WrapLines    = text.WrapLines
)

// Style is a foreground/background/attribute style.
type Style = style.Style

// Color is a terminal color (default, palette index, or RGB).
type Color = style.Color

// AttrMask is a bitmask of text attributes.
type AttrMask = style.AttrMask

// ColorKindDefault is the style package's default-color kind.
var ColorKindDefault = style.ColorKindDefault

// ColorRGB constructs a true-color RGB color.
func ColorRGB(r, g, b uint8) Color {
	return style.ColorRGB(r, g, b)
}

// ColorBasic constructs a color from one of the 16 basic ANSI slots.
func ColorBasic(index uint8) Color {
	return style.ColorBasic(index)
}

// ColorIndex constructs a color from the 256-color xterm palette.
func ColorIndex(index uint8) Color {
	return style.ColorIndex(index)
}

// Capability describes the terminal's color/feature support.
type Capability = style.Capability

// Palette defines semantic theme roles.
type Palette = rovel.Palette

// Theme is a complete style set for the app.
type Theme = rovel.Theme

// Ctx is the per-call context passed to Paint and Handle.
type Ctx = rovel.Ctx

// Drawer is the backend-neutral drawing surface.
type Drawer = rovel.Drawer

// CellDrawer extends Drawer with cell-precise writes.
type CellDrawer = rovel.CellDrawer

// CellDrawerOf returns the cell-precise drawer when the underlying drawer
// supports it.
var CellDrawerOf = rovel.CellDrawerOf

// BoxStyle describes border glyphs and style.
type BoxStyle = rovel.BoxStyle

// Common box glyph presets.
var (
	BoxGlyphsLight = rovel.BoxGlyphsLight
)

// NewPainter wraps a render painter with a base style.
var NewPainter = rovel.NewPainter

// NewDrawer returns a high-level drawer for painter.
var NewDrawer = rovel.NewDrawer

// Painter provides immediate-mode drawing on the underlying buffer.
type Painter = rovel.Painter

const (
	ColorDefault       = style.ColorDefault
	ColorBlack         = style.ColorBlack
	ColorRed           = style.ColorRed
	ColorGreen         = style.ColorGreen
	ColorYellow        = style.ColorYellow
	ColorBlue          = style.ColorBlue
	ColorMagenta       = style.ColorMagenta
	ColorCyan          = style.ColorCyan
	ColorWhite         = style.ColorWhite
	ColorBrightBlack   = style.ColorBrightBlack
	ColorBrightRed     = style.ColorBrightRed
	ColorBrightGreen   = style.ColorBrightGreen
	ColorBrightYellow  = style.ColorBrightYellow
	ColorBrightBlue    = style.ColorBrightBlue
	ColorBrightMagenta = style.ColorBrightMagenta
	ColorBrightCyan    = style.ColorBrightCyan
	ColorBrightWhite   = style.ColorBrightWhite
)

const (
	AttrBold      = style.AttrBold
	AttrDim       = style.AttrDim
	AttrItalic    = style.AttrItalic
	AttrUnderline = style.AttrUnderline
	AttrBlink     = style.AttrBlink
	AttrReverse   = style.AttrReverse
)

// ID constants for key kinds.
const (
	KeyNone      = event.KeyNone
	KeyRune      = event.KeyRune
	KeyEnter     = event.KeyEnter
	KeyEsc       = event.KeyEsc
	KeyTab       = event.KeyTab
	KeyBackspace = event.KeyBackspace
	KeyCtrlC     = event.KeyCtrlC
	KeyUp        = event.KeyUp
	KeyDown      = event.KeyDown
	KeyLeft      = event.KeyLeft
	KeyRight     = event.KeyRight
	KeyF1        = event.KeyF1
	KeyF2        = event.KeyF2
	KeyF3        = event.KeyF3
	KeyF4        = event.KeyF4
	KeyF5        = event.KeyF5
	KeyF6        = event.KeyF6
	KeyF7        = event.KeyF7
	KeyF8        = event.KeyF8
	KeyF9        = event.KeyF9
	KeyF10       = event.KeyF10
	KeyF11       = event.KeyF11
	KeyF12       = event.KeyF12
	KeyHome      = event.KeyHome
	KeyEnd       = event.KeyEnd
	KeyInsert    = event.KeyInsert
	KeyDelete    = event.KeyDelete
	KeyPageUp    = event.KeyPageUp
	KeyPageDown  = event.KeyPageDown
	KeyShiftTab  = event.KeyShiftTab
)

// Modifier constants.
const (
	ModShift = event.ModShift
	ModAlt   = event.ModAlt
	ModCtrl  = event.ModCtrl
)

// Mouse action constants.
const (
	MousePress   = event.MousePress
	MouseRelease = event.MouseRelease
	MouseMove    = event.MouseMove
	MouseDrag    = event.MouseDrag
)

// Mouse button constants.
const (
	MouseButtonNone      = event.MouseButtonNone
	MouseButtonLeft      = event.MouseButtonLeft
	MouseButtonMiddle    = event.MouseButtonMiddle
	MouseButtonRight     = event.MouseButtonRight
	MouseButtonWheelUp   = event.MouseButtonWheelUp
	MouseButtonWheelDown = event.MouseButtonWheelDown
)

// ErrClosed is returned by Post/PostInvalidate when the app is shutting down.
var ErrClosed = rovel.ErrClosed

// RuneWidth returns the cell width of a single rune.
func RuneWidth(r rune) int {
	return rovel.RuneWidth(r)
}
