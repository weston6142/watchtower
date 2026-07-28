**The module path is not the project name.** The Go module is
`github.com/weston6142/watchtower` and the git remote is
`git@github.com:weston6142/watchtower.git`, while the repo directory, the
binary, the CLI, and the product are all `guildhall`. Every internal import is
`github.com/weston6142/watchtower/internal/...`. Writing
`.../guildhall/internal/...` compiles to nothing found — check `go.mod` before
inventing an import path.

Default branch is `develop`, not `main`.

Layout:

- `cmd/guildhall/` — single binary: CLI verbs, daemon, TUI entry.
- `internal/core/` — event types; the shared contract.
- `internal/engine/` — the Conductor: flow execution, stage runs, decisions,
  rehydration.
- `internal/flow/`, `internal/levers/` — flows-as-data (stages, completion
  rules) and the autonomy lever matrix, presets, and escalation routing.
- `internal/store/` — SQLite issue rows + append-only event log.
- `internal/steward/`, `internal/librarian/`, `internal/marshal/` — the three
  overlords (issue state, project memory, merge train), woken on events.
- `internal/projection/` — event log → renderable `State`.
- `internal/tui/` — Bubble Tea client; goldens in `internal/tui/testdata/`.
- `internal/proto/` — Unix-socket JSONL commands/ops.
- `internal/runner/`, `internal/claude/` — the `Runner` interface and the
  headless `claude -p --output-format stream-json` implementation.
- `internal/slots/`, `internal/workspace/` — the heavy-slot pool, and workspace
  provisioning via `git worktree` under `.worktrees/` (one per issue, with a
  release func).
- `internal/archmap/` (repo paths → architecture areas), `internal/touchset/`
  (the file globs a plan expects to touch — the Marshal's overlap input),
  `internal/evidence/` (worktree diffs → `evidence.json` for gates),
  `internal/transcript/` (bounded per-stage agent output for the TUI's doors).
- `internal/pkgs/` (agent packages: `package.yaml` + `prompt.md`),
  `internal/repocfg/` (repo registry + per-repo config),
  `internal/scaffold/` (embedded defaults written by `guildhall init`).
- `.guildhall/` — this repo's own flow + prompt packages (it runs on itself);
  `dist/packages/` holds the shipped defaults.

The TUI is a pure client: it renders projected state and sends ops. It must
never own state or reach past the socket.
