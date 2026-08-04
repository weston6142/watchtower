Planner exploration is bounded by finite defaults for calls, charged tokens,
and elapsed time:

```yaml
planner_budget:
  calls: {warn: 24, hard: 32}
  tokens: {warn: 200000, hard: 250000}
  elapsed: {warn: 8m, hard: 10m}
```

The `planner_budget` block is planner-specific; the existing issue-wide
`budget` setting remains separate. Missing planner configuration uses these
finite defaults. The `new`, `launch`, and `retry` commands accept finite
per-run replacements through these flags:

- `--planner-calls-warn` and `--planner-calls-hard`
- `--planner-tokens-warn` and `--planner-tokens-hard`
- `--planner-elapsed-warn` and `--planner-elapsed-hard`

Overrides may be partial, but every resolved warning must be positive and
strictly below its hard limit. Zero, negative, non-finite, overflow, and
unlimited sentinel values are rejected before the planner starts.

Warnings are informational and do not stop planning. The first hard limit
closes exploration admission while allowing synthesis from the context already
collected. The resulting `budget_limited` outcome remains distinct from
configuration and provider errors.

Within one planner attempt, issue artifacts are considered before touchset
candidates, directly relevant code, and broader sources. Re-reading the same
source identity with the same fingerprint reuses the attempt context without a
new admission or charge; a changed fingerprint is charged as a new read.

Provider usage is reconciled when exact metadata is available. If it is not,
the finite reservation remains charged and the rail marks the token total as
estimated. Failed admitted calls remain charged, and retries consume a new
admission.

The rail projects one live usage snapshot with planner status, calls used/limit,
charged tokens/limit, elapsed/limit, warning dimensions, and the stopped
dimension when applicable. It can show `running`, `warning`, `normal`,
`budget_limited`, `configuration_error`, or `tool_error`; a stopped planner
still synthesizes from its bounded context.

Planner events contain usage metadata and stable source identity only. Source
contents, prompts, credentials, full tool arguments, and provider output never
enter durable events.
