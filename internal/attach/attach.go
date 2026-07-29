// Package attach owns issue attachments end to end: client-side path
// resolution, daemon-side validation, canonical storage under the issue dir,
// per-stage materialization, and the ISSUE.md section that tells an agent the
// files are there.
package attach

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/weston6142/watchtower/internal/store"
)

const (
	// maxFileBytes fits a screenshot or a rotated log and not a core dump.
	maxFileBytes = 10 << 20
	// maxIssueBytes bounds what one issue can land in a worktree.
	maxIssueBytes = 25 << 20
	// maxAttachments bounds the ISSUE.md section and the modal's one-line field.
	maxAttachments = 10
	// dirName is the attachments directory, both canonical and per-stage.
	dirName = "attachments"
	// markerName is the empty file Guildhall writes into an attachments dir it
	// created, so a repo's own attachments/ is never overwritten.
	markerName = ".watchtower"
)

// Item is one validated attachment. SourcePath is the absolute path bytes are
// read from; a Retained item is already stored under the issue dir and its
// SourcePath is only provenance.
type Item struct {
	Name       string
	SourcePath string
	Size       int64
	Retained   bool
}

// Set is a validated attachment set: the whole set an issue will have, in the
// order the entries were given.
type Set struct {
	Items []Item
}

// Names returns the stored names in order.
func (s Set) Names() []string {
	if len(s.Items) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.Items))
	for _, item := range s.Items {
		out = append(out, item.Name)
	}
	return out
}

// Plan validates entries against the attachments already on an issue and
// returns the whole new set. Any failure refuses the entire call: Plan needs no
// issue ID, which is what lets a caller validate before allocating one.
func Plan(existing []store.AttachmentRow, entries []string) (Set, error) {
	var set Set
	taken := map[string]bool{}
	var total int64
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if row, ok := retainRow(existing, entry); ok {
			if taken[row.Name] {
				continue // the same retain typed twice is still one file
			}
			taken[row.Name] = true
			total += row.Size
			set.Items = append(set.Items, Item{
				Name: row.Name, SourcePath: row.SourcePath, Size: row.Size, Retained: true})
			continue
		}
		if !hasSeparator(entry) {
			return Set{}, fmt.Errorf(
				"attachment %q: not an existing attachment and not an absolute path", entry)
		}
		if !filepath.IsAbs(entry) {
			return Set{}, fmt.Errorf("attachment %q: path must be absolute", entry)
		}
		info, err := os.Stat(entry)
		if errors.Is(err, os.ErrNotExist) {
			return Set{}, fmt.Errorf("attachment %q: no such file", entry)
		}
		if err != nil {
			return Set{}, fmt.Errorf("attachment %q: %w", entry, err)
		}
		if !info.Mode().IsRegular() {
			return Set{}, fmt.Errorf("attachment %q: not a regular file", entry)
		}
		if info.Size() > maxFileBytes {
			return Set{}, fmt.Errorf("attachment %q: %s exceeds the 10 MiB per-file limit",
				entry, humanBytes(info.Size()))
		}
		name := uniqueName(filepath.Base(entry), taken)
		taken[name] = true
		total += info.Size()
		set.Items = append(set.Items, Item{Name: name, SourcePath: entry, Size: info.Size()})
	}
	if len(set.Items) > maxAttachments {
		return Set{}, fmt.Errorf("%d attachments exceeds the limit of %d",
			len(set.Items), maxAttachments)
	}
	if total > maxIssueBytes {
		return Set{}, fmt.Errorf("attachments total %s exceeds the 25 MiB per-issue limit",
			humanBytes(total))
	}
	return set, nil
}

// retainRow reports whether entry names an attachment already on the issue. A
// bare name is the retain signal; anything holding a separator is a path.
func retainRow(existing []store.AttachmentRow, entry string) (store.AttachmentRow, bool) {
	if hasSeparator(entry) {
		return store.AttachmentRow{}, false
	}
	for _, row := range existing {
		if row.Name == entry {
			return row, true
		}
	}
	return store.AttachmentRow{}, false
}

func hasSeparator(entry string) bool {
	return strings.ContainsRune(entry, '/') || strings.ContainsRune(entry, filepath.Separator)
}

// uniqueName inserts a numeric suffix before the extension until the name is
// free: app.log -> app-2.log; Makefile -> Makefile-2.
func uniqueName(base string, taken map[string]bool) string {
	if !taken[base] {
		return base
	}
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, n, ext)
		if !taken[candidate] {
			return candidate
		}
	}
}

// humanBytes formats a size the way the error messages and ISSUE.md quote it.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// Rows converts a validated set into store rows, ord = index so display order
// equals the order typed in the field.
func Rows(issueID string, s Set, now time.Time) []store.AttachmentRow {
	if len(s.Items) == 0 {
		return nil
	}
	rows := make([]store.AttachmentRow, 0, len(s.Items))
	for i, item := range s.Items {
		rows = append(rows, store.AttachmentRow{
			IssueID: issueID, Name: item.Name, Size: item.Size,
			SourcePath: item.SourcePath, AddedAt: now, Ord: i,
		})
	}
	return rows
}

