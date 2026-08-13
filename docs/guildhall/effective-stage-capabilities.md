A stage's authority comes from one immutable, provider-neutral effective
capability contract. Flow names, assigned packages, prompt text, provider tool
names, and legacy `allowed_tools` metadata are not authority.

The engine resolves authority before choosing or starting a provider. It binds
the contract to the issue, stage attempt, canonical workspace, starting Git
identity, exact read paths, path-and-mutation write grants, output ownership,
and these grantable operation classes:

- `workspace-read`
- `workspace-mutate`
- `local-process`
- `vcs-read`
- `vcs-commit`
- `planner-artifact-apply`

Push, merge, rebase, publication, authoritative verification, receipt
creation, lifecycle advancement, and cleanup have no grantable operation
class. They remain engine-only even if a package, prompt, provider, fallback,
or retry requests them.

Every flow stage declares one fixed `capability_profile`:

| Profile | Effective scope |
|---|---|
| `artifact` | Reads only exact files materialized for the attempt and writes only declared agent-owned outputs. Planner artifact application is a separate explicit operation. |
| `inspect` | Reads the enumerated repository view without workspace mutation or commit authority. |
| `implementation` | Mutates only paths from the archived touchset bound to the accepted plan review. |
| `review` | Uses the same approval-bound touchset for focused review repairs. |
| `librarian` | Mutates only the intersection of explicit `documentation_paths` and the approval-bound touchset. |
| `final-review` | Inspects and repairs only within the approval-bound touchset; it has no final-verification or lifecycle authority. |
| `conflict-resolution` | Edits only the engine-declared conflict paths that are also in the approval-bound touchset; the engine controls the rebase. |

`workspace: readonly` removes mutation, commit, and planner-apply authority from
any profile. A readonly stage that requires an agent-owned output is invalid.
All paths use the shared canonical touchset grammar; unsafe, root-wide, Git
metadata, engine-state, traversal, and escaping paths fail closed. Rename
requires both endpoints to be allowed.

For implementation, review, librarian, final-review, and conflict-resolution,
the mutable worktree `touchset.json` is never authority. The engine loads the
archived touchset named by the accepted plan-review binding, verifies its
digest, and compiles from that immutable copy. Librarian documentation scope is
also explicit: a Markdown extension, directory name, package, prompt, or file
contents cannot classify a path as documentation. The bundled flow declares
`docs/**` and `docs-draft-*`, but the approved touchset may narrow that set.

`verification.json` is always engine-owned. Other declared outputs are
agent-owned unless the engine classifies them otherwise. Agent-owned outputs
receive exact-path write grants in addition to any product scope; workflow
artifacts are never included in a mediated product commit.

The compiler emits contract version 1, a `ContractID`, and an
`AuthorityDigest`. The contract ID binds the stage attempt. The authority
digest omits only the attempt ID, so a recovered unchanged retry gets a new
contract identity while retaining the same effective authority. Legacy
`allowed_tools` may remove workspace-read, workspace-mutate, or local-process
authority during compilation; it cannot add an operation, path, credential, or
provider flag. Unknown legacy entries are configuration errors.

The execution order is fixed:

1. The engine materializes inputs and resolves immutable authority.
2. It compiles and persists the contract once for the stage attempt.
3. The selected adapter preflights every required enforcement control and
   persists a plan bound to the contract.
4. The engine captures a trusted filesystem and Git baseline.
5. The provider starts inside the contract-bound capability session.
6. Agent operations pass through the scoped gateway; direct unmanaged provider
   tools are denied.
7. All provider and descendant processes are reaped.
8. The engine compares the complete workspace/Git state with the baseline and
   validates runtime audit records, output ownership, commits, and mutations
   against the same contract.
9. Only a passing validation bound to the immutable stage result can authorize
   `runner_succeeded` and later artifact, gate, dependency, or lifecycle work.

Preflight proves workspace read/write mediation, descendant containment,
network separation, Git-common-directory and engine-state isolation, scratch
isolation, process-group control, gateway mediation, and credential
separation. Codex and Claude both receive only contract-derived Watchtower MCP
operations; their native filesystem, shell, Git, and web paths do not confer
authority. Provider transport may reach its model, while agent-requested local
processes use a separately scrubbed, network-denied environment. A provider or
platform that cannot prove the complete plan returns
`capability_provider_unsupported` before the provider process starts.

Runtime mediation canonicalizes paths immediately before filesystem actions,
uses exact mutation classes, runs local processes without a shell, and exposes
bounded VCS reads plus a hook-free, signing-free, single-parent local commit.
The commit compare-and-swaps only the issue branch from the contract's starting
commit and includes only approved product paths. A denied request terminates
the complete process tree and records `capability_runtime_denied`.

Final validation is provider-independent. The observer covers tracked and
untracked entries, file type and metadata, links, content identities, branch,
HEAD, tree, index, worktree state, local refs, the Git common directory, and a
redacted remote identity. It rejects out-of-scope creates, changes, deletes,
renames, metadata/link mutations, external targets, unmediated operations,
nonlinear history, unrelated ref changes, remote changes, missing agent
outputs, and agent-authored engine outputs as
`capability_post_stage_violation`.

The four stable policy reasons have different recovery boundaries:

- `capability_contract_invalid` — change invalid configuration or authority;
  no provider started.
- `capability_provider_unsupported` — supply a provider/platform that can prove
  the complete contract; no provider started.
- `capability_runtime_denied` — the attempted side effect was denied and the
  workspace requires engine-owned trusted recovery.
- `capability_post_stage_violation` — the complete result is rejected and the
  workspace requires engine-owned trusted recovery.

For the latter two, Watchtower records the rejected attempt, marks the exact
issue workspace `capability_recovery_needed`, and prevents its bytes from
feeding any later stage. Recovery validates the issue/worktree/repository/ref
identities, reconstructs the exact trusted commit and tree through the
workspace provider, and rematerializes engine-owned inputs. It never asks the
agent to clean up and never broadens authority. An unchanged retry compiles a
new contract with the same authority digest.

Capability attempt slots are immutable and exact-idempotent; policy audit
records are append-only and ordered. Durable evidence includes contract and
plan identities, baseline and result bindings, phase/outcome, stable reason,
provider implementation, normalized operation, and canonical path facts. It
excludes prompts, file contents, command payloads, environment values,
credentials, sockets, handles, and raw remote URLs. The setup inspector exposes
the safe durable projection described in setup-inspector; lifecycle ordering
and recovery are described in lane-ops-and-issue-states.
