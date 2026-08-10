Planner stages produce `plan.md` and `touchset.json` through a bounded,
sectioned session. These are the only durable planner outputs: the manifest,
checkpoint, request payload, and any temporary staging state remain private
coordinator state and are never archived or added to the worktree artifact
set.

## Authority and transport

Planner artifact writes use the engine-owned daemon route across the real
subprocess boundary:

```text
Codex exec_command -> installed watchtower CLI -> daemon JSONL route -> engine authority -> planner state
```

The engine is the sole authority. It creates or resumes an active record bound
to the exact tuple `(issue, stage, attempt, canonical worktree)`. The daemon
brokers capability issuance and structured requests for that record; the
installed CLI is a thin client and cannot initialize, replace, or widen it.

The capability is an opaque in-memory handle. Only its digest and safe planner
state are durable. The raw handle, request body, and private
`WATCHTOWER_PLANNER_SESSION` value must never appear in the worktree,
environment, argv, transcript, or logs. An inherited descriptor may remain as
compatibility metadata, but it is not an authorizing transport. Codex and
Claude use the installed CLI route; an in-process adapter is reserved for
deterministic fake-runner tests.

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
whole-file writes. The installed CLI sends that structured request to the
existing repository daemon; it does not fall back to fd 3 or a private session.
The decoded Markdown bytes plus the canonical serialized touchset delta must
be at most 65,536 bytes; the exact limit is valid and the first byte beyond it
is rejected before either target changes.

The first apply for an immutable section must contain final reviewed Markdown,
the next key in the immutable ordered manifest, and the real canonical
repository-relative glob delta for that section. Placeholder/probe content,
synthetic or unsafe globs, malformed data, and mismatched scope are rejected
before the pair or authority state changes. The daemon and engine validate the
entire request before publishing the paired artifacts and durable accepted
prefix.

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

Retry rotates the live capability while retaining the same exact scope and
validated prefix. After daemon recovery, the engine reloads and verifies its
durable authority record before issuing a new handle; it never reconstructs
authority from a descriptor, private session, or untrusted client files. If
the record or pair cannot be proven valid, recovery fails closed and preserves
the last validated state.

Stable failure classes distinguish transport and authority state:

| Class | Meaning |
| --- | --- |
| `transport_unavailable` | The daemon cannot be reached or cannot carry the request. |
| `authority_uninitialized` | The daemon is reachable, but no active engine authority exists. |
| `scope_mismatch` | Issue, stage, attempt, or canonical worktree differs. |
| `stale_capability` | The handle is expired, replaced, or otherwise invalid. |
| `private_session_rejected` | A client-created private planner session was presented. |
| `descriptor_non_authoritative` | An inherited descriptor was used as authority. |
| `invalid_section` | Manifest, content, ordering, size, replay, or path data is invalid. |
| `authority_state_unavailable` | Durable state or atomic publication cannot be proven. |

Diagnostics may expose safe scope identifiers, correlation IDs, section keys,
failure classes, and outcomes. They must not echo capability material,
capability digests, request JSON/Markdown, private-session values, or complete
sensitive worktree paths.

## Review gate

Complete validation requires every manifest section exactly once, valid paired
anchors in order, valid UTF-8 and Markdown structure, and a touchset equal to
the deduplicated union of all manifest deltas. The engine performs this pair
validation before archiving, artifact-produced events, or plan review. A
partial, malformed, over-limit, or inconsistent pair cannot reach review, and
the existing `plan_review` decision remains the only approval decision.

Codex, Claude, and deterministic fake planners use the same provider-neutral
request contract. The production providers invoke the installed CLI and
daemon; fake planners may inject an in-process adapter only in tests. Retries
reuse the retained worktree; no network call or additional persisted recovery
artifact is required.

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
