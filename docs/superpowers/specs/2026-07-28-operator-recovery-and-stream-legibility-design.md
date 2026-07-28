# Operator Recovery and Stream Legibility

**Date:** 2026-07-28
**Status:** Approved, ready for planning

## Problem

An operator watching the tower found a lane that appeared permanently stuck, with
no visible way to continue it and no way to see what its agent had been doing.

The lane was not stuck. Issue `GH-2` was parked at a legitimate between-stage
pause gate (`issue_paused`, emitted after `brainstorm` completed), with its
`runFrom` goroutine alive and waiting. Pressing `p` on the focused lane would
have resumed it. The operator could not tell, because four separate surfaces
were misreporting the state:

- `cellContentForStage` (`internal/tui/render.go:177`) tests `iv.Paused` before
  it tests the current stage, so a paused lane paints `⏸ paused` in **every**
  stage row. The lane looks globally frozen rather than parked at one point.
- Nothing was focused, so every action key answered `no lane focused` instead of
  acting.
- The footer legend says `p pause`. The help overlay (`render.go:435`) says
  `p pause / resume`. The footer is wrong.
- The header read `● all clear` while the only lane in the tower waited on a
  human.

Investigating that surfaced three further defects, each independently reachable:

1. **`Resume` can silently do nothing.** `Engine.Resume`
   (`internal/engine/engine.go:240`) closes `is.pauseGate` and returns success
   whether or not a goroutine is waiting on it. The gate is selected on only
   inside the `runFrom` stage loop. After `KillStage`, after an abort, or after
   a daemon restart rehydrates a lane, no goroutine is waiting — the close is a
   no-op reported as success.
2. **Resuming a killed lane destroys its escape hatch.** `EvIssueResumed`
   (`internal/projection/projection.go:154`) sets `Paused = false` and
   `State = "running"` but leaves `Killed` set — while the `R` retry guard
   (`internal/tui/app.go:520`) requires `State == "failed" || Killed`. The
   combination means pressing the advertised key on a killed lane changes state
   without restarting anything.
3. **The help overlay swallows no input.** `m.help` is read only in `View()`
   (`app.go:1167`, `app.go:1234`), never in `Update()`. The overlay paints over
   the screen while every key still drives the app beneath it: `esc` falls
   through to the next handler in the chain, `p` pauses a lane, `x` arms a kill
   confirm behind the overlay. The overlay's own footer advertises
   `? / esc close` — a binding it never receives.

Separately, the transcript door works but is unreadable and incomplete. It is
the only surface in the TUI that does not wear the room's chrome
(`renderTextDoor`, `internal/tui/doors.go:145`, dumps unstyled lines under a
bare `panelTitle`), and `ParseLine` (`internal/claude/stream.go`) forwards only
assistant *text* blocks. `tool_use` blocks become `KindOther` and are dropped,
so during a tool-heavy stage such as `execute` the door sits motionless — the
exact case where an operator reaches for it.

## Goals

- `p` means "make this lane run again" for every stopped state.
- The tower reports *where* a lane is parked, not merely *that* it is.
- The stream door is legible and shows tool activity.
- The topmost visible layer consumes keyboard input.

## Non-goals

- A raw stream-json firehose viewer. `watchtower transcript` and `daemon.log`
  remain the unfiltered sources.
- Converting the transcript door into a centered overlay. It stays a door.
- Any change to pause semantics. Pausing still takes effect at the next gate.

---

## Part 1 — Resume that always means "make this lane run again"

`Engine.Resume` gains a liveness check, taken under `e.mu`:

- **`is.running == true`** — a live waiter is at the gate. Close `pauseGate` and
  nil it. Unchanged from today; this is the ordinary mid-flow pause.
- **`is.running == false`** — no waiter exists. Clear `pauseGate`,
  `killRequested`, and `terminal`, then launch the restart in a goroutine.

The restart re-enters at **`is.stageIdx`** — the stage the lane stopped on, not
the one after it. A killed `execute` stage restarts `execute`. `stageIdx` is
already assigned at the top of the stage loop (`engine.go:737`), so the resume
point needs no new bookkeeping.

