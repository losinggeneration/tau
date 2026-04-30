package tui

import "github.com/losinggeneration/rovel/render"

// Buffer is a terminal cell buffer used by tests and low-level render paths.
type Buffer = render.Buffer

// Cell is one terminal buffer cell.
type Cell = render.Cell

// NewBuffer creates a cell buffer with the given dimensions.
var NewBuffer = render.NewBuffer

// NewRenderPainter creates a low-level render painter over a buffer.
var NewRenderPainter = render.NewPainter
