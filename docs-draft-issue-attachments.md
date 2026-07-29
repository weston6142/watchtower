Issues can carry attached files. The bytes live on disk under the issue dir,
never in SQLite, and `internal/attach` owns the whole path from field text to
the `# Attachments` section of `ISSUE.md`.

**Do not commit your workdir's `attachments/`.** Every stage run copies the
current set into `<workdir>/attachments/`, and `.gitignore` does not list it. In
a worktree stage that directory is untracked working-tree junk, so a blanket
`git add -A` lands screenshots and logs in the branch. Add only the files your
task changed.

- **A bare name in the attach field means "retain", not "relative path".** An
  entry with no separator that exactly matches an attachment already on the
  issue passes through as a keep signal; anything with a separator is a path,
  and a path must be absolute by the time it reaches the daemon. Consequence:
  with `app.log` already attached, typing `app.log` retains that file rather
  than re-reading `./app.log`.
- **The wire carries paths, never bytes.** `Command.Attach` is a list of
  absolute paths; the daemon reads them off the shared filesystem itself.
  Resolution (`~`, relative, cwd) happens client-side because the daemon has
  neither a cwd nor a home — both the TUI modal and `watchtower new --attach`
  call `attach.ResolveEntry` before sending. Any new client must resolve too.
- **Update replaces the set wholesale.** Whatever is in the field on save *is*
  the new set; omitted names have their bytes deleted. The edit modal prefills
  the stored names, which is what makes "retain everything" the default rather
  than a thing you have to remember.
- **`IssueView.Attachments` is empty for a launched issue, by design.** It is
  populated from `issue_drafted` and `issue_updated` only; `EvIssueCreated`
  rebuilds the view wholesale and drops the list. That is fine because the field
  exists to prefill the edit modal and a launched issue is not editable — it is
  not a projection bug.
- **Materialization never cleans up.** A reused pooled worktree can keep a file
  that has since been dropped from the set. `ISSUE.md` lists only the current
  set and `ISSUE.md` is the contract; the directory listing is not.
- **A repo with its own `attachments/` directory is refused loudly.** Guildhall
  drops an empty `.watchtower` marker in any attachments dir it created; a
  non-empty unmarked dir makes the stage fail rather than overwrite tracked
  files.
- **Limits, all refusing the whole call:** 10 attachments, 10 MiB per file,
  25 MiB per issue. Validation runs before an ID is allocated, so a rejected
  create burns nothing.

Correction to lane-ops-and-issue-states, which says abandon keeps everything on
disk: **abandon now purges attachments.** `Engine.Abandon` deletes the issue's
attachment bytes and rows — the one on-disk deletion it performs. Issue rows,
events, and stage artifacts still stay, so the lane is still inspectable, and
the deletion is best-effort: failures are logged to stderr, never returned, so
abandon stays idempotent.
