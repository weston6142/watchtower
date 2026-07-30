The setup inspector (`f` from the grid, `internal/tui/setup.go` plus
`setup_outline`/`setup_prompt` in `internal/proto/server.go`) reports **what the
daemon is running**, not what the files on disk say. It reads the daemon's
cached flows and packages and never stats a file, so an edit to `config.yaml`,
a flow, or a `prompt.md` shows up only after a daemon restart. The load stamp in
the header exists to make that lag readable rather than confusing. What the
panel reports about agents is resolved elsewhere — see
agent-prompt-and-model-resolution.

**A repo-level field is blank unless `main.go` hands it over.**
`srv.SetRepoSetup(...)` in `runDaemon` is the only writer of `proto.RepoSetup`,
and it is deliberately one setter for all of it. Add a field to `RepoSetup`
without adding it to that call and it renders empty — no error, no test failure,
just a missing value on screen. This is the same multi-site sweep
adding-an-event describes for events. The values passed there are
post-flag-override on purpose, including the workspace provider, which
`workspace.Detect` picks off `PATH` and which nothing else can report.

**No render path may call `time.Now()`,** or the goldens stop being
byte-comparable. `LoadedAt` is formatted to a string once, at startup, and the
TUI fixtures pin it for the same reason.

Four rules hold for any new overlay, not just this one:

- **It has to be ordered against the pager arms in two places.**
  `Model.Update`'s key handling and `Model.View` are each a single if/else-if
  chain. The setup arms sit above the pager arms and both carry
  `&& m.pager.Mode == ""`; without it the overlay swallows `j`/`k`/`esc` and the
  pager it opens never renders. This is not defensive code — drop the guard and
  the prompt body is unscrollable and invisible.
- **An overlay that opens a pager with no `Files` behind it needs its own `esc`
  arm.** `updatePagerKey`'s `esc` otherwise falls back to the artifact list and
  lands the operator in an empty "no artifacts" box. The setup arm clears the
  pager and leaves `Sel`/`Top` alone so `esc` returns to the same outline row.
- **An arm handling an async response must re-check the overlay is still open
  before acting.** `f` closes the panel, and the socket round trip it started
  can land afterwards.
- **`overlayCenter` degrades to `lipgloss.Place` — dropping the dimmed base,
  silently — once the box reaches the terminal's full width or height.** So an
  overlay pins its chrome rows to stay shorter than the terminal, and pins its
  content column so a long value can never reflow the panel out of the
  100-cell `narrow` snapshot. An overlay that means to grow with the viewport
  instead of hugging its content has more to get right — see
  tui-overlay-sizing.

Consequence for tests: `internal/tui/testdata/setup-*.golden` are geometry, not
prose. Regenerate with `-update` deliberately and read the diff, the discipline
the help goldens already need.
