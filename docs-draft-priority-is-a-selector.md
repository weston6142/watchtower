Priority is not a text field anywhere in the TUI, and the stored int is not what
the UI shows.

- **Higher number = more urgent.** Both consumers sort *descending*: the slot
  queue (`internal/slots/slots.go`, ties broken by arrival order) and the
  backlog list (`internal/tui/app.go`, ties broken by id).
- **The vocabulary lives in `internal/priority`.** Four named levels,
  `low/normal/high/urgent` = `-1/0/1/2`, shared by the TUI and the CLI backlog
  listing so there is one definition rather than two. `normal` is anchored at
  `0` so rows stored at the old default read correctly and the CLI's
  `-priority` default still agrees with the modal default.
- **Nothing persists a name.** `store.IssueRow`, `proto.Command`, and
  `projection.IssueView` all keep a plain `int`; the names are a render concern.
- **The modal's priority slot is a chooser, not an input.** `h`/`left` and
  `l`/`right` cycle it (wrapping, like the lever editor's `◂ value ▸`), and it
  has no case in `setFieldValue`/`fieldValue` at all — backspace and rune input
  physically cannot reach it. A bad priority is unreachable, not validated, so
  don't add validation for one. The cycling lives in the key router and is gated
  on the focused field, because `h` and `l` are ordinary letters everywhere else.
- **Out-of-set ints are rendered, never renumbered.** `watchtower new -priority
  42` and legacy rows survive: the modal, the backlog, and the CLI listing all
  show `42`, and only a deliberate `h`/`l` moves it — onto the nearest named
  level, never back to the raw number. Losing the odd value takes a keypress.
- **Consequence, accepted:** a row stored above `2` outranks every `urgent`
  issue in both sorts until someone opens it and cycles its priority.

See also [[lane-ops-and-issue-states]] for the other TUI-guards-what-the-daemon
allows split; this is the same shape (the daemon still accepts any int).
