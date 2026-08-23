Five operator actions look similar and are routinely confused. They are not
interchangeable:

| Action | Key | Op | Scope |
|---|---|---|---|
| pause / resume | `p` (toggle) | `pause_issue` / `resume_issue` | issue keeps its place; reversible |
| kill | `x` | `kill_stage` | cancels the *running stage* only; the lane stays |
| retry | `R` | `retry_stage` | retries a failed stage after its required state change, including trusted-workspace recovery; gate-authorizes a generic failed-stage retry from durable failure/state evidence; re-runs invalid final review or stale-identity reverification; or resumes verified integration/publication/cleanup without a model call |
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

Ledger-only completion is a separate administrative branch:

```text
backlog -> done
```

`watchtower close <issue-id>` is daemon-mediated and eligible only for a
known, idle backlog/draft issue with no active claim, pending decision, or
durable claim/integration record. It does not claim or inspect a worktree, run
stages or providers, verify code, merge, push, publish, or clean up. The daemon
persists an explicit `ledger_closed` integration checkpoint with no branch,
commit, or landed SHA and emits `issue_completed` with `completion: ledger`.
That checkpoint satisfies dependencies and wakes waiting dependents, but it
does not imply verification, merge, publication, or cleanup. A normal `done`
issue is an idempotent no-op; `done (unmerged)`, `merged`, `cleanup_needed`,
`abandoned`, and all active states remain refusals.

Backlog dependency visibility is a read-time projection, not relationship
cleanup. Stored dependency relationships remain available in the raw issue
data, while backlog CLI, structured, and TUI views render `depends on` and
claimability from active blockers. A referenced issue in
`IntegrationMerged`, `IntegrationCleanupNeeded`, or `IntegrationLedgerClosed`
satisfies its dependency and is omitted from active blockers; preserved or
unmerged work, missing integration evidence, and unknown states remain visible
so the system fails closed. A later status change is reflected on the next
backlog read without recreating the relationship.

Legacy completed issues may be reconciled during daemon rehydrate when durable
`issue_merged` and `issue_completed` evidence identifies one landed commit, or
when the exact branch-only merge shape is followed by a completion. Direct
landed-commit evidence must prove that commit reachable from the configured
base branch. Branch-only evidence is accepted only when that base history
contains exactly one reachable, canonical two-parent merge whose subject is
`Merge branch 'issue/<issue-id>' into <base-branch>`. Malformed,
contradictory, ambiguous, unreachable, non-merge, and `left-unmerged` evidence
remains blocked. Reconciliation writes the existing merged integration
checkpoint without appending an inferred lifecycle event; write failures fail
rehydrate, and dependency claims never use lifecycle evidence without current
integration state.

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
  `verification.json` are strict machine contracts; unknown fields and missing
  merge identity are rejected. The final-review agent authors only the report
  and merge recommendation. For every integrating flow, `test_cmd` is required
  and the engine authors `verification.json` after the final-review capability
  result is validated and durably bound.
- Cache-managed verification carries strict `cache_evidence` in
  `verification.json`: the engine acquires the lease only after final-review
  capability validation, runs the exact configured command, seals the lease,
  and writes the receipt. The agent never receives the lease or its managed
  environment. A cache hit never replaces the configured command.
- The engine validates the recommendation and receipt against the current
  branch, base, tree, configured verification command, immutable capability
  result, and cache identity, then persists `verification_ready` before
  emitting final-stage completion.
- A failure before that checkpoint belongs to the final verifier, so `R`
  reruns that stage only after the shared durable retry gate authorizes its
  failure class, fingerprint, allowance, and current state vector. A failure
  after it normally belongs to finalization, so `R` retries integration without
  calling a model or entering the generic gate. If finalization identifies
  stale branch, tree, lease, or cache identity, it remains failed closed until
  an explicit `R`; that retry quarantines the stale verification attempt and
  re-enters merge-verification for fresh current-identity proof. The old
  receipt remains immutable. Matching proof and unrelated finalization
  failures retain their existing retry behavior. `publish_pending` and
  `cleanup_needed` retries are likewise checkpoint-specific and model-free.
  See [retry-gating.md](retry-gating.md).
- Restart automatically resumes `verification_ready` finalization and a
  committed pending reverification attempt. Other interrupted stages fail
  visibly and wait for an operator retry.
