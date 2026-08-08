The tower enables Bubble Tea cell-motion mouse reporting, but mouse input stays
an ephemeral TUI interaction. No pointer, hover, or focus state is persisted,
and mouse handling does not add protocol or issue-data changes.

In the grid, a left-button press inside a currently visible issue lane focuses
that issue. The target covers the rendered lane body, including its identity
and stage rows, and uses the same focus normalization and detail-refresh path
as keyboard focus movement. Stage gutters, compact edge gutters, hidden lanes,
rows view, overlays, reconnecting state, and non-grid doors are not click
targets. A wheel event never changes focus.

The artifact pager and live transcript each accept wheel input only inside
their active rendered reading surface. Wheel-up moves one logical line toward
older content; wheel-down moves one line toward newer content. Existing pager
and transcript transitions provide the bounds. Transcript scrolling preserves
the held-versus-following distinction and reattaches at the newest bound using
the existing refresh behavior.

Unsupported buttons or actions, motion and release events, unusable
coordinates, and mouse input outside an applicable surface are silent,
state-preserving no-ops. Hit-testing follows the final rendered geometry, so
terminal resizing, vertical centering, stacked versus side-by-side layout, and
focused-lane compaction cannot silently create a second coordinate system.
