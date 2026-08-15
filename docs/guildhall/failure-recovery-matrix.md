# Deterministic failure and recovery matrix

GH-69 exercises the shipped Watchtower engine with an offline, disposable Git
repository and deterministic clock. The normal repository gate is:

```sh
scripts/verify
```

The gate captures `HEAD`, runs two complete unfiltered matrix executions at
that revision, compares normalized observations, then runs race tests, vet,
build, and `git diff --check`. It writes a completion receipt only after every
check passes, in the worktree-local directory:

```sh
.watchtower/matrix-receipts
```

Receipts are private, atomically written JSON files. A changed revision,
filtered or partial run, skipped or failed scenario, unequal normalized
result, or failed repository check invalidates completion.

For bounded diagnosis, select one stable scenario without an evidence path:

```sh
go run ./cmd/watchtower-matrix run \
  --manifest internal/engineharness/testdata/failure-recovery-matrix.yaml \
  --flow internal/scaffold/defaults/flows/default.yaml \
  --revision "$(git rev-parse HEAD)" \
  --scenario failure/execute/runner
```

The manifest contains stable IDs for every applicable failure-family slot,
all 48 stage-boundary restarts, six finalization boundaries, and eight
cross-cutting cases. The harness records public outcomes, durable state,
normalized classifications, artifact identities, effects, and bounded
checkpoint diagnostics while omitting temporary paths, timestamps, and record
IDs.

Real-provider smoke tests are supplementary and opt-in. They are not part of
`scripts/verify`, may not use or create the deterministic receipt, and GH-69
introduces no real-provider smoke suite.
