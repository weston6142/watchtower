An agent's prompt and its model come from more places than the agent package,
and two of those places are parsed but never sent. These rules belong to
`internal/claude/` and `internal/proto/`, not to the panel that exposes them
(setup-inspector).

**The prompt is assembled from three sources, and only one of them is a file
you can edit.** `setup_prompt` shows the first two, in order:

1. The first user message — `claude.TaskMessage(stage, issueID)` in
   `internal/claude/runner.go`. It lives in Go source, not in any package, and
   it is what tells the agent to read `ISSUE.md` and where earlier artifacts
   sit. Editing `prompt.md` cannot change it.
2. `.watchtower/packages/<pkg>/prompt.md`, passed as `--append-system-prompt`.
   This is the agent's role.
3. Claude Code's own base system prompt, added by the CLI. Watchtower never
   sees it and cannot show it.

**Two declared fields are parsed and then dropped on the floor.** The runner
passes `pkg.Model` and nothing else, so:

- A flow's `agents[].model` reaches `flow.AgentRef.Model` and stops there. Set
  it and the agent still runs the package's model, silently.
- `max_turns` in `package.yaml` reaches `pkgs.Package.MaxTurns` and stops
  there. Watchtower never passes a turn cap to the CLI.

The setup inspector labels both `declared … — not applied` precisely because
they read as effective. If you make either one real, that label is the second
place to change.

**Effort reaches the CLI only as a thinking-token budget.**
`claude.ThinkingTokens(effort)` maps the level to a bare number and `EffortEnv`
wraps it as `MAX_THINKING_TOKENS=…`. A new effort level goes in
`ThinkingTokens`, never in `EffortEnv`, or the number the inspector reports and
the number the CLI receives drift apart. Empty or unknown levels mean CLI
default, not zero.

**Resolve through `Server.effectiveAgent`, not through `sv.packages`
directly.** It returns the effective package plus the declared-but-unapplied
model as separate values, and both `issue_detail` and `setup_outline` go
through it. A third surface that looks the package up itself is how two panels
in one TUI end up printing different models for the same stage.