- Transcript completion is never lifecycle authority. Only validated receipts,
  durable integration state, and completion/merge events can finish a lane.

Stage attempts also have six ordered v1 checkpoints:
`runner_succeeded`, `artifacts_validated`, `artifacts_archived`, `gate_resolved`,
`verification_passed`, and `finalization_ready`. Each attempt has a stable
identity and an immutable model-result slot materialized once before
`runner_succeeded`; a restart resumes from the latest committed checkpoint
without requesting the model again.

For new attempts, `runner_succeeded` additionally requires an immutable
effective-capability record with a matching contract, enforcement plan,
trusted baseline, passing final delta validation, and the same result digest.
Provider success or a transcript cannot fill that gap. The contract is
compiled once per engine attempt and reused unchanged by initial, resumed,
parallel, and fallback turns.

Capability failures use four stable reasons. Contract-invalid and
provider-unsupported failures stop before provider launch and require a
configuration/provider change. Runtime-denied and post-stage-violation failures
reject the complete attempt, mark the exact issue workspace
`capability_recovery_needed`, and require `trusted_workspace`. No artifact,
gate, dependency, verification, or finalization state may consume rejected
bytes. The engine reconstructs the exact trusted commit/tree through the
workspace provider before retry; an unchanged retry has a new contract ID but
the same authority digest. See effective-stage-capabilities for the complete
boundary.
Prepared rows and unreferenced filesystem bytes are never recovery authority.
Checkpoint-finalization failures fail closed and leave the preceding committed
checkpoint as the retry boundary. Recovery validates the predecessor chain,
result digest, archive paths, and archive digests before advancing; corruption,
conflicts, missing results, or unknown versions stop with a diagnostic.

For `execute`, correctness review, clean-code review, and librarian, that
immutable model-result manifest also contains one versioned, stage-specific
structured result. Providers supply evidence only; the engine binds the issue,
attempt, logical kind, and newest valid predecessor, validates the envelope and
payload, persists the immutable manifest and lifecycle summary, and only then
may commit `runner_succeeded` or advance the stage. A `completed` result can
resume unfinished post-run checkpoints without another model call. A valid
`retryable` result remains durable history, but retry creates a new attempt
linked to the newest supported valid predecessor instead of reusing the prior
attempt.

Structured result history is append-only. Exact replay of an attempt is
idempotent; conflicting reuse is rejected, and earlier task, commit, finding,
fix, review, skip, no-change, and documentation evidence remains queryable.
The next attempt's `STAGE.md` is projected from only the newest supported valid
result and contains explicit unfinished tasks, open findings, explained skips,
unreviewed paths, missing documentation, and remaining concerns—not completed
work, applied fixes, reviewed paths, or transcript prose. Invalid,
contradictory, wrong-stage, unsupported, or incompletely persisted candidates
cannot advance the stage or supersede the previous valid retry authority.

The attempt archive path is the canonical operator link for newly archived
artifacts. Legacy `stage_checkpoints` rows and `artifacts/<name>` paths remain
readable for migration and historical pages, but Watchtower never rewrites
their bytes or replaces a historical artifact with a later attempt. The
verification-ready, publish-pending, cleanup-needed, and model-free retry
boundaries described above remain separate from these stage-attempt
checkpoints.

Flow integration is capability-based, not tied to a stage name or stage count:

- Every stage declares one fixed `capability_profile`; librarian stages also
  declare explicit `documentation_paths`. Runtime authority comes from the
  compiled contract, not the stage name, assigned package, prompt, or tool
  metadata.
- A flow may have no `merge_barrier`. Watchtower then never merges or pushes on
  that flow's behalf. A clean workspace is released; committed or uncommitted
  work is preserved and reported with its branch and worktree.
- A flow that integrates has exactly one `merge_barrier: true`, and it must be
  the final stage. That stage must declare `merge-report.md`,
  `merge-decision.json`, and `verification.json`.
- Integrating flows require a non-empty repository `test_cmd`. The daemon
  records that exact command and Watchtower rechecks the receipt against the
  current base, branch, and tree before merging.
- Stage names, the number of stages, and the agents assigned to them remain
  customizable. Runtime behavior and E2E expectations derive from the flow's
  declared capabilities instead of the bundled default flow.

