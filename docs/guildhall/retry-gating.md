# Retry gating

Watchtower authorizes a repeated stage or verification attempt from durable
failure evidence. Automatic stage retries and `watchtower retry` use the same
retry context, counters, and policy. A caller cannot supply its own failure
class, fingerprint, counter, cap, or state digest.

## Policy

The bundled policy defaults are:

```yaml
retry_policy:
  policy_id: retry-v1
  policy_version: "1"
  transient_limit: 2
  deterministic_limit: 1
  model_resample_limit: 1
  model_resample_classes: [execution, transport, protocol]
```

Omitting `retry_policy` from `.watchtower/config.yaml` uses these defaults. If
the section is present, it must be complete: missing, zero, negative,
malformed, or unknown values make repository configuration invalid instead of
restoring a hidden fallback.

Automatic and explicit retries consume the same shared count for one issue,
stage, failure site, failure class, and failure fingerprint. A state change can
unlock the next attempt but does not replenish that count.

Transient failures may retry within `transient_limit`. Deterministic failures
may retry within `deterministic_limit` and require a change in at least one
independently compared state dimension:

- `tree`: repository, branch commit, and Git tree identity;
- `config`: stage, flow, retry, and verification configuration identity;
- `environment`: runtime, tool, and verification-cache identity; and
- `decision`: durable decisions and model-sample identity.

Unavailable evidence is not a change. Watchtower rejects closed if any
required dimension, retry context, configured policy, or atomic store update
cannot be trusted. One compare-and-update conflict reloads and reevaluates the
context; a second conflict rejects without starting work.

## Model resampling

Model resampling is explicit-only, limited to the configured failure classes,
and separately capped. Select a newer durable decision that belongs to the
failed issue and stage:

```text
watchtower retry GH-65 --model-resample 42
```

The selected decision must be newer than the retry context. Its canonical
durable identity is hashed into decision state before authorization. A model
resample consumes both the context's shared allowance and
`model_resample_limit`; it cannot bypass unchanged or missing decision state.

## Rejections and next actions

Retry results are recorded as `retry_authorized` or `retry_rejected` events and
appear in issue detail. Stable rejection reasons are:

| Reason | Operator action |
|---|---|
| `invalid_retry_context` | Record fresh durable failure evidence; do not infer a class or fingerprint. |
| `fingerprint_unavailable` | Restore the named tree, config, environment, or decision evidence. |
| `state_unchanged` | Change one relevant durable dimension before repeating deterministic verification. |
| `retry_kind_not_allowed` | Use an ordinary explicit retry or select a newer eligible model decision. |
| `retry_cap_exhausted` | Establish a genuinely new failure context or follow specialized/operator recovery. |
| `model_resample_cap_exhausted` | Change non-model state or follow operator recovery. |
| `retry_persistence_unavailable` | Restore durable retry storage and retry-context validation. |
| `retry_persistence_conflict` | Reload after the concurrent retry finishes; Watchtower will not loop blindly. |
| `failure_non_retryable` | Follow operator recovery guidance instead of repeating verification. |

A rejected generic retry leaves the issue terminal and retryable. It does not
start a runner, model, verifier, merge, publication, or cleanup action, and it
does not consume allowance.

## Privacy and specialized recovery

Retry records and events contain fixed classifications, SHA-256 digests,
counters, finite caps, policy identity, changed dimension names, reasons, and
next actions. They never contain raw environment values, command text,
exception text, artifact bytes, decision text, model output, or capability
material.

`verification_ready`, `publish_pending`, and `cleanup_needed` remain
checkpoint-specific recovery states. Their retry paths resume verified
finalization, publication, or cleanup without invoking the generic retry gate
or a model.
