# Investigative Mode (GH-5)

Sometimes the operator wants to explore in a chat session before committing to
the formal flow. Investigative mode spawns a real interactive agent session in
a herdr pane, and gives it a structured exit: discard, save a backlog draft, or
save and launch the workflow.

## Entry — picker modal, then spawn

Pressing `i` in the TUI opens a small modal (same visual family as the
new-issue modal) with three fields:

- **Agent**: `claude` | `codex`
- **Model**: cycled list appropriate to the selected agent (claude: fable-5 /
  opus-5 / sonnet-5; codex: gpt-5.6-luna / other configured models)
- **Effort**: low / medium / high / xhigh

Enter spawns a new herdr pane via the same socket `internal/herdr` already
dials (extended with a `pane.split` call):

- cwd = repo root (the main working copy, not a worktree)
- the pane runs the agent command directly (not a shell), so quitting the
  agent closes the pane
- focus moves to the new pane

Spawn commands:

- claude: `claude --model <m>` plus the investigation system prompt via
  `--append-system-prompt`. Effort is set through claude's settings mechanism
  (`--settings` or env); the implementation plan pins the exact flag.
- codex: `codex -m <m> -c model_reasoning_effort=<effort>` with the
  investigation prompt injected through codex's config/prompt mechanism; the
  implementation plan pins the exact flag.

The last-used agent/model/effort are remembered per-repo in the existing repo
config and preselected next time. Esc cancels the modal.

If watchtower is not running under herdr (`HERDR_SOCKET_PATH` or
`HERDR_PANE_ID` unset), the TUI shows a status-line message containing the
exact command to run manually instead of failing silently.

Fire-and-forget: watchtower keeps no state about the spawned pane. Multiple
concurrent investigations need no special treatment — each is its own pane.

## In-session protocol

The injected system prompt tells the agent: this is a watchtower investigation
of this repository; when the investigation winds down, proactively offer three
exits — discard, save issue, save + launch — and point at `/wrap-up` as the
explicit trigger.

The wrap-up protocol has the agent:

1. Distill the investigation into an issue title + body, with a suggested
   priority.
2. Show it to the user and ask: **save draft** / **save + launch** /
   **discard**.
3. Run `watchtower new --title … --body … --draft` (save draft) or the same
   command without `--draft` (save + launch). Both commands already exist.
4. Close the pane: `herdr pane close "$HERDR_PANE_ID"`.

The protocol text is agent-neutral and shared by both spawn paths. For claude,
watchtower additionally ships a `/wrap-up` slash command (skill) so the
trigger is one keystroke-ish; codex receives the same instructions through its
prompt injection. Plain exit (quit the agent) closes the pane via process
exit.

## Error handling

- `watchtower new` fails inside the session: the agent reports the error and
  the pane stays open — nothing is lost.
- Pane spawn fails: TUI status-line error including the manual command.

## Testing

- Unit tests for the new herdr spawn call in `internal/herdr` against a fake
  socket server, matching the existing test pattern.
- TUI tests: `i` opens the modal; the modal's keys cycle fields; Enter issues
  the spawn request with the selected agent/model/effort; the no-herdr
  fallback message renders.
- The system prompt and skill are prose artifacts, reviewed rather than
  unit-tested.

## Out of scope

Tracking spawned sessions, per-investigation worktrees, transcript capture,
and any daemon involvement in the investigation lifecycle.