The bundled default flow requires explicit artifact review for `spec` and an
explicit plan-review authorization for `plan`. After each producer archives its
declared artifacts, Watchtower pauses before handing off to the next stage.
The default is manual plan review:

```yaml
plan_review:
  policy_id: manual-default
  policy_version: "1"
  auto_approve_regular: false
```

Regular-mode policy auto-approval is an explicit opt-in. For example, a
repository may set `policy_id: team-ci`, `policy_version: "2026-08-03"`, and
`auto_approve_regular: true`; strict mode still requires a human regardless of
that setting. Missing or malformed policy configuration falls back to manual
human review and never authorizes automatically. Plan review history uses
`plan_review_requested`, `plan_review_human_approved`,
`plan_review_policy_approved`, `plan_review_rejected`, and
`execution_started` to distinguish the request, approval provenance, and
execution boundary. Changing this configuration affects new runs only; an
in-flight run keeps its persisted policy snapshot.

All decisions also pass through one engine-owned escalation policy. Stage,
operation, path, destructive-risk, publication-risk, and default policy floors
are independent; the strictest matching floor is authoritative. Model
importance is advisory and can raise scrutiny only. An engine-required floor
cannot be bypassed by omitting or understating model metadata.

Artifact and plan approvals persist exact per-item bindings: kind, SHA-256,
repository-relative path, trusted operation, policy identity/version, matched
signals, and explicit dependency hashes. A changed item or policy is stale;
only explicit dependents are invalidated, while unrelated items retain their
own approval state. The same gate is re-evaluated at answer time and immediately
before handoff or execution. Missing context, malformed policy, and stale
evidence fail closed with explicit outcomes rather than appearing approved.

Artifact and plan reviews are engine-owned choice decisions: they cite the
exact archived artifact names, checkpoint, and SHA-256 digests, require an
`approve` / `revise` or `approve` / `reject` option, and never accept typed
feedback. Revision or rejection stops the run; retry creates and reviews a new
artifact version. A policy auto-approval creates no pending `need_you` item but
does record the policy identity and resolved decision archive. The general
decision publication, archive, and restart-repair rules are documented in
agent-prompt-and-model-resolution.

`watchtower init` does not overwrite an existing `.watchtower/flows/default.yaml`;
to migrate an existing repository, leave the `spec` gate as `approve_artifact`
and change only the `plan` gate to `plan_review`, then validate and restart the
flow. A stage already running is not changed retroactively, and the migration
deletes no prior artifacts, decisions, or checkpoints.

Two state vocabularies exist and do not match — reading the wrong one is a
live source of bugs:

- **Store** (`IssueRow.State`) is written by the steward's `setState`, the
  initial `running` from `Engine.CreateIssue`, and the transactional pause /
  resume persistence path. It uses a stage-qualified
  running form — `running:spec` — plus `backlog`, `claimed`, `verifying`, `waiting:integration`,
  `integrating`, `paused`, `failed`, `failed:finalize`, `waiting_decision`, `done`,
  `done (unmerged)`, `merged`, `cleanup_needed`, `abandoned`. A
  `cleanup_needed` issue is already semantically merged: dependents wake, while
  the exact worktree-release or safe branch-delete operation remains visible
  and retryable without rerunning stages, merge, or verification. There is
  also a durable `paused` run state. It records the stage boundary together
  with the issue, worktree, branch, base, and artifact references. The steward
  projects existing `issue_paused`/`issue_resumed` events to `paused`/`running`.
  A paused row rehydrates without a worker, failure event, or overview count;
  `p` / `watchtower resume <issue-id>` resumes its preserved boundary, and a
  repeated resume request does not start duplicate execution. A missing worker
  enters interrupted-stage recovery only when the persisted run state was
  `active`.
- **Projection** (`IssueView.State`, what the TUI sees) uses plain `running`,
  never `running:<stage>`; the stage lives in `CurrentStage`. It adds
  `queued_for_slot`, `claimed`, and `paused`, and shares the four explicit finalization
  states above. Overview counts verifying, waiting-for-integration, and
  integrating lanes as building; `failed:finalize` is failing. Herdr derives
  its working/blocked/idle report from those overview totals. Terminal,
  claimed, abandoned, decision-waiting, and cleanup-only lanes are not builders.

Compare against the projection vocabulary in TUI code, against the store
vocabulary in daemon/rehydrate code.
