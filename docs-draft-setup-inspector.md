The setup inspector (`f` from the grid, `internal/tui/setup.go` +
`setup_outline`/`setup_prompt` in `internal/proto/server.go`) reports **what the
daemon is running**, not what the files on disk say. It reads the daemon's
cached flows and packages and never stats a file, so an edit to `config.yaml`,
a flow, or a `prompt.md` shows up only after a daemon restart. The load stamp in
the header exists to make that lag readable rather than confusing.

**A repo-level field is blank unless `main.go` hands it over.**
`srv.SetRepoSetup(...)` in `runDaemon` is the only writer of `proto.RepoSetup`,
and it is deliberately one setter for all of it. Add a field to `RepoSetup`
without adding it to that call and it renders empty — no error, no test failure,
just a missing value on screen. This is the same multi-site sweep
adding-an-event describes for events. The values passed there are
post-flag-override on purpose, including the workspace provider, which
`workspace.Detect` picks off `PATH` and which nothing else can report.
`LoadedAt` is formatted to a string once, at startup, for a related reason: no
render path may call `time.Now()` or the goldens stop being byte-comparable.

**A new overlay has to be ordered against the pager arms in two places.**
`Model.Update`'s key handling and `Model.View` are each a single if/else-if
chain. The setup arms sit above the pager arms and both carry
`&& m.pager.Mode == ""`; without it, the overlay swallows `j`/`k`/`esc` and the
pager it opens never renders. This is not defensive code — drop the guard and
the prompt body is unscrollable and invisible.

The same applies to leaving that pager. `updatePagerKey`'s `esc` normally falls
back to the artifact list, so an overlay that opens a pager with no `Files`
behind it needs its own `esc` arm or the operator lands in an empty "no
artifacts" box. The setup arm clears the pager and leaves `Sel`/`Top` alone so
`esc` returns to the same outline row.

Consequence for tests: `internal/tui/testdata/setup-*.golden` are geometry, not
prose. The outline's content column is pinned at `setupRowWidth = 90` so a long
tools list can never reflow the panel, and 90 + 4 cells of box chrome is what
keeps it inside the 100-cell `narrow` snapshot; `setupChromeRows = 10` is what
keeps the box shorter than the terminal, which matters because `overlayCenter`
degrades to `lipgloss.Place` — dropping the dimmed base, silently — once the box
reaches the terminal's height. `fixtureSetup` also pins the load time
for the same reason the render paths avoid clocks. Regenerate with `-update`
deliberately and read the diff, the discipline the help goldens already need.