`Resume` keeps its current signature — `Resume(issueID string) error` — and
still returns synchronously, so `proto/server.go:129` continues to surface
validation errors in the response. The restart runs detached, mirroring the
shape `RetryStage` already has and matching how `start_issue` and `retry_stage`
are dispatched at `proto/server.go:118` and `:139`:

```go
go func() {
    err := e.runFrom(context.Background(), is, startIdx)
    e.mu.Lock()
    is.terminal = err != nil
    e.mu.Unlock()
}()
```

Failures surface as `stage_failed` events, as they do for start and retry.

Clearing `killRequested` is safe at this point. Its only reader is `wasKilled`
(`engine.go:327`), called at `engine.go:652` and `:663` to decide whether a
cancelled stage emits `EvStageKilled` or a plain failure. Both calls happen
inside `runStage`, synchronously, before `runFrom` returns and sets
`running = false` — so by the time `Resume` observes a stopped lane, the
previous stage's teardown has already read the flag. `RetryStage` clears it at
the same point for the same reason (`engine.go:838`).

`Resume` continues to return an error for an unknown issue. It no longer returns
`"issue %s is not paused"` when `pauseGate` is nil but the lane is stopped —
that is now the case it handles. It still errors when the lane is running
normally with no gate, since there is nothing to resume.

`Resume` and `RetryStage` now overlap: both restart a stopped lane at
`stageIdx`. They stay separate verbs. `RetryStage` requires `is.terminal` and is
the operator's word for "that failed, try again"; `Resume` accepts any stopped
lane and is the word for "keep going". The shared restart body is extracted into
one unexported helper so the two cannot drift.

`EvIssueResumed` in the projection additionally clears `Killed`, so a lane that
resumes and later fails does not carry a stale kill flag into the `R` guard.

### Tests

- Resume with a live waiter: gate closes, the following stage runs.
- Resume after `KillStage`: the killed stage restarts, not its successor.
- Resume on a rehydrated lane with no goroutine: the lane runs.
- Resume on a normally-running lane with no gate: returns an error.
- Projection: `EvIssueResumed` clears `Killed` as well as `Paused`.

---

## Part 2 — The tower stops lying about paused lanes

Render-layer only; no state changes.

1. **`render.go:177`** — the `iv.Paused || iv.Killed || iv.State == "paused"`
   branch stays where it is in the precedence order but gains a
   `iv.CurrentStage == stage` guard. Stages other than the current one fall
   through to the completed/waiting logic below, so a paused lane shows `✓` on
   finished stages, `⏸ paused` on the one it is parked at, and dim glyphs after.

   The guard cannot simply be dropped in favor of *moving* the branch below the
   other state checks. Those checks all test `iv.State` against `"running"`,
   `"waiting_decision"`, or `"failed"`, and `EvIssuePaused` sets
   `iv.State = "paused"` — so none of them match a paused lane, and reordering
   alone would make the `⏸ paused` marker vanish from every cell rather than
   narrow to one.

2. **`EvIssuePaused` gains a payload.** The render rule above needs
   `iv.CurrentStage` to name the stage the lane is parked *at*, and today it
   does not. `EvStageCompleted` (`projection.go:128`) appends to `Completed`
   without advancing `CurrentStage`, and `EvIssuePaused` is emitted with a nil
   payload (`engine.go:742`). For `GH-2` right now, `CurrentStage` is still
   `brainstorm` — a stage that is also in `Completed`, so the marker and the `✓`
   would collide on one cell.

   The engine emits `EvIssuePaused` with `{"stage": f.Stages[i].Name}`, the
   upcoming stage it is about to gate on, and the projection sets
   `iv.CurrentStage` from that payload when present. Events already in the log
   carry no payload; the projection leaves `CurrentStage` untouched for those.

   As a safety net for rehydrated and legacy lanes, if `CurrentStage` is empty
   or already appears in `Completed`, the renderer marks the first
   non-completed stage instead.
3. **Footer legend** — `p pause` becomes `p pause/resume`, matching the help
   overlay. `T transcript` is added; it is currently absent from the footer
   entirely.
