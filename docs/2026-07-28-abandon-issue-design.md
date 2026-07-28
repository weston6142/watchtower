# Abandon Issue + Kill Guard — Design

**Date:** 2026-07-28
**Status:** Approved

## Problem

Two gaps surfaced while operating the control room:

1. Pressing `x` (kill) on a lane with nothing running walks through the kill
   confirm and then errors from the daemon ("issue GH-1 has no running
   stage"). Kill is only meaningful for an in-flight stage; the TUI should
   know that before asking.
2. There is no way to remove a lane for good. `c` (retire) only hides
   *shipped* lanes, and only in this TUI session's memory — a failed or
   unwanted issue haunts the tower, the parked shelf, and the overview counts
   forever, across restarts.

## Design

### 1. Kill guard (TUI only)

In the `x` handler (`internal/tui/app.go`), open the kill confirm only when
the focused issue has something in flight (`State == "running"` or
`"waiting_decision"`). Otherwise dock a keybar hint via the existing `m.Err`
slot: `nothing running — R retries · X abandons`. No daemon round-trip.

### 2. Durable abandon

- **Event:** `EvIssueAbandoned EventType = "issue_abandoned"` in
  `internal/core`.
- **Engine:** `Abandon(issueID string) error` — unknown issue is an error;
  if a stage is running, reuse the KillStage mechanics (set killRequested,
  cancel `stageCancel`, close the issue's pending decisions as `"killed"`);
  remove the issue from `e.issues`; emit `EvIssueAbandoned`. Rehydrated
  (failed/terminal) issues abandon without any cancel step.
- **Steward:** `EvIssueAbandoned` → durable issue state `"abandoned"`.
- **Rehydrate:** treat `"abandoned"` as terminal — never rebuilt, never
  marked failed again.
- **Overview:** abandoned issues do not count as failing/building anywhere
  the daemon computes counts from issue states.
- **Projection:** on `issue_abandoned`, delete the issue from `Issues`,
  remove it from `Order`, `ShippedToday`, and `Parked`, and drop its
  decisions — the lane disappears from every surface.
- **Proto/CLI:** op `"abandon_issue"` on the server; CLI verb
  `guildhall abandon <issue-id>` alongside pause/resume/kill/retry.
- **TUI:** `X` on a focused lane opens the existing confirm box —
  `abandon <title>? the lane is removed for good` — and on `y` sends
  `abandon_issue`. Help overlay CONTROL group gains `X abandon lane`.

## Non-goals

- No undo/un-abandon; the event log keeps history, but no UI resurrects it.
- No deletion of DB rows or artifacts — abandon is a state, not a purge.
- Workspace/worktree cleanup for abandoned issues stays out of scope (same
  TODO as rehydration).

## Testing

- Engine: abandon a rehydrated failed issue → removed from map,
  `issue_abandoned` emitted, store state `abandoned`, second `Rehydrate()`
  does not resurrect it; abandon a running issue → stage cancelled, pending
  decisions closed; abandon unknown issue → error.
- Projection: `issue_abandoned` removes the lane, parked entry, and decisions.
- TUI: `x` on a failed lane → hint, no confirm; `X` → confirm → command sent;
  goldens regenerated only if fixture output changes (the new hint text does
  not appear in fixtures).
- `go test ./...` green; live check via rebuild + abandoning the real GH-1 if
  the user chooses.
