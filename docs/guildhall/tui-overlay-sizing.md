Overlays in `internal/tui` hug their content, because they are prompts. Two
surfaces are exceptions, because they are read rather than answered:
`renderBacklog` and `renderStreamDoor` take width *and* height from the viewport
and window their content to fit. Every other door still hugs — and
`renderTextDoor` (timeline, decisions, tray, shelf) is therefore still unbounded
in height, so a long-lived lane's timeline overflows the terminal. That is a
known gap, not a design choice. Any surface that wants to grow with the viewport
inherits four constraints, none of which fail loudly — plus a fifth that is
screen chrome and holds everywhere. Doors bypass `overlayCenter`, so the first
applies to overlays only.

- **An overlay that reaches the full terminal width *or* height loses the
  overlay look entirely.** `overlayCenter` falls back to `lipgloss.Place` at
  that point, and the fallback drops the dimmed base with it — silently, no
  error. So a viewport-sized box reserves margin rather than fills; the
  backlog's chrome constants are that margin, not cosmetic slack.
- **`renderBox` sizes to its widest content line, so a viewport-sized pane has
  to pad every line it emits.** One short line anywhere in the content collapses
  the whole frame back to content width. That is why the backlog pads through
  `padCell`/`padStyled` even where the text would look fine unpadded.
- **Once content is *windowed*, padding is necessary but not sufficient: the
  widest row in the visible window sets the frame, so over-wide rows have to be
  truncated too.** Unpadded the box shrinks; over-wide it grows — either way it
  wobbles as the window moves, taking any `inner`-relative footer arithmetic with
  it, silently. So every row `streamBody` emits is *exactly* `inner` cells:
  wrapped or truncated, then `padStyled`. Truncate the **plain** text before
  styling it — cutting an already-styled string slices an escape sequence and
  bleeds colour into the rest of the row. Both the stage gutter and an unwrapped
  tool-call row can exceed a narrow `inner` on their own.
- **Height reaches the render function as `Model.Height`, and `0` is a real
  value** — it means no `WindowSizeMsg` has landed yet, at startup or in a test
  that constructs a `Model` directly. A height-aware overlay needs a fallback
  for that case, not a division by zero or a one-row box. `streamRows` borrows
  `backlogFallbackRows` rather than spelling a second literal, floors at one body
  row, and subtracts a hand-derived `streamChromeRows`; the width fallback lives
  once in `Model.layoutWidth()` so a key arm and `View` cannot size to different
  numbers.
- **Anything routed into the keybar is a reserved single row and must have its
  newlines collapsed.** `chromeBar` caps width through `MaxWidth` but cannot cap
  height, and `m.Err` carries `err.Error()` straight from the daemon, where an
  error wrapping a command's `CombinedOutput` is routinely multi-line. `errText`
  collapses them for the same reason `renderNoticeRow` does. A chrome row that
  quietly becomes two costs the header off the top of the screen.

Consequence for tests: **a golden only catches a height regression if its
fixture is deeper than the terminal.** `TestSnapshots` renders at fixed sizes
(200x50 and 100x40), so a content-sized fixture like `backlog` produces
byte-identical goldens whether the height argument arrives or is dropped on the
floor. Only a fixture deep enough to clip moves with it, and only at the size
where it clips: `backlog-long` clips at 40 rows, so its *narrow* golden tracks
height while its wide one does not. Keep both kinds of fixture. The stream door's
pair splits differently: `stream` clips at neither size, `stream-long` clips at
both — which is what licenses an *equality* assertion on the deep one.
Height plumbing still wants a separate `View()`-level assertion
(`TestViewPlumbsHeightIntoBacklog`, `TestViewPlumbsHeightIntoStreamDoor`),
because a golden proves the number reached the sizing arithmetic, not that
`View()` read it from the model. Assert `lipgloss.Height(m.View())` *equals*
`m.Height` on a fixture that clips: a chrome budget that drifts by a row then
fails loudly instead of wasting or overflowing one. Width moves every backlog
golden, so regenerate with `-update` deliberately and read the diff — the
discipline adding-an-event asks for on the help goldens.

Sizing is geometry only. The lifecycle rules every overlay also owes — ordering
against the pager arms, its own `esc` arm, re-checking it is still open when an
async response lands, and how a polling surface holds a reading position — are in
setup-inspector.
