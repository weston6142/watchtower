# Watchtower — Design Spec

*2026-07-26 · watchtower*

> Renamed from guildhall. User state moved to `~/.local/share/watchtower` and
> the repo config folder to `.watchtower/`, but the checkout path is
> deliberately unchanged: the per-repo ID is a hash of the absolute checkout
> path, so moving the checkout would orphan all issue state.

A terminal-native orchestrator for running many AI coding agents on real software projects, rendered as a pixel-art tower. Visual reference for the target look and feel: the tower concept mockup at https://claude.ai/code/artifact/b736348a-1393-4d49-9d97-85942a298bc7 (stage-floors variant; v2 sprite styling), fused with the Forge Line's stage-pipeline separation (https://claude.ai/code/artifact/6bc478b0-209d-4b46-938d-56856bbc76b5). The engine runs issues through a configurable pipeline (brainstorm → spec → plan → execute → review → merge) with per-stage autonomy levers; the TUI makes 10 parallel flows legible at a glance and funnels every human call into one decision queue.

## Goals

- Manage ~10 issues in flight at once without cognitive overload.
- Every meaningful human decision surfaces in one queue; everything else proceeds on agent recommendations, tuned by autonomy levers.
- Legible to a layman at the top level; drillable to diffs and transcripts for engineers — all in-app (no external editor required).
- Watch the architecture actually build over time, including in-progress branch work.
- Terminal-native: lives in tmux/SSH beside nvim and agent CLIs (herdr-adjacent workflow).
- The pipeline is data, not code: users can swap stages or ship whole alternative flows. Watchtower provides infrastructure; the shipped flow is just the default.

## Non-goals (v1)

Sprites/kitty graphics (v2), operator avatar (v2), non-Claude runners (Codex/Cursor — interface reserved, not built), flow registry/marketplace, web renderer, multi-repo projects.

## Architecture

Three layers over one contract:

### 1. Conductor (daemon, Go)

Owns all real state: SQLite issue store, flow definitions, lever matrices, slot pool, decision queue, append-only event log. Executes flows by walking a flow definition; for each stage it spawns a **runner** — a headless Claude Code subprocess (`claude -p --output-format stream-json`) with the stage's prompt package, in the issue's treehouse worktree when the stage requires one.

- `Runner` is an interface from day one; `ClaudeCodeRunner` is the only v1 implementation.
- Runners are children of the daemon, never the TUI. The daemon journals runner session IDs so a restart can resume flows (`claude --resume`).
- Overlords are woken on relevant events, not kept always-on.

### 2. Event stream

Every state change is an event (`stage_started`, `decision_required`, `test_run`, `files_changed`, `merge_sequenced`, `proposal_filed`, …) appended to the log and broadcast over a Unix socket as JSONL. Clients send commands on the same socket (`answer_decision`, `set_lever`, `pause_issue`, `accept_proposal`, `set_focus`). Full UI state is reconstructable by replaying the log — this is what makes SSH attach, TUI restarts, and future renderers cheap.

### 3. TUI (Go, Bubble Tea)

A pure client: renders the tower from events, sends commands. v1 is cell-based (box-drawing, color, unicode). v2 swaps the middle pane for a graphics viewport with a tiered fallback chain: kitty graphics protocol → sixel → unicode half-block sprites → plain cells. Chrome, tables, toasts, and pagers stay cell-based in every tier.

## Flows as data

A flow definition (`~/.config/watchtower/flows/*.yaml`, `default.yaml` shipped) is an ordered list of stages. Each stage declares:

- **`agents`** — one or more prompt packages that run it (a stage can fan out to several agents, sequentially or in parallel — e.g. the review stage runs clean-code-reviewer + general reviewer + documentation agent concurrently in the same worktree). Each package is a directory containing a system-prompt/skill markdown, allowed tools, and model/effort settings. The shipped defaults wrap the superpowers skills (brainstorming, writing-plans, executing-plans) and the reviewer/doc agents. The stage completes when all its agents complete (or per an `all`/`any` completion rule).
- **`workspace`** — `none` (Q&A stages), `worktree` (execution; acquired via treehouse), or `readonly`.
- **`gate`** — completion behavior: `approve_artifact` (human reviews spec/plan/diff), `decision_queue` (escalated questions), or `auto`.
- **`lever_row`** — which lever governs the stage and what YOLO / Regular / Strict mean for it.
- **`artifacts`** — required outputs (e.g. `spec.md`, `plan.md`, diff, docs). The engine validates presence, so a swapped-in community stage cannot silently produce nothing.
- **`on_fail`** — retry count, then escalate to the decision queue.

