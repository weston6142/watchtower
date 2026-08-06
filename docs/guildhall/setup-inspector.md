The setup inspector (`f` from the grid, `internal/tui/setup.go` plus
`setup_outline`/`setup_prompt` in `internal/proto/server.go`) reports **what the
daemon is running**, not what the files on disk say. It reads the daemon's
cached flows and packages and never stats a file, so an edit to `config.yaml`,
a flow, or a `prompt.md` shows up only after a daemon restart. This includes
runner, provider binary, Codex model/effort defaults, and package settings. The
header reports the selected provider binary (`codex_bin` or `claude_bin`); for
Codex it also reports the repository model/effort pair. The load stamp makes
stale daemon state readable rather than confusing. What the panel reports
about agents is resolved elsewhere — see agent-prompt-and-model-resolution.

Repository checks follow the same cached-config rule. `checks` in
`.watchtower/config.yaml` names repository-owned commands, while each stage's
`verify_after_change` in the flow selects a check or the reserved `test_cmd`.
The flow file's stage list is also the execution order; Watchtower validates
these references at daemon startup and does not infer commands or reorder the
repository's workflow.

## Codex profiles and fallback

New repositories use a Codex primary profile with an explicit empty feature map
and terminal policy:

```yaml
codex:
  primary:
    feature_overrides: {}
```

The only supported feature override is the boolean `unified_exec`. An opt-in
fallback keeps the same binary, model, and effort and changes only the
supported feature state. With no fallback the effective policy is `terminal`;
adding a fallback opts the operation into the bounded `fallback_once` policy:

```yaml
codex:
  primary:
    feature_overrides:
      unified_exec: false
  fallback:
    feature_overrides:
      unified_exec: true
```

Unknown feature names, non-boolean values, missing profile data, mismatched
profile identity, and an identical primary/fallback pair are rejected before a
process launches. A configured fallback is reserved and attempted at most
once for each runner operation. A daemon restart records an interrupted
operation as terminal and exposes the existing manual retry path; it never
loops the primary or duplicates a reserved/consumed fallback automatically.

The setup panel reports the daemon's cached normalized profiles, effective
feature overrides, fallback policy, and redacted initial/resumed argv shape.
Prompts, developer instructions, environment values, access or resume tokens,
and raw stderr are omitted from setup, stored attempt metadata, lifecycle
events, and terminal errors. Restart the daemon after editing configuration so
the cached view and the runner use the new profiles.

The inspector describes loaded configuration, not lane completion. In
particular, a completed merge-verifier transcript does not make an issue done.
Final-stage completion requires strict `merge-decision.json` and
`verification.json` validation plus the durable `verification_ready`
checkpoint described in lane-ops-and-issue-states. When `test_cmd` is
configured, the daemon authors `verification.json` after the merge-barrier
stage; repositories without a `test_cmd` retain the agent-authored receipt
path. Restart the daemon after any provider, config, prompt, or flow edit so
the verifier receives the current contract.

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
TUI fixtures pin it for the same reason. The shipped shelf's day cutoff obeys the
same rule: `Model.dayStart` is injected on the tick, never read where anything
renders, and `FixtureModel` pins it from a fixed `fixtureNow`. Keep the
fixture's merge offset inside `fixtureNow`'s calendar day — push it across the
boundary and `ml-retry` leaves the shelf, churning many goldens at once.

Five rules hold for any new overlay, not just this one:

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
- **A surface that polls holds its reading position by gating the poll, and
  gating the fetch is not enough on its own.** `m.doorLines` is replaced
  wholesale from an evicting ring buffer, so an offset alone keeps pointing at
  content that moved — the stream door freezes the refetch instead, through the
  single `followingTranscript()` predicate, because the transcript has *two*
  refetch sites (the `tickMsg` arm and the `Msg` arm) that must not disagree. One
  response is still in flight when the operator scrolls away, so the
  `transcriptMsg` arm discards a late reply while the door is detached and
  already holds lines — checking the current mode first, since a reply landing
  under a different door is not this arm's to eat. And because
  `streamState.Follow` is *derived* from the clamped `Top`, its zero value means
  detached at row 0, not following: `T`, `popMode` and the fixtures each say
  `Follow: true` explicitly, and a site that forgets renders the oldest rows.
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
