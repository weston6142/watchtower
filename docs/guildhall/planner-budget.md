# Planner budget

Planner exploration is bounded by finite defaults for calls, charged tokens,
and elapsed time:

```yaml
planner_budget:
  calls: {warn: 24, hard: 32}
  tokens: {warn: 200000, hard: 250000}
  elapsed: {warn: 8m, hard: 10m}
```

Warnings are informational and do not stop planning. The first hard limit
closes exploration admission while allowing synthesis from the context already
collected. The resulting `budget_limited` outcome remains distinct from
configuration and provider errors.

Per-run command flags can replace finite values for calls, tokens, and elapsed
time. Every override must be positive, finite, and have a warning below its
hard limit; unlimited sentinel values are rejected.

Provider usage is reconciled when exact metadata is available. If it is not,
the finite reservation remains charged and the rail marks the token total as
estimated. Failed admitted calls remain charged, and retries consume a new
admission.

Planner events contain usage metadata and stable source identity only. Source
contents, prompts, credentials, full tool arguments, and provider output never
enter durable events.
