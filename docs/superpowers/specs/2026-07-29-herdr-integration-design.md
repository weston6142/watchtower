# Herdr Integration for Watchtower (GH-7)

**Date:** 2026-07-29
**Issue:** GH-7 — herdr notifications whenever watchtower is blocked

## Problem

When a watchtower lane needs the user (a decision, a proposal, or a failing
stage), nothing surfaces in herdr — the terminal workspace manager the user
lives in. Watchtower should appear in herdr's agent sidebar as an agent named
"watchtower" and report `blocked` whenever anything needs the user, so herdr's
native blocked-notification machinery produces the popup.

## Background: how herdr integrations work

Herdr exposes a local socket API (NDJSON over a Unix socket). Panes get
`HERDR_SOCKET_PATH` and `HERDR_PANE_ID` injected into their environment.
External tools report agent state with `pane.report_agent` using a
`custom:<name>` source; herdr drives sidebar state and blocked notifications
from those reports. Display naming is set via `pane.report_metadata`.

Assumption (confirmed with user): the watchtower TUI is normally open in a
herdr pane, so the TUI reports state for its own pane. No daemon-side
reporting.

## Design

### New component: `internal/herdr`

`Reporter` struct, constructed from the environment:

- Reads `HERDR_SOCKET_PATH` and `HERDR_PANE_ID` at construction.
- If either is missing, the constructor returns a disabled reporter whose
  methods are no-ops — zero behavior change outside herdr.

### Wiring

- The TUI constructs the Reporter at startup and calls
  `reporter.Report(overview)` from the same code path that applies overview
  updates to the model.
- On TUI shutdown, it reports `idle` (best-effort) so a closed TUI doesn't
  leave a stale blocked state in the sidebar.

### State mapping

Level-triggered, computed fresh from each overview snapshot:

| Condition | State | Message |
|---|---|---|
| `NeedYou + Failing > 0` | `blocked` | `"N need you"` (decisions + proposals + failing combined) |
| else `Building > 0` | `working` | `"N building"` |
| else | `idle` | — |

The Reporter remembers the last state+message sent and skips writes when
nothing changed.

### Wire protocol

One NDJSON request per report over a short-lived Unix socket connection
(connect, write, best-effort read, close):

```json
{"id":"watchtower:<ts>","method":"pane.report_agent","params":{"pane_id":"...","source":"custom:watchtower","agent":"watchtower","state":"blocked","message":"3 need you"}}
```

On the first successful report, also send `pane.report_metadata` with a
display-name token so the sidebar reads "watchtower".

Herdr's own notification system produces the popup on the transition to
`blocked`; watchtower does not call `notification.show`.

### Error handling

- All socket errors are swallowed; 500ms timeout; never block or crash the TUI.
- A failed write clears the remembered state so the next overview update
  retries.

## Testing

Unit tests point the Reporter at a fake Unix socket in a temp dir and assert:

- JSON payloads for each state transition (blocked / working / idle).
- Dedupe: identical consecutive states produce no write.
- Retry after a failed write.
- Disabled no-op path when env vars are unset.

No integration test against real herdr.

## Out of scope

- Daemon-side reporting when no TUI is open.
- Direct `notification.show` popups.
- Per-event (edge-triggered) hooks on engine events.
