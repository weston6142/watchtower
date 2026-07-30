package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// newRepo builds the binary, picks a data dir, and initializes one repo,
// registering the cleanup that kills the daemons that repo's commands spawn.
// Darwin limits Unix socket paths, hence the short TMPDIR.
func newRepo(t *testing.T) (bin, base, repo string) {
	t.Helper()
	t.Setenv("TMPDIR", "/tmp")
	bin = buildBinary(t)
	base = t.TempDir()
	repo = initRepo(t, bin, base)
	t.Cleanup(func() { exec.Command("pkill", "-f", bin).Run() })
	return bin, base, repo
}

// runErr is like run but expects failure and returns the combined output.
func runErr(t *testing.T, bin, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("%s %v unexpectedly succeeded: %s", bin, args, out)
	}
	return string(out)
}

func TestNewWithAttachAndBody(t *testing.T) {
	bin, base, repo := newRepo(t)

	src := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(src, []byte("boom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(lastLine(run(t, bin, repo, "new", "--data", base,
		"--draft", "--title", "with attachment", "--body", "see the log",
		"--attach", src)))
	if id == "" {
		t.Fatal("new --draft returned no id")
	}
	// The engine's DataDir is <base>/repos/<repo-hash>/issues (main.go:471), so
	// the bytes land at <base>/repos/<hash>/issues/<id>/attachments/<name>. The
	// hash is not knowable here, hence the glob; filepath.Glob has no **, so
	// every other segment is literal.
	matches, err := filepath.Glob(filepath.Join(base, "repos", "*", "issues", id, "attachments", "app.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("attachment bytes not stored: %v", matches)
	}
	if b, err := os.ReadFile(matches[0]); err != nil || string(b) != "boom\n" {
		t.Fatalf("stored bytes wrong: %v %q", err, b)
	}
	if out := run(t, bin, repo, "issues", "--data", base); !strings.Contains(out, "with attachment") {
		t.Fatalf("issue missing from list: %s", out)
	}
}

func TestNewDraftWithDependenciesAppearsInBacklog(t *testing.T) {
	bin, base, repo := newRepo(t)
	parent := strings.TrimSpace(lastLine(run(t, bin, repo, "new", "--data", base,
		"--draft", "--title", "parent")))
	child := strings.TrimSpace(lastLine(run(t, bin, repo, "new", "--data", base,
		"--draft", "--title", "child", "--depends-on", parent, "--depends-on", parent)))
	if child == "" {
		t.Fatal("child draft returned no id")
	}
	out := run(t, bin, repo, "backlog", "--data", base)
	if !strings.Contains(out, "child") || !strings.Contains(out, "depends on "+parent) {
		t.Fatalf("dependency missing from backlog:\n%s", out)
	}
	if strings.Count(out, "depends on "+parent) != 1 {
		t.Fatalf("dependency was not deduplicated:\n%s", out)
	}
}

func TestResetReplacesWatchtowerTreeAndRestartsDaemon(t *testing.T) {
	bin, base, repo := newRepo(t)
	extra := filepath.Join(repo, ".watchtower", "extra.txt")
	if err := os.WriteFile(extra, []byte("remove"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(repo, ".watchtower", "config.yaml")
	if err := os.WriteFile(config, []byte("runner: fake\nslots: 99\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, bin, repo, "status", "--data", base)
	output := run(t, bin, repo, "reset", "--data", base, "--yes")
	if !strings.Contains(output, "replaced .watchtower defaults") {
		t.Fatalf("reset output:\n%s", output)
	}
	if _, err := os.Stat(extra); !os.IsNotExist(err) {
		t.Fatalf("extra file survived reset: %v", err)
	}
	body, err := os.ReadFile(config)
	if err != nil || strings.Contains(string(body), "slots: 99") {
		t.Fatalf("config not replaced: %q err=%v", body, err)
	}
	entries, err := os.ReadDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "watchtower-old") ||
			strings.Contains(entry.Name(), "watchtower-reset") {
			t.Fatalf("reset retained %s", entry.Name())
		}
	}
	if status := run(t, bin, repo, "status", "--data", base); status == "" {
		t.Fatal("restarted daemon returned empty status")
	}
}

func TestResetYesStillRefusesPendingDecision(t *testing.T) {
	bin, base, repo := newRepo(t)
	flowBody := `name: default
stages:
  - name: review
    agents: [{package: reviewer}]
    gate: approve_artifact
    artifacts: [review.md]
`
	flowPath := filepath.Join(repo, ".watchtower", "flows", "default.yaml")
	if err := os.WriteFile(flowPath, []byte(flowBody), 0o644); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(repo, ".watchtower", "keep-on-refusal.txt")
	if err := os.WriteFile(extra, []byte("still here"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, bin, repo, "new", "--data", base, "--title", "pending reset guard")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if output := run(t, bin, repo, "decisions", "--data", base); strings.Contains(output, "Approve review artifacts?") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pending decision did not appear")
		}
		time.Sleep(25 * time.Millisecond)
	}
	output := runErr(t, bin, repo, "reset", "--data", base, "--yes")
	if !strings.Contains(output, "pending decision") {
		t.Fatalf("reset refusal:\n%s", output)
	}
	if body, err := os.ReadFile(extra); err != nil || string(body) != "still here" {
		t.Fatalf("reset changed tree despite refusal: %q err=%v", body, err)
	}
}

func TestNewRefusesMissingAttachment(t *testing.T) {
	bin, base, repo := newRepo(t)

	out := runErr(t, bin, repo, "new", "--data", base, "--draft",
		"--title", "bad", "--attach", "definitely-not-here.log")
	if !strings.Contains(out, "definitely-not-here.log") || !strings.Contains(out, "no such file") {
		t.Fatalf("refusal did not name the path: %s", out)
	}
	if listed := run(t, bin, repo, "issues", "--data", base); strings.Contains(listed, "bad") {
		t.Fatalf("refused issue was created anyway: %s", listed)
	}
}

// The CLI forwarded no Body at all before this change, and no CLI output shows
// one, so the proof is ISSUE.md: the file the stage agents actually read.
func TestNewForwardsBodyIntoIssueMD(t *testing.T) {
	bin, base, repo := newRepo(t)

	id := strings.TrimSpace(lastLine(run(t, bin, repo, "new", "--data", base,
		"--title", "launched", "--body", "the body text")))
	if id == "" {
		t.Fatal("new returned no id")
	}
	// The first stage writes ISSUE.md into its workdir, which is the acquired
	// worktree for a stage with no explicit workspace: "none". Glob both homes
	// rather than depending on which branch stageWorkdir took.
	deadline := time.Now().Add(20 * time.Second)
	for {
		var found []string
		for _, pattern := range []string{
			filepath.Join(base, "repos", "*", "issues", id, "ISSUE.md"),
			filepath.Join(repo, ".worktrees", id, "ISSUE.md"),
		} {
			matches, err := filepath.Glob(pattern)
			if err != nil {
				t.Fatal(err)
			}
			found = append(found, matches...)
		}
		for _, path := range found {
			b, err := os.ReadFile(path)
			if err == nil && strings.Contains(string(b), "the body text") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("ISSUE.md never carried the body; candidates: %v", found)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
