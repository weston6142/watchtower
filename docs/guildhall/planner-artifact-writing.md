Planner stages produce `plan.md` and `touchset.json` through a bounded,
sectioned session. These are the only durable planner outputs: the manifest,
checkpoint, request payload, and any temporary staging state remain private
coordinator state and are never archived or added to the worktree artifact
set.

## Section contract

The planner creates one deterministic ordered manifest with these required
sections:

- `goal`
- `architecture`
- `technology-stack`
- `execution-contract`
- `file-structure`
- one or more zero-padded `task-NNNN` sections
- `verification`

Each accepted section is represented in `plan.md` by a paired invisible
anchor. Anchors are unique and remain in manifest order:

```text
<!-- watchtower-section: key=goal -->
Section Markdown content.
<!-- watchtower-section-end: key=goal -->
```

The section's canonical touchset delta is merged into `touchset.json` as
structured JSON. Exact duplicate globs are removed; every glob must be a
non-empty repository-relative path.

## Bounded writes and recovery

The planner submits exactly one section request at a time through a private
request file or standard input, using the provider-neutral
`watchtower planner-artifact apply` boundary. Section content must not be
placed in command-line arguments, generated patches, or shell-quoted
whole-file writes. The decoded Markdown bytes plus the canonical serialized
touchset delta must be at most 65,536 bytes; the exact limit is valid and the
first byte beyond it is rejected before either target changes.

The engine initializes both targets before the planner starts. A new session
starts with a valid plan root and `{"globs":[]}`. Existing sectioned progress
is adopted without rewriting it. Legacy unanchored plans, malformed targets,
unsafe paths, and ambiguous or partial markers fail closed without overwrite.

After each accepted section, both targets pass intermediate validation before
the next section is requested. A failed transport, decode, size, schema, or
section-validation operation leaves the last validated bytes intact. Retry
reads the existing anchors, skips byte-equivalent accepted sections, and
regenerates only the first pending section. Accepted sections are never
rewritten. Replaying accepted content is an idempotent no-op; conflicting
content, duplicate anchors, and out-of-order sections are rejected, while
exact duplicate globs are deduplicated during structured merge.

## Review gate

Complete validation requires every manifest section exactly once, valid paired
anchors in order, valid UTF-8 and Markdown structure, and a touchset equal to
the deduplicated union of all manifest deltas. The engine performs this pair
validation before archiving, artifact-produced events, or plan review. A
partial, malformed, over-limit, or inconsistent pair cannot reach review, and
the existing `plan_review` decision remains the only approval decision.

Codex, Claude, and deterministic fake planners use the same provider-neutral
request contract. Retries reuse the retained worktree; no network call or
additional persisted recovery artifact is required.

Plan review is one protected engine decision, not an approval inferred from the
planner transcript. The global escalation policy evaluates the plan and
touchset artifacts using independent stage, operation, path, execution, and
publication floors; the strictest floor wins. The decision record exposes the
policy ID/version, required and effective floors, matched signals, exact
SHA-256/path/operation bindings, and any explicit dependency evidence.

Model importance and rationale are advisory metadata. They may ask for stricter
review but cannot downgrade or omit an engine-required approval. Human or
permitted policy approval is checked against the exact binding at answer time
and again before execution. A changed artifact, policy identity, floor,
operation, or explicit dependency returns stale; missing or malformed context
returns invalid-context or policy-error. These outcomes are fail-closed and do
not authorize execution. `plan.md` and `touchset.json` remain the durable
planner outputs; the engine-owned plan-review decision is the only authorization
boundary for handing them to the next stage.