The engine knows nothing about "brainstorming" — only stages, gates, artifacts, levers. Replacing the planning method = one stage package + one line in the YAML. An alternative pipeline = a new YAML. Each issue selects its flow at kickoff (default: `default.yaml`). Distribution in MVP is directories + git clone; no registry.

### Default flow (shipped)

1. **Brainstorm** — superpowers brainstorming wrapper; questions either surface or auto-resolve to the recommendation per lever.
2. **Spec** — writes the design doc; gate `approve_artifact`.
3. **Plan** — implementation plan; gate `approve_artifact`.
4. **Execute** — subagent executes the plan on a treehouse worktree; heaviest slot consumer.
5. **Review & docs** — three agents in parallel: clean-code-reviewer + generalized reviewer (both fix what they find) + documentation agent drafting into the worktree.
6. **Merge** — Merge Marshal sequences and lands it; gate per lever.

## Autonomy levers

- **Per-stage matrix per issue**: rows = stages, columns = YOLO / Regular / Strict. YOLO ≈ auto-accept recommendations; Strict ≈ every question and artifact gated on the human; Regular between.
- **Presets** (YOLO / Regular / Strict) fill the whole matrix in one keypress; any cell is individually overridable. A global default matrix applies to new issues.
- **Escalation** (what still reaches the human in permissive cells) is three-layered:
  1. **Built-in floor** — always escalates regardless of lever: destructive migrations, code deletion beyond thresholds, auth/security changes, spending money, public API breaks.
  2. **User rules** — a rules file of glob patterns + descriptions (e.g. `payments/**` → always ask).
  3. **Model judgment** — the agent rates decision importance; the lever cell sets the surfacing threshold.
