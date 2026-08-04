**Tower daemon reconnect**

The tower is a long-lived client across repository-daemon loss or replacement.
Initial startup may use the normal connect-or-start path, but recovery is
dial-only against the repository identity and socket captured at launch. A
reconnect must never spawn, replace, or remove daemon state, and one tower may
have only one reconnect loop and one active replacement attempt.

Transport loss invalidates the old session generation and replaces the stale
screen and raw socket error with a concise reconnecting status. Retry continues
indefinitely with capped exponential backoff: 250 ms, 500 ms, 1 s, 2 s, then
4 s. While disconnected, remote commands are ignored rather than queued; help
and quit remain available. A successful recovery resets the delay only after
the replacement data is fully ready.

Recovery is an atomic refresh transaction. The replacement session rebuilds
projection state from the event tail starting at sequence zero, refreshes the
overview, and fetches the retained focused detail when that item still exists.
Only after refresh and context reconciliation succeed does the tower become
interactive again. A valid view and selection are restored; a missing
selection clears item-specific state and falls back to the repository overview.
Refresh failures discard the partial session and keep the tower reconnecting.

Every asynchronous session response carries its session generation. Responses
from an invalidated generation are ignored so a delayed old-daemon response
cannot overwrite replacement state. Shutdown is terminal: it cancels retry
work, invalidates the generation, closes active or partial sessions, and leaves
no background recovery operation.

The behavior boundary is covered by deterministic model tests for retry
cadence, single-loop ownership, refresh gating, context fallback, stale-session
isolation, input gating, and shutdown, plus a live daemon-replacement test that
checks one tower process survives and no duplicate daemon is created.
