// Package attach owns issue attachments end to end: client-side path
// resolution, daemon-side validation, canonical storage under the issue dir,
// per-stage materialization, and the ISSUE.md section that tells an agent the
// files are there.
package attach

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
