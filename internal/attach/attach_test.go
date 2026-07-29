package attach

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/store"
)

// writeFile creates a file of exactly size bytes and returns its absolute path.
func writeFile(t *testing.T, dir, name string, size int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPlanAcceptsAndRejects(t *testing.T) {
	dir := t.TempDir()
	good := writeFile(t, dir, "app.log", 10)
	big := writeFile(t, dir, "huge.bin", maxFileBytes+1)
	subdir := filepath.Join(dir, "nested")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		entry string
		want  string // substring of the expected error; "" means accept
	}{
		{"regular file", good, ""},
		{"directory", subdir, "not a regular file"},
		{"missing", filepath.Join(dir, "ghost.log"), "no such file"},
		{"relative", "logs/app.log", "path must be absolute"},
		{"bare name", "app.log", "not an existing attachment and not an absolute path"},
		{"oversized", big, "exceeds the 10 MiB per-file limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set, err := Plan(nil, []string{tc.entry})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Plan(%q): %v", tc.entry, err)
				}
				if len(set.Items) != 1 || set.Items[0].Name != "app.log" || set.Items[0].Size != 10 {
					t.Fatalf("set = %+v", set)
				}
				return
			}
			if err == nil {
				t.Fatalf("Plan(%q) accepted; want error containing %q", tc.entry, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err, tc.want)
			}
		})
	}
}

func TestPlanEnforcesSetLimits(t *testing.T) {
	dir := t.TempDir()
	var entries []string
	for i := 0; i < maxAttachments+1; i++ {
		entries = append(entries, writeFile(t, dir, "f"+string(rune('a'+i))+".log", 1))
	}
	if _, err := Plan(nil, entries); err == nil ||
		!strings.Contains(err.Error(), "exceeds the limit of 10") {
		t.Fatalf("count limit not enforced: %v", err)
	}

	// The per-issue total counts retained files, not just incoming ones: a
	// retained 20 MiB row plus a new 6 MiB file is over the 25 MiB cap.
	existing := []store.AttachmentRow{{Name: "old.bin", Size: 20 << 20}}
	fresh := writeFile(t, dir, "new.bin", 6<<20)
	if _, err := Plan(existing, []string{"old.bin", fresh}); err == nil ||
		!strings.Contains(err.Error(), "exceeds the 25 MiB per-issue limit") {
		t.Fatalf("per-issue limit ignored retained bytes: %v", err)
	}
}

