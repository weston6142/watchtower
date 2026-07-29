Priority is a plain `int` everywhere it is stored, and a name everywhere it is
shown. Neither half is optional knowledge: the number decides ordering, the name
is all an operator ever sees.

- **Higher number = more urgent, and both consumers sort descending.** The slot
  queue (`internal/slots/slots.go`, ties broken by arrival order) and the backlog
  list (`internal/tui/app.go`, ties broken by id). A comparator written the
  intuitive "lower number first" way is backwards.
- **The vocabulary lives in `internal/priority`.** Four named levels,
  `low/normal/high/urgent` = `-1/0/1/2`, shared by the TUI and the CLI so there
  is one definition rather than two. `normal` is anchored at `0` so rows stored
  at the old default read correctly and `watchtower new -priority`'s default
  still agrees with the modal default.
- **Nothing persists a name.** `store.IssueRow`, `proto.Command`, and
  `projection.IssueView` all keep an `int`; `priority.Label` is a render concern.
  The daemon accepts any int — the constraint is a TUI one.
- **The modal's priority slot is a chooser, not an input.** `h`/`left` and
  `l`/`right` cycle it, wrapping, in the lever editor's `◂ value ▸` idiom. It has
  no text-entry path at all, so a bad priority is unreachable rather than
  validated — don't add validation for one. The cycling is gated on the focused
  field, because `h` and `l` are ordinary letters in every text field.
- **Out-of-set ints are rendered, never renumbered.** `watchtower new -priority
  42` and legacy rows survive: the modal, the backlog, and `watchtower backlog`
  all show `42`, and only a deliberate `h`/`l` moves it — onto the nearest named
  level in the direction travelled, never back to the raw number. An earlier
  attempt to clamp on modal open was reverted for this reason.
- **Consequence, accepted:** a row stored above `2` outranks every `urgent`
  issue in both sorts until someone opens it and cycles its priority.

The CLI is asymmetric on purpose: `-priority` takes a number in, and only the
render sites name it. Teaching the flag to accept names is open follow-up work.

This is the same split as the state vocabularies in lane-ops-and-issue-states —
the TUI guards what the daemon still allows.
