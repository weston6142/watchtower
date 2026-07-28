package scaffold

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitCreatesTree(t *testing.T) {
	root := t.TempDir()
	created, skipped, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Fatalf("fresh init skipped files: %v", skipped)
	}
	if len(created) == 0 {
		t.Fatal("nothing created")
	}
	for _, p := range []string{
		filepath.Join(root, ".guildhall", "config.yaml"),
		filepath.Join(root, ".guildhall", "flows", "default.yaml"),
		filepath.Join(root, ".guildhall", "packages", "executor", "package.yaml"),
		filepath.Join(root, ".guildhall", "packages", "executor", "prompt.md"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing %s: %v", p, err)
		}
	}
}

func TestInitIdempotentAndNonDestructive(t *testing.T) {
	root := t.TempDir()
	if _, _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(root, ".guildhall", "config.yaml")
	os.WriteFile(custom, []byte("runner: fake\n"), 0o644)
	created, skipped, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 0 {
		t.Fatalf("second init created files: %v", created)
	}
	if len(skipped) == 0 {
		t.Fatal("second init reported no skips")
	}
	b, _ := os.ReadFile(custom)
	if string(b) != "runner: fake\n" {
		t.Fatal("init overwrote existing config.yaml")
	}
}