// Save copies the set's incoming bytes into <issueDir>/attachments/, creating
// the directory itself: a draft has an ID but no issue dir, because only
// runStageOnce ever creates one today. Retained items are already there.
func Save(issueDir string, s Set) error {
	if len(s.Items) == 0 {
		return nil
	}
	dir := filepath.Join(issueDir, dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, item := range s.Items {
		if item.Retained {
			continue
		}
		if err := copyFile(item.SourcePath, filepath.Join(dir, item.Name)); err != nil {
			return fmt.Errorf("attachment %q: %w", item.Name, err)
		}
	}
	return nil
}

// Materialize puts the current set where the running stage can read it. For a
// workspace: "none" stage dstDir is the issue dir itself and the files are
// already in place, so it is a no-op — never a self-copy. There is no cleanup
// pass: a reused pooled worktree may keep a file dropped from the set, which is
// harmless because ISSUE.md lists only the current set and ISSUE.md is the
// contract.
func Materialize(dstDir, srcDir string, rows []store.AttachmentRow) error {
	if len(rows) == 0 {
		return nil
	}
	if filepath.Clean(dstDir) == filepath.Clean(srcDir) {
		return nil
	}
	dir := filepath.Join(dstDir, dirName)
	if err := claimDir(dir); err != nil {
		return err
	}
	for _, row := range rows {
		src := filepath.Join(srcDir, dirName, row.Name)
		if err := copyFile(src, filepath.Join(dir, row.Name)); err != nil {
			return fmt.Errorf("attachment %q: %w", row.Name, err)
		}
	}
	return nil
}

// claimDir makes dir Guildhall's or refuses loudly. A directory Guildhall
// created holds an empty marker file; a non-empty unmarked directory is the
// repo's own, and overwriting a tracked attachments/app.log would be silent
// data loss.
func claimDir(dir string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, markerName), nil, 0o644)
	case err != nil:
		return err
	case len(entries) == 0:
		return os.WriteFile(filepath.Join(dir, markerName), nil, 0o644)
	}
	for _, entry := range entries {
		if entry.Name() == markerName {
			return nil
		}
	}
	return fmt.Errorf("%s already exists and is not Guildhall's — this repo uses its own "+
		"attachments/ directory, so issue attachments cannot be used here", dir)
}

// DeleteDropped removes the bytes of attachments the issue had and the new set
// does not. Callers run it last, so a mid-sequence failure leaves extra bytes
// rather than a missing file the table still claims.
func DeleteDropped(issueDir string, existing []store.AttachmentRow, s Set) error {
	keep := make(map[string]bool, len(s.Items))
	for _, item := range s.Items {
		keep[item.Name] = true
	}
	dir := filepath.Join(issueDir, dirName)
	for _, row := range existing {
		if keep[row.Name] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, row.Name)); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// DeleteAll reclaims every attachment byte for an issue. Missing is success.
func DeleteAll(issueDir string) error {
	return os.RemoveAll(filepath.Join(issueDir, dirName))
}

// Section is the ISSUE.md block that tells an agent the files exist. Empty for
// an issue with no attachments.
func Section(rows []store.AttachmentRow) string {
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n# Attachments\n\n")
	b.WriteString("The person who filed this issue attached these files. Paths are relative to\n")
	b.WriteString("this directory.\n\n")
	for _, row := range rows {
		fmt.Fprintf(&b, "- `%s/%s` (%s)\n", dirName, row.Name, humanBytes(row.Size))
	}
	return b.String()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ResolveEntry resolves one field entry on the client, which is the only side
// that knows the user's cwd and home. An entry that exactly names an
// attachment already on the issue and holds no separator passes through
// verbatim — that is the retain signal, and it is why prefilling the edit modal
// with stored names makes "retain everything" the default. Consequence worth
// knowing: with app.log already attached, typing app.log retains it rather than
// re-reading ./app.log. An empty entry resolves to "".
func ResolveEntry(entry string, existing []string, cwd, home string) (string, error) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return "", nil
	}
	if !hasSeparator(entry) {
		for _, name := range existing {
			if name == entry {
				return entry, nil
			}
		}
	}
	if home != "" {
		if entry == "~" {
			entry = home
		} else if strings.HasPrefix(entry, "~/") {
			entry = filepath.Join(home, entry[2:])
		}
	}
	if filepath.IsAbs(entry) {
		return filepath.Clean(entry), nil
	}
	if cwd == "" {
		return "", fmt.Errorf("attachment %q: cannot resolve a relative path without a working directory", entry)
	}
	return filepath.Join(cwd, entry), nil
}

// Resolve splits a comma-separated field value and resolves each entry,
// dropping empties. Comma, not space: paths may contain spaces.
func Resolve(field string, existing []string, cwd, home string) ([]string, error) {
	var out []string
	for _, entry := range strings.Split(field, ",") {
		resolved, err := ResolveEntry(entry, existing, cwd, home)
		if err != nil {
			return nil, err
		}
		if resolved != "" {
			out = append(out, resolved)
		}
	}
	return out, nil
}
