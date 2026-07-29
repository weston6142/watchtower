Five operator actions look similar and are routinely confused. They are not
interchangeable:

| Action | Key | Op | Scope |
|---|---|---|---|
| pause / resume | `p` (toggle) | `pause_issue` / `resume_issue` | issue keeps its place; reversible |
| kill | `x` | `kill_stage` | cancels the *running stage* only; the lane stays |
| retry | `R` | `retry_stage` | re-runs the failed/killed stage; only offered when `State == "failed"` or `Killed` |
| retire | `c` | none (TUI-local) | hides a *shipped* lane in this TUI session only; not durable |
| abandon | `X` | `abandon_issue` | removes the lane everywhere, durably, forever |

Rules that hold across the daemon:

- **`abandoned` is terminal.** `Rehydrate` skips it alongside `done`,
  `done (unmerged)`, and `merged` (`internal/engine/engine.go`). A restart must
  never resurrect an abandoned lane or re-mark it failed.
- **Abandon is a state, not a purge.** `Engine.Abandon` cancels any running
  stage, closes pending decisions as `killed`, drops the issue from
  `e.issues`, and emits `issue_abandoned`. Issue rows, events, and artifacts
  stay in the store, so an abandoned lane is still inspectable. There is no undo
  and no UI to resurrect a lane. **The one exception is attachments:** their
  bytes and rows are deleted, best-effort — see issue-attachments.
- **Worktree/workspace cleanup for abandoned issues is not implemented** — the
  same open TODO as rehydration. Don't assume a worktree was released.
- **Kill is guarded in the TUI, not the daemon.** `x` only opens the confirm
  when the focused lane is `running` or `waiting_decision`; otherwise it docks
  the keybar hint `nothing running — R retries · X abandons`. The daemon still
  errors on a kill with no running stage, so any new client needs its own guard.

Two state vocabularies exist and do not match — reading the wrong one is a
live source of bugs:

- **Store** (`IssueRow.State`) is written only by the steward's `setState` (plus
  the initial `running` from `Engine.CreateIssue`). It uses a stage-qualified
  running form — `running:spec` — plus `waiting_decision`, `failed`, `done`,
  `done (unmerged)`, `merged`, `abandoned`. There is **no** `paused` here: the
  steward has no `issue_paused`/`issue_resumed` case, and `Engine.Pause` is a
  purely in-memory `pauseGate`. Pause does not survive a daemon restart.
- **Projection** (`IssueView.State`, what the TUI sees) uses plain `running`,
  never `running:<stage>`; the stage lives in `CurrentStage`. It adds
  `queued_for_slot` and `paused`.

Compare against the projection vocabulary in TUI code, against the store
vocabulary in daemon/rehydrate code.
