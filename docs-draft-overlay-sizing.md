Overlays in `internal/tui` hug their content, with one exception. Almost every
overlay is a prompt — a few fields you answer and dismiss — so `renderBox`
sizing itself to its widest content line is right for them. The backlog is the
one browsing surface, so `renderBacklog` takes width *and* height from the
viewport and spends the space on a list pane plus a detail pane. Any future
overlay that wants to grow inherits the three constraints below.

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

Consequence for tests: `internal/tui/testdata/backlog-*.golden` now track the
fixture's terminal dimensions rather than the drafts' content, so changing
`FixtureModel`'s width or height rewrites them. Regenerate with `-update`
deliberately and read the diff, the same discipline the help goldens need in
adding-an-event.
