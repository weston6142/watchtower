Cache-managed verification is entirely engine-owned. The final-review agent
must first exit, all descendants must be reaped, and its complete workspace/Git
delta and immutable result binding must pass capability validation. Only then
does the engine acquire a repository-neutral lease, run the configured
verification command, create the receipt, and seal cache evidence. An
integrating flow without a non-empty `test_cmd` is invalid.

The runtime lives below the per-repository daemon data directory, uses the
canonical Git common-directory path as repository identity, and keeps its
state outside issue worktrees and workflow-artifact scans.

The lease's explicit child environment belongs only to the engine verification
process. The agent and provider never receive the lease, its managed cache
paths, cache authority, or receipt writer. `GOCACHE`, `GOMODCACHE`, and
`GOPATH` point to lease-specific staging paths for engine replay. The configured
verification command remains exact argv without a shell layer, and its
canonical JSON argv digest is part of the lease identity.

Lease state is monotonic: `active` can become `complete` or `quarantined`.
Only a validated complete snapshot may seed a new attempt, and seeding is
copy-on-write so a known-good snapshot is never opened for writes. A live lease
is never stolen. Failed, interrupted, malformed, corrupt, stale, or
identity-mismatched state is retained as machine-readable quarantine evidence;
retries acquire a fresh lease and do not delete or repair the prior complete
snapshot. When finalization detects stale lease or cache identity in a
verification receipt, the explicit retry must follow that same fresh-lease
path; it must not relabel the old receipt or retry automatically. There is no
automatic cache cleanup or retention policy.

For cache-managed verification, engine-authored `verification.json` includes
strict `cache_evidence` bound to the repository, base/branch/tree SHAs, lease
identity, managed scope, complete state, seed identity, quarantine records, and
the exact command argv digest. The configured command still runs when a
complete snapshot exists. `verification_ready` is created only after the
command passes and the receipt plus complete lease evidence validate together
against the current repository and bound capability result; cache completion
alone is never proof. Inherited environment values and secrets are not
persisted in cache manifests or receipts.
