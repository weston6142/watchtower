package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildBinary compiles watchtower once into a temp dir.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "watchtower")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	return bin
}

func run(t *testing.T, bin, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", bin, args, err, out)
	}
	return string(out)
}

func initRepo(t *testing.T, bin, base string) string {
	t.Helper()
	repo := t.TempDir()
	run(t, bin, repo, "init", "--data", base)
	cfg := filepath.Join(repo, ".watchtower", "config.yaml")
	if err := os.WriteFile(cfg, []byte("runner: fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestTwoReposAutoSpawnWithoutCollision(t *testing.T) {
	// Darwin limits Unix socket paths; keep the generated data path short.
	t.Setenv("TMPDIR", "/tmp")
	bin := buildBinary(t)
	base := t.TempDir()
	repoA := initRepo(t, bin, base)
	repoB := initRepo(t, bin, base)
	t.Cleanup(func() { // kill spawned daemons
		exec.Command("pkill", "-f", bin).Run()
	})

	idA := strings.TrimSpace(lastLine(run(t, bin, repoA, "new", "--data", base, "--title", "issue-a")))
	idB := strings.TrimSpace(lastLine(run(t, bin, repoB, "new", "--data", base, "--title", "issue-b")))
	if idA == "" || idB == "" {
		t.Fatal("issue creation returned empty id")
	}

	// Each repo's daemon sees only its own issue.
	outA := run(t, bin, repoA, "issues", "--data", base)
	outB := run(t, bin, repoB, "issues", "--data", base)
	if !strings.Contains(outA, "issue-a") || strings.Contains(outA, "issue-b") {
		t.Fatalf("repoA issues wrong: %s", outA)
	}
	if !strings.Contains(outB, "issue-b") || strings.Contains(outB, "issue-a") {
		t.Fatalf("repoB issues wrong: %s", outB)
	}

	// Both daemons registered and running.
	repos := run(t, bin, repoA, "repos", "--data", base)
	if strings.Count(repos, "running") != 2 {
		t.Fatalf("expected 2 running daemons:\n%s", repos)
	}
}

// lastLine returns the final non-empty line of s ("new" prints the spawn
// notice to stderr but CombinedOutput merges streams, so take the last line).
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}