- Auto-resolved decisions are still logged as events (auditable; visible in the issue's history).

## Overlords

Event-woken agents with narrow authority:

- **Merge Marshal** — on plan approval: predicts file overlap between in-flight plans (from declared touch-sets) and sequences risky pairs ("#7 rebases after #5 merges"). On review pass: decides merge order, runs the merge train, dispatches a rebase/conflict subagent when needed. All predictions and sequencing are events — visible in the war room, overridable. Autonomous merging is just the merge stage's lever cell.
- **Librarian** — sole writer of `docs/`, ADRs, and shared project memory on main. Per-issue doc agents draft in worktrees; at merge the Librarian reconciles drafts (one voice, contradictions resolved, stale docs flagged). Also the context curator: at flow kickoff the Conductor asks it "what should this issue know?" and injects the answer into stage prompts.
- **Issue Steward** — watches all events; keeps issue records in sync (status, links, scope changes); files **proposed** issues into a triage tray when agents discover work. Never creates real backlog items itself; the user accepts/rejects proposals with one keypress.

## Concurrency & cost

- **Slot pool**: configurable count (default 4) of heavy-runner slots; execution-class stages require a slot; cheap stages (Q&A, doc drafts) run outside the pool. Issues queue for slots by priority; queued-for-slot is a visible tower state.
- **Token budgets** per issue as a guardrail; budget exhaustion escalates to the decision queue.

## Data model

SQLite core tables:

| Table | Contents |
|---|---|
| `issues` | id, title, body, state, flow name, lever matrix, priority, links |
| `stage_runs` | issue, stage, agent package, runner session id, worktree path, artifact refs, status, tokens (one row per agent — a multi-agent stage has several concurrent rows) |
| `decisions` | question, options, recommendation, evidence refs, lever context, status, answer, answered_by |
| `events` | append-only log (the UI's food) |
| `proposals` | triage tray items from the Issue Steward |

Artifacts (specs, plans, transcripts, diffs) live on disk in per-issue directories; the DB stores paths + hashes.

**Decision queue record** (uniform for gates, escalations, overlord signoffs): plain-English question, options with one recommended, evidence links, and what it blocks. Ordering: blocking-cost first (issues/slots unblocked by answering), then age.

## TUI

**Layout — the tower.** Floors = stages of the default flow; war room on top. Issue cards sit on their current floor showing id, title, agent state glyphs, derived progress (tests/artifacts only — never self-reported), token spend, and red/gold edging for failing/blocked. Floor pile-ups read as bottlenecks.

**Issue identity.** Every issue is assigned a stable identity at kickoff: a color from a distinguishable palette plus a two-letter tag (e.g. `#7 PA` for "payments," rendered in that issue's color). Every agent working the issue wears the identity — card edging, agent glyphs, arch-map ghosts, decision toasts, merge-train entries all share it — so "which issue does this agent belong to" is answerable at a glance anywhere in the UI. A stage running multiple agents shows one glyph per agent inside the issue card (e.g. `◆◆◆` for the three review agents), each with its own state; in v2 these become individual sprites clustered at the issue's desk in the issue's colors.

**War room floor**: merge-order lane (the sequenced train approaching merge), overlord status, triage tray count, slot pool gauge.

**Right rail**: context panel for the focused thing (issue → stage run → artifacts) and the decision queue with the top item always visible.

**Toasts**: `decision_required` pops an overlay — question, recommendation, `y` accept recommendation, `n` choose another option, `o` open evidence, `Esc` defer to queue.

**Drill-down (all in-app)**: toast → evidence panel (summary, diff stats, test results) → artifact pager (spec/plan/diff, syntax highlighted) → raw transcript viewer. No external editor required; an optional "open in $EDITOR" keybind may come later as a power-user extra.

**Navigation (flip-based, no free walking)**: `j`/`k` floors · `h`/`l` cards within a floor · `1–9` jump to issue · `Enter`/`Esc` drill in/out · `Tab` cycles attention items only (blocked + decisions, worst first) · `d` decision queue · `t` triage tray · `L` lever matrix for focused issue · `a`/`A` architecture view · `?` help. In v2 the operator sprite climbs/descends to wherever focus lands; animation speed configurable, default very fast.

## Architecture view

- **Base map** generated from main: packages/modules + import edges (cached; re-derived on merge).
- **Worktree overlay**: each in-flight issue ghosts onto the map in its own color — new modules dashed/translucent, edited modules pulsing. Merging solidifies ghosts into the base. Over time you watch the system physically grow.
- Selecting a node shows owning issues, recent churn, test health, and the Librarian's docs for it.
- v1 renders as a cell-based box diagram; v2 uses the graphics viewport.

## Error handling

- Stage failure: retry per `on_fail`, then escalate with evidence.
- Runner crash/orphan: daemon journal + session resume; unresumable runs mark the stage failed and escalate.
- Daemon crash: state is SQLite + event log; on restart, reattach or resume runners, replay tail for clients.
- Escalation floor misfire (agent bypasses a gate): artifact validation + merge stage as backstop — nothing reaches main except through the Marshal.

## Testing

- Engine: unit tests on the flow runner state machine with a `FakeRunner` (scripted event emission); golden tests on lever/escalation routing (given decision X + matrix Y → queue or auto).
- Protocol: replay tests — a recorded event log must reconstruct identical client state.
- Overlords: fixture plans with known overlaps → expected sequencing.
- TUI: teatest (Bubble Tea's harness) snapshot tests of tower rendering from canned event streams.
- End-to-end: one scripted issue through all six stages against a sandbox git repo with a stub runner; a manual smoke flow with the real Claude runner.

## v1 cutline

**In**: Conductor + ClaudeCodeRunner; default flow YAML wrapping superpowers skills + treehouse; lever matrix + presets + escalation floor/user rules; decision queue + toasts; slot pool + token budgets; all three overlords in minimal form; cell-based tower TUI with full navigation and in-app drill-down; cell-based architecture map; 2–3 real issues end-to-end.

**Out**: everything in Non-goals.