func TestPlanSuffixesCollidingNames(t *testing.T) {
	a, b, c := t.TempDir(), t.TempDir(), t.TempDir()
	set, err := Plan(nil, []string{
		writeFile(t, a, "app.log", 1),
		writeFile(t, b, "app.log", 1),
		writeFile(t, c, "app.log", 1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := set.Names(); len(got) != 3 ||
		got[0] != "app.log" || got[1] != "app-2.log" || got[2] != "app-3.log" {
		t.Fatalf("names = %v", got)
	}

	set, err = Plan(nil, []string{writeFile(t, a, "Makefile", 1), writeFile(t, b, "Makefile", 1)})
	if err != nil {
		t.Fatal(err)
	}
	if got := set.Names(); len(got) != 2 || got[1] != "Makefile-2" {
		t.Fatalf("extensionless names = %v", got)
	}
}

func TestPlanRetainsByName(t *testing.T) {
	existing := []store.AttachmentRow{{Name: "app.log", Size: 42, SourcePath: "/old/app.log"}}
	set, err := Plan(existing, []string{" app.log "})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Items) != 1 || !set.Items[0].Retained || set.Items[0].Size != 42 {
		t.Fatalf("retain failed: %+v", set.Items)
	}
	// A name with a separator is a path, not a retain signal.
	if _, err := Plan(existing, []string{"logs/app.log"}); err == nil ||
		!strings.Contains(err.Error(), "path must be absolute") {
		t.Fatalf("separator treated as retain: %v", err)
	}
}

func TestPlanSkipsEmptyEntries(t *testing.T) {
	set, err := Plan(nil, []string{"", "   "})
	if err != nil || len(set.Items) != 0 {
		t.Fatalf("set = %+v err = %v", set, err)
	}
}

func TestSaveCreatesIssueDirForADraft(t *testing.T) {
	src := t.TempDir()
	// The draft case: DraftIssue hands out an ID without any stage having run,
	// so <DataDir>/<id> does not exist yet.
	issueDir := filepath.Join(t.TempDir(), "GH-7")
	set, err := Plan(nil, []string{writeFile(t, src, "app.log", 5)})
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(issueDir, set); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(issueDir, dirName, "app.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 5 {
		t.Fatalf("copied %d bytes, want 5", len(stored))
	}
}

func TestSaveSkipsRetainedItems(t *testing.T) {
	issueDir := t.TempDir()
	// A retained item's bytes are already stored; Save must not try to read a
	// SourcePath that may no longer exist.
	set := Set{Items: []Item{{Name: "app.log", SourcePath: "/gone/app.log", Size: 1, Retained: true}}}
	if err := Save(issueDir, set); err != nil {
		t.Fatalf("Save re-read a retained item: %v", err)
	}
}

func TestMaterializeIsNoOpWhenDstEqualsSrc(t *testing.T) {
	issueDir := t.TempDir()
	rows := []store.AttachmentRow{{Name: "app.log", Size: 1}}
	if err := Materialize(issueDir, issueDir, rows); err != nil {
		t.Fatal(err)
	}
	// A self-copy would have created (or truncated) the file; nothing should
	// exist because Save was never called here.
	if _, err := os.Stat(filepath.Join(issueDir, dirName, "app.log")); !os.IsNotExist(err) {
		t.Fatalf("Materialize self-copied: %v", err)
	}
}

func TestMaterializeCopiesAndMarks(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, dirName), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(src, dirName), "app.log", 7)
	rows := []store.AttachmentRow{{Name: "app.log", Size: 7}}
	if err := Materialize(dst, src, rows); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, dirName, "app.log")); err != nil || len(b) != 7 {
		t.Fatalf("copy wrong: %v %d", err, len(b))
	}
	if _, err := os.Stat(filepath.Join(dst, dirName, markerName)); err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	// Still readable at the canonical path.
	if _, err := os.Stat(filepath.Join(src, dirName, "app.log")); err != nil {
		t.Fatalf("canonical copy disturbed: %v", err)
	}
	// Idempotent: a second run over a marked directory is fine.
	if err := Materialize(dst, src, rows); err != nil {
		t.Fatalf("second materialize: %v", err)
	}
}

func TestMaterializeRefusesForeignDirectory(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	foreign := filepath.Join(dst, dirName)
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, foreign, "tracked.txt", 1) // the repo's own attachments/
	err := Materialize(dst, src, []store.AttachmentRow{{Name: "app.log"}})
	if err == nil || !strings.Contains(err.Error(), "is not Guildhall's") {
		t.Fatalf("foreign attachments/ not refused: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(foreign, "tracked.txt")); len(b) != 1 {
		t.Fatal("refusal was destructive")
	}
}

func TestDeleteDroppedRemovesOnlyDroppedNames(t *testing.T) {
	issueDir := t.TempDir()
	dir := filepath.Join(issueDir, dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "keep.log", 1)
	writeFile(t, dir, "drop.log", 1)
	existing := []store.AttachmentRow{{Name: "keep.log"}, {Name: "drop.log"}}
	set := Set{Items: []Item{{Name: "keep.log", Retained: true}}}
	if err := DeleteDropped(issueDir, existing, set); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep.log")); err != nil {
		t.Fatalf("retained file deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "drop.log")); !os.IsNotExist(err) {
		t.Fatalf("dropped file survived: %v", err)
	}
}

func TestDeleteAllIsIdempotent(t *testing.T) {
	issueDir := t.TempDir()
	if err := DeleteAll(issueDir); err != nil {
		t.Fatalf("missing dir must not error: %v", err)
	}
}

func TestSectionListsNamesAndSizes(t *testing.T) {
	got := Section([]store.AttachmentRow{
		{Name: "app.log", Size: 2202010}, {Name: "shot.png", Size: 412 << 10}})
	for _, want := range []string{
		"# Attachments", "`attachments/app.log` (2.1 MiB)", "`attachments/shot.png` (412 KiB)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if Section(nil) != "" {
		t.Fatalf("empty set produced a section: %q", Section(nil))
	}
}

func TestRowsNumbersOrdinals(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	rows := Rows("GH-1", Set{Items: []Item{{Name: "a"}, {Name: "b"}}}, now)
	if len(rows) != 2 || rows[0].Ord != 0 || rows[1].Ord != 1 ||
		rows[1].IssueID != "GH-1" || !rows[1].AddedAt.Equal(now) {
		t.Fatalf("rows = %+v", rows)
	}
}
