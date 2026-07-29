Overlays in `internal/tui` hug their content because they are prompts; the
backlog is the one browsing surface, and `renderBacklog` takes width *and*
height from the viewport instead. Any future overlay that wants to grow the same
way inherits the three constraints below.

- **An overlay that reaches the full terminal width or height loses the overlay
  look entirely.** `overlayCenter` falls back to `lipgloss.Place` at that point,
  and the fallback drops the dimmed base along with it — silently, no error. So a
  viewport-sized box must reserve margin rather than fill; the backlog's chrome
  constants exist for this and are not cosmetic slack.
- **`renderBox` sizes to its widest content line, so a viewport-sized pane has to
  pad every line it emits.** One short line anywhere in the content collapses the
  whole frame back to content width. This is why the backlog pads through
  `padCell`/`padStyled` even where the text would look fine unpadded.
- **Height reaches the render function as `Model.Height`, and `0` is a real
  value** — it means no `WindowSizeMsg` has landed yet, at startup or in a test
  that constructs a `Model` directly. A height-aware overlay needs a fallback for
  that case, not a division by zero or a one-row box.

Consequence for tests: **the backlog goldens cannot catch a height regression.**
`TestSnapshots` renders at fixed sizes (200x50 and 100x40), and the backlog
fixture holds few enough drafts that the panes stay content-sized at both — so
`backlog-wide.golden` and `backlog-narrow.golden` are byte-identical whether the
height argument arrives or is dropped on the floor. Height plumbing needs a
separate `View()`-level assertion, which is what
`TestViewPlumbsHeightIntoBacklog` is; deleting it as redundant with the goldens
would leave the wiring untested. Width does move the goldens, so regenerate with
`-update` deliberately and read the diff — the same discipline the help goldens
need in adding-an-event.
