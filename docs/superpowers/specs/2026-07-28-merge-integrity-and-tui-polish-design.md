# Merge Integrity and TUI Polish — Design

Date: 2026-07-28
Status: approved

## Problem

Eight operator-reported issues, one severe:

1. **Merge stage reports success without landing work.** GH-2 completed with
   `issue_merged`/`issue_completed` emitted, but its four commits sit unreachable
   in a detached-HEAD treehouse worktree. `develop` never received the code.
2. Artifacts view looks like plain text, unrelated to the decision view, and the
   way back to the tower is not discoverable.
3. The `g` "war room" hotkey appears to do nothing.
4. The timeline door shows "0 lines".
5. The right-side focus rail does not read as a separate layer.
6. Tool-heavy stages (e.g. PLAN) show an apparently empty transcript.
7. No visibility or control over which model/thinking effort stages run with.

## 1. Merge integrity (engine + workspace)

### Root cause

`workspace.Treehouse.Acquire` (internal/workspace/workspace.go) leases a pool
worktree via `treehouse get --lease` and returns the path as-is. Treehouse
worktrees are detached HEAD. The engine later resolves the branch with
`git rev-parse --abbrev-ref HEAD` (internal/engine/engine.go ~783), gets the
literal `"HEAD"`, which is non-empty, and `marshal.Train.Land` runs
`git merge --no-ff --no-edit HEAD` against the base — "Already up to date",
success, `issue_merged` emitted, nothing lands. Because `Detect` prefers
Treehouse whenever the binary is on PATH, **every issue** on such a machine
hits this path. The `GitWorktree` fallback creates `issue/<id>` via
`worktree add -b` and is unaffected.

### Fix (three layers)

1. **Root cause:** `Treehouse.Acquire` checks out a branch `issue/<issueID>`
   in the leased worktree immediately after acquiring it
   (`git -C <path> checkout -b issue/<id>`; if the branch already exists from a
   prior lease of the same issue, `checkout` it and reset to the leased tip).
   Release behavior unchanged; the branch remains available for the merge.
2. **Guard:** in `Engine.runFrom`, if the resolved worktree branch is empty or
   the literal `HEAD`, do not call `Land`. Emit a merge failure and route
   through the existing merge-conflict escalation path (retry / leave-unmerged
   decision). `issue_merged` is never emitted.
3. **Verify:** after `Land` returns success, run
   `git merge-base --is-ancestor <worktree-tip> <base>` from the main repo.
   Only emit `issue_merged` (and subsequent `issue_completed` with merged
   semantics) when it passes. On failure, treat as a land failure → same
   escalation path as (2).

No new lane state: an unverifiable merge is a failed merge, which existing lane
states already surface.

### Testing

- Workspace test: Treehouse acquire leaves the worktree on `issue/<id>`
  (mock/fake treehouse binary as the existing tests do).
- Engine table tests: branch = `HEAD` → escalation, no `issue_merged`;
  Land success + ancestor check false → escalation; ancestor check true →
  `issue_merged` emitted.

## 2. Artifacts view styling + navigation affordance

`renderArtifactList` (internal/tui/pager.go) is restyled to match the decision
view: bordered `renderBox` chrome (same as stream doors), themed title, a
highlighted selection row, and an explicit hint line
`enter open · esc back to tower`. Navigation is unchanged (`esc` already
returns to the grid); the fix is discoverability and visual coherence.

## 3. War room hotkey `g`

`g` gets a real handler: it jumps focus to the war-room summary line and
expands it in place — the summary line highlighted plus a second breakout line
(full shipping order, builder utilization, ideas/questions counts). Pressing
`g` again or moving focus collapses it. No separate door. Help text stays
`g — war room`.

## 4. Timeline door (minimal fix)

- Refresh `doorLines` on tick while the timeline door is open, mirroring the
  transcript door's re-fetch (app.go ~194-201).
- When no lane is focused, show the existing no-lane-focused message instead
  of an empty door. Event-type coverage in `humanizeEvents` is unchanged.

## 5. Focus rail box

Wrap the right rail (internal/tui/rail.go `renderRail`) in the same bordered
box style used by doors/modals, titled `FOCUS`. It remains a normal column in
the horizontal join — same z layer, not an overlay. Width budget accounts for
the border (inner content width shrinks by 2).

## 6. Transcript visibility for tool-heavy stages

Transcripts stay in-memory (no persistence across daemon restarts).

- Tool-call `↳` lines render at normal intensity with the tool name accented
  (drop the Dim style in `renderStreamDoor`).
- Stream door subtitle becomes `N lines · M tool calls` (count `↳` lines).
- When the buffer is empty but the stage is running, the placeholder reads
  "agent is working — transcript may have been lost to a daemon restart"
  rather than "the agent has not spoken this stage".

## 7. Model and thinking effort

- Package YAML: keep `model`, add `effort`. Shipped packages get explicit
  values (model set deliberately, not blank-means-CLI-default).
- Runner (internal/claude/runner.go): pass `--model` when set (existing), and
  wire `effort` through whatever mechanism the installed CLI supports (flag or
  settings/env) — exact mechanism verified at plan time.
- TUI: the focus rail and the stream door header show the model and effort the
  focused lane's current stage runs with (plumbed through `proto.IssueDetail`).
  When unset, display `model: cli default` honestly.

## Testing overview

- Engine/workspace: table-driven tests with the existing fake-git harness.
- TUI: snapshot tests for artifacts box, war-room expansion, focus rail
  border, stream door subtitle/placeholder, rail model line.
- Runner: flag-assembly test asserting `--model`/effort wiring.

## Out of scope

- Transcript persistence to disk.
- Expanded timeline event coverage / global timeline.
- A dedicated war-room door.
- New lane state for unverifiable merges.
