**Everything is named `watchtower` except the checkout directory.** The Go module
is `github.com/weston6142/watchtower`, the remote is
`git@github.com:weston6142/watchtower.git`, and the binary, the CLI, and the
product are `watchtower` too — only the checkout directory is still `guildhall`.
Every internal import is
`github.com/weston6142/watchtower/internal/...`; writing
`.../guildhall/internal/...` compiles to nothing found.

`guildhall` survives only in the directory name and in this docs path
(`docs/guildhall/`). Anything else you remember as `guildhall` — `cmd/guildhall`,
a `guildhall` verb, a `.guildhall/` config dir — was renamed and no longer
exists.

Default branch is `develop`, not `main`.

Layout:

- `cmd/watchtower/` — single binary: CLI verbs, daemon, TUI entry.
- `internal/core/` — event types and the day-boundary helper; the shared
  contract, and the only package both daemon and TUI may depend on for
  agreement.
- `internal/engine/` — the Conductor: flow execution, stage runs, decisions,
  rehydration.
- `internal/flow/`, `internal/levers/` — flows-as-data (stages, completion
  rules) and the autonomy lever matrix, presets, and escalation routing.
- `internal/store/` — SQLite issue rows, append-only event log, attachment
  metadata.
- `internal/steward/`, `internal/librarian/`, `internal/marshal/` — the three
  overlords (issue state, project memory, merge train), woken on events.
- `internal/projection/` — event log → renderable `State`.
- `internal/tui/` — Bubble Tea client; goldens in `internal/tui/testdata/`.
- `internal/proto/` — Unix-socket JSONL commands/ops.
- `internal/runner/`, `internal/claude/` — the `Runner` interface and the
  headless `claude -p --output-format stream-json` implementation.
- `internal/slots/`, `internal/workspace/` — the heavy-slot pool, and workspace
  provisioning (one per issue, with a release func). Two providers:
  `workspace.Detect` prefers `treehouse` leases when that binary is on `PATH`
  and otherwise falls back to `git worktree` under `.worktrees/`. Which one won
  is visible only in the setup inspector — see setup-inspector.
- `internal/archmap/` (repo paths → architecture areas), `internal/touchset/`
  (the file globs a plan expects to touch — the Marshal's overlap input),
  `internal/evidence/` (worktree diffs → `evidence.json` for gates),
  `internal/transcript/` (bounded per-stage agent output for the TUI's doors),
  `internal/attach/` (issue attachments end to end — resolution, validation,
  storage, per-stage materialization; see issue-attachments).
- `internal/pkgs/` (agent packages: `package.yaml` + `prompt.md`),
  `internal/repocfg/` (repo registry + per-repo config),
  `internal/scaffold/` (embedded defaults written by `watchtower init`).
- `internal/priority/` — the shared `low/normal/high/urgent` render vocabulary
  over the stored int; see priority-levels.
- `.watchtower/` — this repo's own flow + prompt packages (it runs on itself);
  `dist/packages/` holds the shipped defaults.

The TUI is a pure client: it renders projected state and sends ops. It must
never own state or reach past the socket.
