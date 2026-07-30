Overlays in `internal/tui` hug their content, because they are prompts. The
backlog is the exception — a browsing surface, so `renderBacklog` takes width
*and* height from the viewport. Any future overlay that wants to grow the same
way inherits three constraints, none of which fail loudly.

- **An overlay that reaches the full terminal width *or* height loses the
  overlay look entirely.** `overlayCenter` falls back to `lipgloss.Place` at
  that point, and the fallback drops the dimmed base with it — silently, no
  error. So a viewport-sized box reserves margin rather than fills; the
  backlog's chrome constants are that margin, not cosmetic slack.
- **`renderBox` sizes to its widest content line, so a viewport-sized pane has
  to pad every line it emits.** One short line anywhere in the content collapses
  the whole frame back to content width. That is why the backlog pads through
  `padCell`/`padStyled` even where the text would look fine unpadded.
- **Height reaches the render function as `Model.Height`, and `0` is a real
  value** — it means no `WindowSizeMsg` has landed yet, at startup or in a test
  that constructs a `Model` directly. A height-aware overlay needs a fallback
  for that case, not a division by zero or a one-row box.

Consequence for tests: **a golden only catches a height regression if its
fixture is deeper than the terminal.** `TestSnapshots` renders at fixed sizes
(200x50 and 100x40), so a content-sized fixture like `backlog` produces
byte-identical goldens whether the height argument arrives or is dropped on the
floor. Only a fixture deep enough to clip moves with it, and only at the size
where it clips: `backlog-long` clips at 40 rows, so its *narrow* golden tracks
height while its wide one does not. Keep both kinds of fixture. Height plumbing still wants a separate `View()`-level assertion
(`TestViewPlumbsHeightIntoBacklog`), because a golden proves the number reached
the sizing arithmetic, not that `View()` read it from the model. Width moves
every backlog golden, so regenerate with `-update` deliberately and read the
diff — the discipline adding-an-event asks for on the help goldens.

Sizing is geometry only. The lifecycle rules every overlay also owes — ordering
against the pager arms, its own `esc` arm, re-checking it is still open when an
async response lands — are in setup-inspector.
