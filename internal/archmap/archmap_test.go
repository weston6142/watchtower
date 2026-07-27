package archmap

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanCountsAndSkips(t *testing.T) {
	repo := t.TempDir()
	mk := func(p string) {
		full := filepath.Join(repo, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("internal/pay/a.go")
	mk("internal/pay/b.go")
	mk("internal/cart/c.go")
	mk("docs/readme.md")
	mk(".worktrees/GH-1/junk.go")
	mk(".git/config")
	mods, err := Scan(repo)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]int{}
	for _, m := range mods {
		byName[m.Name] = m.Files
	}
	if byName["internal/pay"] != 2 || byName["internal/cart"] != 1 || byName["docs"] != 1 {
		t.Fatalf("modules: %+v", byName)
	}
	if _, ok := byName[".worktrees/GH-1"]; ok {
		t.Fatal("worktrees not skipped")
	}
	if _, ok := byName[".git"]; ok {
		t.Fatal(".git not skipped")
	}
}
