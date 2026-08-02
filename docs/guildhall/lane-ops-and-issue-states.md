Five operator actions look similar and are routinely confused. They are not
interchangeable:

| Action | Key | Op | Scope |
|---|---|---|---|
| pause / resume | `p` (toggle) | `pause_issue` / `resume_issue` | issue keeps its place; reversible |
| kill | `x` | `kill_stage` | cancels the *running stage* only; the lane stays |
| retry | `R` | `retry_stage` | re-runs an invalid/unvalidated final verifier, or resumes verified integration/publication/cleanup without a model call |
| retire | `c` | none (TUI-local) | hides a *shipped* lane in this TUI session only; not durable |
| abandon | `X` | `abandon_issue` | removes the lane everywhere, durably, forever |

Retire also fires on its own, from `autoRetire` on the TUI tick: once
`retireAfter` (5 m by default) has elapsed since `MergedAt`, and — immediately,
without waiting it out — for any lane merged before the current day began. Both
paths sit behind the same `retireAfter > 0` guard, so a hand-built model with a
zero `retireAfter`, as `FixtureModel` has, retires nothing while `shelfItems`
still filters: the stale lane leaves the shelf but stays on the grid. That
combination is unreachable in production and test-only — see setup-inspector for
the fixture side of the day cutoff.

**Focus follows the rendered grid.**
`Model.Focus` is transient TUI state. A lane is eligible for focus only when its
ID is in `projection.State.Order`, its issue still exists in `State.Issues`, it
is not locally retired, and its current stage resolves to a configured,
renderable floor. Whenever an eligible lane exists, focus names exactly one;
empty focus is normal only when no eligible lane can be rendered.

The TUI normalizes focus after event batches and replay, navigation and explicit
focus requests, and local or automatic retirement. It preserves the current
eligible lane and recomputes its floor/card coordinates. If the current focus
is missing or stale, it selects the first eligible lane in `State.Order` after
the existing visibility and renderability filters. An unavailable explicit
target leaves a valid current focus unchanged. When the last eligible lane
leaves the grid focus becomes empty, and the first lane becoming eligible
restores focus automatically. Because focus is transient, replay repairs it
from the current projection; no focus field is persisted or migrated.

**"Shipped today" is scoped in the TUI, not the projection.**
`projection.State.Shipped` is an all-time, clock-free accumulator of merged lane
IDs, because the projection stays a deterministic fold over the event log. The
day scope lives only in `internal/tui`, applied at exactly `autoRetire` and
`shelfItems` against `Model.dayStart` — local midnight, refreshed every tick
from `core.StartOfDay`. A new consumer that means "today" filters
`IssueView.MergedAt` itself; no field name does it for you.

The header's shipped-today count is a different *population*, not merely a
different boundary: it counts raw `issue_merged` events read from the store,
while the shelf reads `State.Shipped`, which `issue_abandoned` removes from.
Merge a lane today and then abandon it and the header says one shipped while the
shelf shows none. That divergence is correct — don't unify it. Separately,
`Store.EventsSinceTime` compares RFC3339Nano strings lexicographically, so an
exact-midnight threshold formats with no fractional digits and wrongly excludes
events in `[midnight, midnight+1s)`; it can only under-count, and any new
since-midnight query inherits the bug until it is fixed.

Rules that hold across the daemon:

- **`abandoned` is terminal.** `Rehydrate` skips it alongside `done`,
  `done (unmerged)`, `merged`, and published `cleanup_needed` lanes
  (`internal/engine/engine.go`). A restart must
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

Externally worked tasks have a deliberately small lifecycle:

```text
backlog -> claimed -> verifying -> integrating -> merged -> done
```

`watchtower claim <issue-id>` atomically reserves a ready backlog item and
returns its durable issue branch, base commit, and isolated worktree. Repeating
the command resumes the same valid claim; it does not create another workspace.
The conversation and implementation inside that worktree are intentionally not
constrained by Watchtower's normal stage flow.

`watchtower release <issue-id>` is only for an unused claim. It returns the task
to `backlog` when the worktree is clean and the issue branch has no commits past
the recorded base. It refuses to discard either committed or uncommitted work.

`watchtower finish [<issue-id>]` is the explicit handoff. It accepts only the
exact clean worktree and branch recorded by the claim, then enters the normal
merge-verification and durable finalization path. A claim with no new commit is
rejected unless the operator explicitly supplies `--allow-no-change` after
acknowledging that outcome. Daemon restart restores idle claims and failed
external verification from the recorded workspace identity; retry reuses that
workspace. Publication and cleanup failures retain their existing durable retry
states.

Neither a skill nor a client may declare the task complete from a successful
command invocation or transcript. Only Watchtower may emit terminal completion,
after verification receipts, merge, configured push, and cleanup have reached
their durable checkpoints.

Finalization has a durable boundary that is independent of an agent transcript:

- `merge-report.md` contains rich human evidence. `merge-decision.json` and
  `verification.json` are strict machine receipts; unknown fields and missing
  merge identity are rejected.
- The engine validates both receipts against the current branch, base, tree,
  and configured verification command, then persists `verification_ready`
  before emitting final-stage completion.
- A failure before that checkpoint belongs to the final verifier, so `R`
  reruns that stage. A failure after it belongs to finalization, so `R` retries
  integration without calling a model. `publish_pending` and `cleanup_needed`
  retries are likewise model-free.
- Restart automatically resumes only `verification_ready` finalization. Other
  interrupted stages fail visibly and wait for an operator retry.
- Transcript completion is never lifecycle authority. Only validated receipts,
  durable integration state, and completion/merge events can finish a lane.

Two state vocabularies exist and do not match — reading the wrong one is a
live source of bugs:

- **Store** (`IssueRow.State`) is written only by the steward's `setState` (plus
  the initial `running` from `Engine.CreateIssue`). It uses a stage-qualified
  running form — `running:spec` — plus `backlog`, `claimed`, `verifying`, `waiting:integration`,
  `integrating`, `failed`, `failed:finalize`, `waiting_decision`, `done`,
  `done (unmerged)`, `merged`, `cleanup_needed`, `abandoned`. A
  `cleanup_needed` issue is already semantically merged: dependents wake, while
  the exact worktree-release or safe branch-delete operation remains visible
  and retryable without rerunning stages, merge, or verification. There is
  **no** `paused` here: the
  steward has no `issue_paused`/`issue_resumed` case, and `Engine.Pause` is a
  purely in-memory `pauseGate`. Pause does not survive a daemon restart.
- **Projection** (`IssueView.State`, what the TUI sees) uses plain `running`,
  never `running:<stage>`; the stage lives in `CurrentStage`. It adds
  `queued_for_slot`, `claimed`, and `paused`, and shares the four explicit finalization
  states above. Overview counts verifying, waiting-for-integration, and
  integrating lanes as building; `failed:finalize` is failing. Herdr derives
  its working/blocked/idle report from those overview totals. Terminal,
  claimed, abandoned, decision-waiting, and cleanup-only lanes are not builders.

Compare against the projection vocabulary in TUI code, against the store
vocabulary in daemon/rehydrate code.