4. **Notice row** — a lane parked with no pending decision is not `all clear`.
   The notice row surfaces `GH-2 parked at spec — p resumes`, naming the stage
   from item 2. Naming where it will *resume* rather than what last finished
   describes the thing the operator is about to act on.
5. **`T` with no focus** — joins the key list at `app.go:540` that answers
   `no lane focused — press j or 1-9 to focus`. Today `fetchTranscript` returns
   nil when `m.Focus.Issue == ""`, opening an empty door with no explanation.

### Tests

- Snapshot: a paused lane's grid row shows `✓` on completed stages and `⏸
  paused` on the current one only. Existing snapshots in `render_test.go` and
  `snapshot_test.go` encode the current all-rows-paused output and will need
  updating; review that diff rather than regenerating it blind.
- A paused lane whose `CurrentStage` is empty or already completed marks the
  first non-completed stage.
- Projection: `EvIssuePaused` with a stage payload advances `CurrentStage`; the
  same event with a nil payload leaves it unchanged.
- Snapshot: the notice row with one parked lane and no decisions.
- `T` with no focus sets the error message and opens no door.

---

## Part 3 — The stream door, boxed and legible

### Chrome

The transcript stays a **door**, not an overlay: it keeps its place on the
`m.modes` stack and its already-working `esc`-pops-back. Only its rendering
changes — from `renderTextDoor` to `renderBox`, the same helper the help overlay
uses:

- Bordered box, bright title `Transcript`.
- Subtitle carrying the lane and stage: `GH-2 · brainstorm`.
- `esc close` corner badge.
- Rule and footer key chips.

This gives the modal appearance without forking door navigation.

### Typography

The body is currently raw lines in a single color. It becomes:

- Stage gutter (`brainstorm │`) at `Dim`.
- Agent prose at `Text`.
- `— turn complete (13560 tokens) —` promoted from a line of prose into a real
  dim rule.

### Tool calls

`rawLine.Message.Content` gains `Name` and `Input` fields. `ParseLine` emits a
new `KindToolUse` for `tool_use` content blocks instead of letting them fall
through to `KindOther`. `CodeRunner` forwards them to `OnLine` in stream
position, formatted as one summarized line each:

```
↳ Bash go test ./...
↳ Read internal/engine/engine.go
↳ Edit render.go
```

The summary is the tool name plus a single-line rendering of its most
identifying input field, truncated to fit. Unrecognized input shapes render the
tool name alone rather than raw JSON. Tool lines render at `Dim` so prose stays
foreground and activity reads as texture beneath it.

`tool_result` blocks are not surfaced. The intent is to show what the agent is
*doing*, not to replay its inputs.

### Tests

- `ParseLine` table tests: `tool_use` with a recognized input shape, with an
  unrecognized one, and with malformed input that must not panic.
- A message mixing text and `tool_use` blocks produces both, in order.
- Snapshot: the boxed door rendering mixed prose and tool lines.

---

## Part 4 — Topmost layer consumes keys

At the top of the `tea.KeyMsg` branch in `Update`, before the existing
`q` / `?` switch:

```go
if m.help {
    // ? and esc close the overlay; q and ctrl+c still quit;
    // every other key is swallowed.
}
```

Nothing beneath the help overlay can be driven while it is visible. The boxed
transcript door needs no equivalent addition — `updateDoorKey` already consumes
input for the door stack — but its `esc` behavior is verified by test alongside
the help fix, since the two are the same rule.

### Tests

- `p` dispatched while `m.help` is set issues no command and leaves the focused
  lane's state untouched. This is the regression that let the bug stay invisible.
- `esc` while `m.help` is set closes the overlay and issues no command.
- `q` while `m.help` is set still quits.
- `esc` in the boxed transcript door pops back to the grid.

---

## Sequencing

Part 1 is an engine correctness fix and stands alone. Part 4 is a small
input-routing fix and stands alone. Parts 2 and 3 are render-layer work and
share no code with each other. All four can be implemented in any order;
Part 1 before Part 2 is the natural pairing, since Part 2's notice row describes
the state Part 1 makes recoverable.
