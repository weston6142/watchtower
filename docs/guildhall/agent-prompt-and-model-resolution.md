# Agent prompt and model resolution

Watchtower supports Codex and Claude as separate runners. They share the task
and structured marker protocol, but each runner constructs its own CLI command
and continuation lifecycle.

## Prompt sources

Every stage begins with `agentprotocol.TaskMessage(stage, issueID)`. This
provider-neutral user message tells the agent to read the materialized
`ISSUE.md`, `STAGE.md`, artifacts, and decisions in the current workspace.
`setup_prompt` displays this message verbatim.

The selected runner injects the composed package prompt differently:

- Codex receives it as the `developer_instructions` config value.
- Claude receives it through `--append-system-prompt`.

Both CLIs still load their normal user and repository configuration. In
particular, Watchtower does not disable Codex user config, `AGENTS.md`, rules,
skills, or MCP configuration. Provider-owned base instructions remain inside
the CLI and are not visible to the setup inspector.

## Model and effort precedence

For Codex, a package's non-empty `model` or `effort` overrides the repository
`codex_model` or `codex_effort`. Empty package values inherit the repository
defaults. New repositories use `gpt-5.6-luna` with `xhigh` reasoning.

Claude retains its existing behavior: the package model is passed when set,
and package effort maps through `claude.ThinkingTokens` to
`MAX_THINKING_TOKENS`. Empty or unknown Claude effort leaves the CLI default in
place.

Two parsed declarations remain unapplied for both providers:

- A flow's `agents[].model` is reported as a declared model but does not
  override the package.
- A package's `max_turns` is reported but is not passed to either CLI.

All setup and issue-detail surfaces must resolve through
`Server.effectiveAgent`; looking directly in `sv.packages` can make two screens
report different effective models.

## Tools and unattended execution

`allowed_tools` is effective only for Claude, where it becomes
`--allowedTools`. Codex uses its normal configured tool environment; the setup
inspector shows the package list as `declared tools ... — not applied` and
labels the effective source `codex config`.

Codex stages explicitly set `sandbox_mode="danger-full-access"` and
`approval_policy="never"`. Watchtower does not add flags that suppress normal
Codex configuration or make a thread ephemeral.

## Decisions and continuations

When an agent emits a decision, Watchtower preserves the first Codex thread
UUID and resumes that exact thread with coaching or the human response. It
never uses `--last`. Tokens accumulate across the initial and resumed turns,
while proposal and dependency markers continue through the shared
`agentprotocol` parser.

Only a decision marker in assistant response text is eligible; markers in tool
calls or tool output are ignored. Both runners coach the same decision when it
lacks a nonblank rationale or reversibility boundary, one nonblank consequence
per choice (or any consequence for freeform), or complete citation pairs in
supplied proof and excerpts. After two unsuccessful repair turns, the runner
fails the stage instead of publishing an incomplete decision.

New choice and freeform decisions carry an engine-owned context envelope with
one frozen task summary plus the emitting agent's full name, explicit color
name, and symbol. The envelope is attached after marker parsing and survives
pending state, events, persistence, projection, daemon JSON, CLI output, TUI
surfaces, auto-resolution, and replay. Decision renderers show it in a
dedicated wrapping header without truncation; the literal color name and
symbol remain visible when color styling is unavailable. Missing, malformed,
partial, or over-limit context fails closed for new decisions. Historical
decisions without the envelope remain readable through the headerless legacy
path, without inferred identity.

Agent-authored choice decisions support both an option response and typed
feedback through the existing freeform response path. The TUI presents
`Add note...` for these choices, and submitting that text through
`answer_decision` resolves the decision and resumes the agent with
`Human decision: <text>`. Engine-owned artifact reviews, plan reviews, and
token-budget gates require a listed option instead: pending-decision JSON
exposes `requires_option: true`, the TUI omits `Add note...`, and freeform
responses are rejected. Agents cannot set this capability in decision markers.
The `allow_freeform` marker and storage field remain for compatibility with
older artifacts, but are legacy metadata rather than a runtime capability gate
for agent-authored choices; missing or false values do not prevent a valid note.

Decision HTML pages keep the original question and place a fixed action
briefing directly below it: do this now, recommended choice and rationale, one
consequence per choice, cited proof, and the exact post-answer continuation.
Watchtower derives the action and continuation from trusted engine state;
agents provide rationale, consequences, and cited proof. Historical gaps are
labeled explicitly rather than filled by paraphrasing the question. The stable
page is `<DataDir>/<issue-id>/decision.html`; frozen archives live at
`<DataDir>/<issue-id>/decisions/<decision-id>.html`.

Pending publication is fail-closed. Watchtower records the decision, freezes a
decision-time page snapshot, writes each per-decision and stable HTML file
atomically, and only then exposes the decision event and includes it in
`list_decisions` / `overview.need_you`. A page or event failure removes
in-memory visibility and rolls back the unpublished row and pages; if cleanup
also fails, the stage error reports both failures. Manual plan-review request
and decision events are appended as one batch. Killing or abandoning the lane
can cancel the same unpublished window before it becomes answerable.

The frozen snapshot keeps an archived page tied to the floors, artifacts,
evidence, and continuation that existed when the decision was raised. Answering
rewrites that archive as resolved and refreshes the stable page to current lane
progress; automatic recommendations and policy-approved plan reviews also get
resolved archives with explicit provenance. On restart, Watchtower repairs a
missing or still-pending archive for a resolved decision from its snapshot and
does not rewrite an archive already marked resolved. Historical rows without a
snapshot remain readable but cannot be reconstructed from current flow state.
For a human answer, the durable resolution remains authoritative if the archive
rewrite fails; the daemon logs the failure and restart repair can finish it.

The TUI's `w` key opens the stable page by absolute path from either the focused
lane or its decision card. Completed floors link to the frozen per-decision
archives.
