package tui

// CursorMarker is a zero-width marker emitted by tau prompt-style row
// components to indicate the desired cursor location. App/render integration
// should map this to rovel cursor/focus behavior instead of depending on a
// tau-owned renderer implementation.
const CursorMarker = "\x00TAU_CURSOR\x00"
