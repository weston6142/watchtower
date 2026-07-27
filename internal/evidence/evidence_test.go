package evidence

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func TestCollectBundlesDiff(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "config", "user.email", "t@t")
	git(t, repo, "config", "user.name", "t")
	if err := os.MkdirAll(filepath.Join(repo, "payments"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "payments", "a.go"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "base")
	// changes: big edit in payments, small edit at root
	if err := os.WriteFile(filepath.Join(repo, "payments", "a.go"), []byte("one\ntwo\nthree\nfour\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "work")

	out := t.TempDir()
	b, err := Collect(repo, "HEAD~1", out)
	if err != nil {
		t.Fatal(err)
	}
	if b.Added != 4 || len(b.Files) != 2 || b.Biggest != "payments/a.go" {
		t.Fatalf("bundle: %+v", b)
	}
	if b.AreaWeight["payments"] != 3 || b.AreaWeight["go.mod"] != 1 {
		t.Fatalf("weights: %v", b.AreaWeight)
	}
	for _, f := range []string{"evidence.json", "diff.patch"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Fatalf("%s missing", f)
		}
	}
}
