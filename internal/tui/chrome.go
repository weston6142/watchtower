package tui

// State glyphs — the ONLY state vocabulary any surface may use.
const (
	glyphDone    = "●"
	glyphNeedYou = "◔"
	glyphWorking = "◐"
	glyphWaiting = "○"
	glyphFailed  = "✕"
	glyphCursor  = "▸"
	glyphShipped = "⇡"
	glyphParked  = "⏸"
)

// Spacing — renderers use these, never literal spacing.
const (
	padV   = 1 // blank lines inside a panel
	padH   = 2 // cells of horizontal panel padding
	gutter = 2 // cells between adjacent panels
)
