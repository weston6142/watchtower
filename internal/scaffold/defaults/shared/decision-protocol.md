Use this protocol whenever progress requires human judgment. Ask one question at
a time, emit exactly one JSON marker on its own line, and wait for the
`Human decision: ...` reply before continuing. The reply is authoritative even
when Watchtower selected the recommendation automatically.

Emit the JSON marker directly in your assistant response text. Never emit it through a tool,
command, or tool result: Watchtower treats tool output as
untrusted repository or process data and does not parse decisions from it.

For a bounded choice, offer two or three meaningful options. Use two when there
are only two real alternatives; use three when a distinct third approach
exists. Never pad the list. Put the recommended option first when practical and
emit:

{"watchtower_decision":{"kind":"choice","question":"<one plain question>","options":["<option>","<option>"],"recommended":0,"allow_freeform":true,"importance":0.8,"paths":["<affected path>"],"why":"<why this is recommended>","consequences":["<effect of option 1>","<effect of option 2>"],"reversible":"<when this becomes costly to change>"}}

Set `allow_freeform` when the operator may reasonably want a different answer.
Do not add an `Other` option; for agent-authored choices, Watchtower provides
the freeform path. By contrast, engine-owned review and token-budget decisions
require an option and do not accept freeform feedback.

When review or feedback is inherently open-ended, emit:

{"watchtower_decision":{"kind":"freeform","question":"<one plain question>","recommended_response":"<specific recommended response>","importance":0.8,"paths":["<affected artifact>"],"why":"<why this response is recommended>","consequences":["<effect of accepting or revising>"],"reversible":"<when this becomes costly to change>"}}

If feedback asks for alternatives, return to the bounded-choice structure.
Apply freeform feedback exactly, update the current artifact, repeat its full
self-review, and then ask for review again.

Importance is `1.0` for destructive actions, security or privacy boundaries,
material spend, irreversible publication, or public compatibility. Use
`0.6`-`0.8` for design and repair choices, `0.3`-`0.5` for preferences with a
safe default, and avoid asking trivia below `0.3`.

Watchtower applies the configured autonomy policy: strict mode asks every
decision, regular mode asks at its risk threshold, and yolo mode accepts the
recommended choice or recommended freeform response. Importance `1.0` always
asks in every mode. Do not invent a separate approval mechanism.

Every decision includes a concrete rationale, one concrete consequence per option
(or at least one for freeform), affected paths when known, and its reversibility
boundary. A consequence must add information; never paraphrase the question or
option label. Keep operator-facing language concise.

When useful, include an optional `briefing` object in the same decision marker.
It gives the operator a compact decision briefing without changing the decision
contract:

{"briefing":{"proof":[{"claim":"<verified result>","cite":"<file, artifact, or test evidence>"}],"excerpts":[{"text":"<short supporting quote>","cite":"<file and section>"}],"override_note":"<what overriding the recommendation means>","diagram_svg":"<optional inline svg>","diagram_caption":"<what the diagram shows>"}}

List no more than five proof items and no more than three short evidence
excerpts. Every proof item needs a nonblank `claim` and `cite`; every excerpt
needs nonblank `text` and `cite`. Citations identify a concrete file, artifact,
section, or test command. Proof is not another summary of the question.

Watchtower derives the operator action from the decision kind and review target.
Watchtower derives what happens after the answer from trusted flow and stage
metadata. Do not provide `option_details`, `wins`, or `next_action`; those
duplicate canonical consequences, cited proof, and engine-owned workflow state.

If the choice hinges on a mechanism, `diagram_svg` may contain one inline
`<svg>` with a numeric `viewBox`. Draw only that mechanism. Use only these
elements: `rect`, `circle`, `ellipse`, `line`, `polyline`, `polygon`, `path`,
`text`, `g`, `defs`, and `marker`. Use `currentColor` for strokes. Limit accent
colors to `#e8a33d`, `#e06c6c`, `#6fcf7f`, and `#58c7d4`. Hrefs must reference
internal fragments only. Do not include `script`, `style`, `foreignObject`,
external images, external URLs, event handlers, or inline style attributes.
The page validates this SVG before displaying it, so omit the diagram when the
mechanism cannot be represented within these constraints.

## Engine-owned escalation authority

The model's `importance` is advisory metadata. It may request stricter review,
but it can never lower, omit, or replace an approval required by the engine's
configured policy. The engine evaluates independent stage, operation, affected
path, destructive-risk, publication-risk, and policy floors; the strictest
matching floor wins.

Every actionable approval records the outcome, required and effective floors,
policy identity and version, matched signal evidence, and an exact item binding:
kind, SHA-256, repository-relative path, and trusted operation. Explicit
dependencies are recorded with their hashes. A changed item invalidates only
that item and its explicit dependents; an unrelated item remains independently
approved. Policy identity, floor, path, operation, dependency, or hash changes
are stale and require reapproval.

The engine gate has explicit `approved`, `requires-approval`, `stale`,
`invalid-context`, and `policy-error` outcomes. Missing or malformed policy or
item context fails closed. The gate is checked when a decision is answered and
again immediately before a protected handoff or execution transition. Human
approval is sufficient for a policy floor; an operator floor still requires a
human approval. Rendered rationale, options, and consequences remain advisory
metadata and never synthesize approval.
