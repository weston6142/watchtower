Cache-managed verification uses one repository-neutral lease from the start of
the merge-verifier agent through daemon replay, receipt creation, and cache
sealing. The runtime lives below the per-repository daemon data directory,
uses the canonical Git common-directory path as repository identity, and keeps
its state outside issue worktrees and workflow-artifact scans.

The lease supplies the same explicit child environment to the agent and daemon
replay. `GOCACHE`, `GOMODCACHE`, and `GOPATH` point to lease-specific staging
paths; runners only propagate those values and never select or interpret cache
state. The configured verification command remains exact argv without a shell
layer, and its canonical JSON argv digest is part of the lease identity.

Lease state is monotonic: `active` can become `complete` or `quarantined`.
Only a validated complete snapshot may seed a new attempt, and seeding is
copy-on-write so a known-good snapshot is never opened for writes. A live lease
is never stolen. Failed, interrupted, malformed, corrupt, stale, or
identity-mismatched state is retained as machine-readable quarantine evidence;
retries acquire a fresh lease and do not delete or repair the prior complete
snapshot. There is no automatic cache cleanup or retention policy.

For cache-managed verification, daemon-authored `verification.json` includes
strict `cache_evidence` bound to the repository, base/branch/tree SHAs, lease
identity, managed scope, complete state, seed identity, quarantine records, and
the exact command argv digest. The configured command and daemon replay still
run when a complete snapshot exists. `verification_ready` is created only
after replay passes and the receipt plus complete lease evidence validate
together; cache completion alone is never proof. Inherited environment values
and secrets are not persisted in cache manifests or receipts.
