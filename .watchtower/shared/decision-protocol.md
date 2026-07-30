Use this protocol whenever progress requires human judgment. Ask one question at
a time, emit exactly one JSON marker on its own line, and wait for the
`Human decision: ...` reply before continuing. The reply is authoritative even
when Watchtower selected the recommendation automatically.

For a bounded choice, offer two or three meaningful options. Use two when there
are only two real alternatives; use three when a distinct third approach
exists. Never pad the list. Put the recommended option first when practical and
emit:

{"watchtower_decision":{"kind":"choice","question":"<one plain question>","options":["<option>","<option>"],"recommended":0,"allow_freeform":true,"importance":0.8,"paths":["<affected path>"],"why":"<why this is recommended>","consequences":["<effect of option 1>","<effect of option 2>"],"reversible":"<when this becomes costly to change>"}}

Set `allow_freeform` when the operator may reasonably want a different answer.
Do not add an `Other` option; Watchtower provides the freeform path.

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

Every decision includes a concrete rationale, one consequence per choice (or at
least one for freeform), affected paths when known, and its reversibility
boundary. Keep operator-facing language concise.
