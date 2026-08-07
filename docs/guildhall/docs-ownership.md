The librarian is the sole workflow stage that writes `docs/`. It runs in the
issue worktree after correctness and clean-code review but before final
verification and integration. It folds relevant drafts into one voice,
resolves contradictions, curates this directory, and commits the documentation
on the issue branch. There is no post-merge documentation session.

A draft only reaches the librarian if it is committed on the issue branch, so
commit it at the repository root as `docs-draft-<topic>.md`. That path is the
handoff channel, not a mistake: the librarian folds the content into
`docs/guildhall/` in house style and deletes the root file in the same reconcile
commit. Nothing named `docs-draft-*` should ever persist on `develop`.

Where things live:

- `docs/guildhall/*.md` — **this directory: curated project memory.** Every
  `.md` here is concatenated (sorted by filename, each under a `## <filename>`
  heading) and injected into *every* stage's `ISSUE.md` for *every* future
  issue, under "Project memory (curated by the Librarian)". So: one topic per
  file, no top-level `#` heading (it would sit redundantly under the injected
  `##`), and only facts a stage would otherwise get *wrong* — not summaries of
  code a reader can see. Filenames are user-visible; keep them descriptive and
  skip numeric prefixes. Cross-reference sibling files by name in prose —
  `[[wiki-link]]` syntax belongs to a different memory system and is not house
  style here. Every file is a standing context tax on all future issues, so
  compress: cut anything a stage could grep.
- `docs/superpowers/specs/` and `docs/superpowers/plans/` — dated design specs
  and implementation plans, one pair per issue. **Historical records**: they
  describe what was decided then, and are not rewritten as the code moves. Only
  their `Status:` line is kept current. Current truth lives in
  `docs/guildhall/`.
- Two design docs (`docs/2026-07-28-*-design.md`) sit at the `docs/` root
  instead. Plans cross-reference those exact paths, so they stay put; new specs
  go under `docs/superpowers/specs/`.

`ISSUE.md` is how a stage learns what it is working on, in three blocks: issue
header + body, then the attachments section if the issue has any, then the
injected project memory. Earlier stages' artifacts (`brainstorm.md`, `spec.md`,
`plan.md`, …) sit alongside it in the same workdir.

Final verification has three artifacts with deliberately separate ownership:

- `merge-report.md` is human-readable evidence and may contain explanations,
  rationale, and command output.
- `merge-decision.json` remains agent-authored, while `verification.json` is
  daemon-authored when `test_cmd` is configured; repositories without a
  `test_cmd` retain the agent-authored receipt path. Both are strict machine
  contracts: their fields are exact, and prose or extra keys belong in the
  report. Cache-managed verification adds strict `cache_evidence` to the
  daemon-authored receipt; it binds the receipt to the complete lease and
  exact verification identity.
- The engine injects the canonical contract into `STAGE.md`, validates the
  archived receipts, and persists `verification_ready` before it declares the
  stage complete. Transcript completion is never lifecycle authority.

After that checkpoint, integration, publication, cleanup retry, and automatic
restart recovery are non-model operations. Only an invalid or unvalidated
receipt causes `R` to rerun the merge verifier.
