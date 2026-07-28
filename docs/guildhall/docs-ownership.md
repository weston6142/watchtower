The librarian is the sole writer of `docs/` on the default branch. Per-issue
doc agents draft inside their worktree; after the merge the librarian folds
those drafts into one voice, resolves contradictions, and curates this
directory. If you are not the librarian stage, do not edit `docs/` — put the
draft in your worktree and let reconciliation place it.

Where things live:

- `docs/guildhall/*.md` — **this directory: curated project memory.** Every
  `.md` here is concatenated (sorted by filename, each under a `## <filename>`
  heading) and injected into *every* stage's `ISSUE.md` for *every* future
  issue, under "Project memory (curated by the Librarian)". So: one topic per
  file, no top-level `#` heading (it would sit redundantly under the injected
  `##`), and only facts a stage would otherwise get *wrong* — not summaries of
  code a reader can see. Filenames are user-visible; keep them descriptive and
  skip numeric prefixes.
- `docs/superpowers/specs/` and `docs/superpowers/plans/` — dated design specs
  and implementation plans, one pair per issue. **Historical records**: they
  describe what was decided then, and are not rewritten as the code moves. Only
  their `Status:` line is kept current. Current truth lives in
  `docs/guildhall/`.
- Two design docs (`docs/2026-07-28-*-design.md`) sit at the `docs/` root
  instead. Plans cross-reference those exact paths, so they stay put; new specs
  go under `docs/superpowers/specs/`.

`ISSUE.md` is how a stage learns what it is working on: issue header + body,
then the injected project memory. Earlier stages' artifacts (`brainstorm.md`,
`spec.md`, `plan.md`, …) sit alongside it in the same workdir.
