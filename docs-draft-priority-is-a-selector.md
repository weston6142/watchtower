Priority is not a text field anywhere in the TUI, and the number does not mean
what the `p0`-is-hottest convention suggests.

- **Higher number = more urgent.** Both consumers sort *descending*: the slot
  queue (`internal/slots/slots.go`, ties broken by arrival order) and the
  backlog list (`internal/tui/app.go`, ties broken by id). So `p3` outranks
  `p0` — the inverse of the usual industry reading of those labels.
- **The modal's priority slot is a selector, not an input.** In
  `handleModalKey`, the priority field intercepts every key except `tab`: `h`
  /`left` and `l`/`right` walk the options and clamp at the ends, and all other
  keystrokes are *dropped* rather than rejected. A bad priority is unreachable,
  not validated — don't add validation for one, and don't assume typing digits
  into that field does anything.
- **Only `p0`–`p3` are reachable in the TUI**, but the wire and CLI are wider:
  `Command.Priority` and `watchtower`'s `-priority` flag take any int, and store rows
  hold it verbatim. Tests and fixtures elsewhere legitimately use values like
  `5` and `9`.
- **Opening the edit modal clamps silently.** `m.modal` is built with
  `priorityIndex(iv.Priority)`, so a draft filed at prio 9 shows `p3` and saves
  back as `3`. This is deliberate — the field displays what saving will
  write — but it means the modal is a lossy round-trip for out-of-range
  priorities filed by the CLI.

See also [[lane-ops-and-issue-states]] for the other TUI-guards-what-the-daemon
allows split; this is the same shape (the daemon still accepts any int).
