# Issue Backlog — Design

**Date:** 2026-07-28
**Status:** Shipped — merged to `develop` 2026-07-29; viewport sizing followed in GH-9

## Problem

`create_issue` is the only way an issue enters the system, and it starts the
lane immediately. There is no way to capture an issue for later, browse what's
queued up, edit it, or launch it when ready. The operator has to keep their
backlog somewhere else and retype it into the modal at launch time.

## Design

A draft is a normal issue with the new durable state `backlog`. It lives in
the existing `issues` table, flows through the event log like everything else,
and the projection renders it. No new tables, no side channels — the TUI stays
a pure client.

### Events (`internal/core`)

- `EvIssueDrafted issue_drafted` — a draft was created (payload: title, body,
  flow, preset, priority).
- `EvIssueUpdated issue_updated` — a draft's fields changed (payload: the new
  field values).
- Launching emits the existing issue-created/stage events via the normal
  CreateIssue run path; no separate `issue_launched` event is needed if the
  existing creation event carries the id. If the run path cannot reuse the
  draft's id cleanly, add `EvIssueLaunched` — decide in the plan after reading
  `Engine.CreateIssue`.

### Engine

- `DraftIssue(title, body, flow, preset, priority) (id, error)` — allocate an
  id the same way CreateIssue does, persist the row with state `backlog`,
  emit `issue_drafted`. No flow starts, no slot is taken.
- `UpdateIssue(id, fields) error` — only legal while state is `backlog`;
  error otherwise. Persist and emit `issue_updated`.
- `LaunchIssue(id) error` — only legal from `backlog`. Runs the existing
  CreateIssue start path (flow resolution, levers, slot queueing) keeping the
  draft's id, title, body, flow, preset, and priority.
- `Abandon(id)` — already exists; it must accept a `backlog` issue (no
  running stage to cancel) and remove it like any other lane. This is the
  delete path; there is no separate hard delete. The row stays in the store
  as `abandoned` history, consistent with lanes.

### Steward / rehydrate

- `issue_drafted` → durable state `backlog`; `issue_updated` rewrites the row
  fields; launch moves it through the normal running states.
- Rehydrate treats `backlog` as inert: rebuilt into memory as a draft so it
  can be edited/launched after a restart, never auto-started, never marked
  failed.
- Backlog issues count nowhere in failing/building/running overview counts.

### Projection

- `issue_drafted` adds the issue with state `backlog` (new projection state
  alongside `queued_for_slot`, `paused`, …). Drafts are kept out of the lane
  `Order` used by the tower grid and exposed as a `Backlog` list sorted by
  priority then id.
- `issue_updated` rewrites the draft's fields.
- Launch and abandon flow through existing handling.

### Proto / CLI

- Ops: `draft_issue`, `update_issue`, `launch_issue` alongside the existing
  verbs. `abandon_issue` already exists.
- CLI: `watchtower new --draft …` (same flags as `new`), `watchtower backlog`
  (list drafts: id, priority level name, flow, title), `watchtower launch <id>`,
  `watchtower abandon <id>` (existing).

### TUI

- **Modal double duty:** the existing new-issue modal keeps Enter = start
  now, and gains a save-to-backlog submit (`ctrl+s`), shown in the modal's
  hint line.
- **Backlog view:** a new key on the tower (`b`) opens a centered modal-style
  box styled like the new-issue modal (same border/title chrome as
  `renderModal`), listing drafts by priority — shown as the level name, not a
  number — with cursor rows. Keys inside:
  - `enter` — edit: reopen the same modal pre-filled; Enter re-saves the
    draft (it does NOT start the lane from the edit context — the hint line
    says so), `ctrl+s` also saves.
  - `l` — launch: confirm box, then `launch_issue`.
  - `X` — delete: existing abandon confirm, then `abandon_issue`.
- **Sized to the viewport (GH-9):** the backlog is the one overlay that grows
  with the terminal instead of hugging its content — it is a browsing surface,
  not a prompt. The frame fills the screen short of a two-cell margin — the
  margin is load-bearing, since `overlayCenter` drops the dimmed base once a box
  reaches the full width or height — floored at the width of its own key hint
  line and capped so one row is not a long walk for the eye on an ultrawide
  terminal. Height follows the same shape, and the row count is content-driven
  under that cap so a two-draft backlog does not render as a tall blank box.
- **Two panes:** the reclaimed width buys a detail pane on the right showing the
  selected draft's id, priority, flow, preset, and body — data `IssueView`
  already carried and the list never showed. Below the two-pane threshold the
  detail pane yields and the list takes the whole frame. A body too tall for the
  pane is cut with `… enter to read it all` rather than stopping mid-sentence.
- **Scrolling list:** more drafts than rows pages the window (no new state — the
  window is derived from the cursor), and the footer says which slice is on
  screen (`1–30 of 36`) so off-window drafts are not read as drafts that do not
  exist. The id column is derived from the widest id in the whole set, so slugs
  like `gh-webhooks` are shown whole and the column does not shift while
  scrolling. When the footer cannot hold both, the count yields and the keys
  stay.
- **Empty backlog keeps its old small box:** there is nothing to size to, and one
  sentence ruled off inside a 130-column frame reads worse than the small box.
- Help overlay gains the new keys.

## Non-goals

- No hard delete or purge of rows; abandon is the removal path.
- No import from GitHub/Linear (separate future feature).
- No editing of issues after launch.
- No reordering UI beyond the priority field. Priority persists as an `int`
  everywhere (store, proto, projection, both sorts); the names are a render
  concern only, with `low/normal/high/urgent` = `-1/0/1/2`. The modal's priority
  field is a chooser over those four, so an unnamed value can only arrive from
  `watchtower new -priority` or a legacy row — it is rendered as its number,
  never silently renumbered.

## Testing

- Engine: draft → row state `backlog`, event emitted, no stage runs; update
  legal only from backlog; launch keeps id/fields and starts the flow; abandon
  removes a draft; restart rehydrates drafts as editable, never running.
- Projection: drafts appear in `Backlog`, not the grid `Order`; update
  rewrites fields; launch moves it to the grid; abandon removes it.
- Proto: round-trips for the three new ops.
- TUI: golden fixtures for the backlog view and the modal's new hint line;
  key tests for `b`, edit, launch, `X`.
- `go test ./...` green.
