package attach

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
