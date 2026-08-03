# Investigative mode

Investigative mode opens a temporary agent conversation for exploring an idea,
bug, or question before it becomes formal Watchtower work. From the tower grid,
press `i` to open the picker. `Esc` cancels it; `Tab` selects the next field and
`h`/`l` changes the selected value.

The picker remembers three choices per repository:

| Field | Values |
|---|---|
| Agent | `claude`, `codex` |
| Model | Claude: `fable-5`, `opus-5`, `sonnet-5`; Codex: `gpt-5.6-luna`, `gpt-5.4` |
| Effort | `low`, `medium`, `high`, `xhigh` |

The saved selection is JSON in `.watchtower/investigate.json`. It is a
convenience preference, not investigation state. Press `Enter` to start the
session using the repository root (the main checkout, including when the tower
was launched from a linked worktree).

## What starts in the new pane

Watchtower asks herdr to split a focused pane to the right, then types one of
these commands into it. The prompt placeholder is the shared investigation
protocol, shell-quoted by Watchtower:

```sh
exec claude --model <model> --effort <effort> --append-system-prompt '<investigation prompt>'
exec codex -m <model> -c model_reasoning_effort=<effort> '<investigation prompt>'
```

`exec` replaces the pane shell with the agent process. Quitting the agent
therefore closes the pane instead of leaving an idle shell behind. Claude also
gets the repository-local `/wrap-up` skill at
`.claude/skills/watchtower-wrap-up/SKILL.md`; an existing file is never
overwritten. Codex receives the same instructions through its initial prompt.

The agent is told to investigate collaboratively and not start the formal
workflow. When the conversation is winding down, or `/wrap-up` is requested,
it prepares a proposed issue title, body, and priority, shows them for approval,
and offers exactly these exits:

1. **Discard** — quit without saving anything.
2. **Save draft** — run `watchtower new --title "<title>" --body "<body>" --draft`.
3. **Save + launch** — run `watchtower new --title "<title>" --body "<body>"`.

After a successful save, the agent closes its pane with:

```sh
herdr pane close "$HERDR_PANE_ID"
```

If `watchtower new` fails, the agent displays the error and keeps the session
open so the investigation is not lost. A plain agent quit remains the discard
path.

## If herdr is unavailable

Without both `HERDR_SOCKET_PATH` and `HERDR_PANE_ID`, Watchtower cannot split a
pane. Enter still leaves the picker open and puts an error in the tower status
line containing a manual command, for example:

```sh
cd <repo> && claude --model fable-5 --effort low --append-system-prompt '<investigation prompt>'
```

The same fallback is shown for socket or pane errors, so the operator can copy
it, adjust the picker, or retry later. A spawn is fire-and-forget: Watchtower
does not involve the daemon in the investigation, track the new pane, capture
its transcript, or create a worktree for it. The independent agent session is
responsible for its own exit protocol and any issue it files.
