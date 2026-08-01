Every state change is one append-only event in `internal/core/event.go`, and
each event has **two independent consumers** plus a client path. Wiring only
one of them is the easy way to half-ship a feature: it works until the daemon
restarts, or it persists but never appears on screen.

The full sweep for a new lane-affecting event:

1. `internal/core/event.go` — the `EventType` constant.
2. `internal/engine/` — whoever emits it, via `e.emit(...)`.
3. `internal/steward/steward.go` — durable `IssueRow.State` for restarts.
4. `internal/projection/projection.go` — in-memory `State` the TUI renders.
   Deleting a lane means `Issues`, `Order`, `Shipped`, `Parked`, *and* its
   decisions.
5. `internal/proto/server.go` — the op name, if operators can trigger it.
6. `cmd/watchtower/main.go` — the CLI verb, in the `ops` map beside
   pause/resume/kill/retry/abandon/launch.
7. `internal/tui/app.go` — the key handler, and the help overlay group (help
   goldens in `internal/tui/testdata/` change when a key is added; regenerate
   deliberately, never blanket `-update`).

Steward and projection are not redundant: the steward answers "what survives a
daemon restart", the projection answers "what is on screen now". A state that
matters after a restart must be written by the steward even if the projection
already shows it.

`Engine.emit` is deliberately best-effort — marshal or store failures are
dropped rather than aborting the stage, because events are observability, not
state. Never make correctness depend on an event having been appended.
