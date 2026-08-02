package main_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/engine"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/proto"
	"github.com/weston6142/watchtower/internal/repocfg"
	"github.com/weston6142/watchtower/internal/store"
)

func TestBacklogClaimReleaseJSON(t *testing.T) {
	bin, base, repo := newRepo(t)
	run(t, "git", repo, "init", "-q", "-b", "develop")
	run(t, "git", repo, "config", "user.email", "test@example.com")
	run(t, "git", repo, "config", "user.name", "Test")
	run(t, "git", repo, "add", "-A")
	run(t, "git", repo, "commit", "-qm", "base")
	id := strings.TrimSpace(lastLine(run(t, bin, repo, "new", "--data", base,
		"--draft", "--title", "explore", "--body", "full body")))

	var listed struct {
		Backlog []proto.BacklogItem `json:"backlog"`
		Claims  []engine.Claim      `json:"claims"`
	}
	if out := run(t, bin, repo, "backlog", "--data", base, "--json"); json.Unmarshal([]byte(out), &listed) != nil {
		t.Fatalf("backlog --json = %q", out)
	}
	if len(listed.Backlog) != 1 || listed.Backlog[0].Issue.ID != id {
		t.Fatalf("backlog JSON = %+v", listed)
	}
	var claim engine.Claim
	if out := run(t, bin, repo, "claim", "--data", base, id, "--json"); json.Unmarshal([]byte(out), &claim) != nil {
		t.Fatalf("claim --json = %q", out)
	}
	if claim.IssueID != id || claim.Branch != "issue/"+id {
		t.Fatalf("claim JSON = %+v", claim)
	}
	run(t, bin, repo, "release", "--data", base, id, "--json")
}

func TestFinishClaimMergesPushesMarksDoneAndCleansWorkspace(t *testing.T) {
	t.Setenv("TMPDIR", "/tmp")
	bin := buildBinary(t)
	base, err := os.MkdirTemp("/tmp", "wt-finish-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopDaemons(base)
		_ = os.RemoveAll(base)
	})
	repo := initRepo(t, bin, base)
	run(t, "git", repo, "init", "-q", "-b", "develop")
	run(t, "git", repo, "config", "user.email", "test@example.com")
	run(t, "git", repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, ".watchtower", "config.yaml"),
		[]byte("runner: fake\ntest_cmd: true\npull: false\npush: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", repo, "add", "-A")
	run(t, "git", repo, "commit", "-qm", "base")
	remote := filepath.Join(t.TempDir(), "origin.git")
	run(t, "git", repo, "init", "--bare", "-q", remote)
	run(t, "git", repo, "remote", "add", "origin", remote)
	run(t, "git", repo, "push", "-qu", "origin", "develop")
	id := strings.TrimSpace(lastLine(run(t, bin, repo, "new", "--data", base,
		"--draft", "--title", "explore")))
	var claim engine.Claim
	if out := run(t, bin, repo, "claim", "--data", base, id, "--json"); json.Unmarshal([]byte(out), &claim) != nil {
		t.Fatalf("claim --json = %q", out)
	}
	if err := os.WriteFile(filepath.Join(claim.Worktree, "result.txt"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", claim.Worktree, "add", "result.txt")
	run(t, "git", claim.Worktree, "commit", "-qm", "result")
	finish := exec.Command(bin, "finish", "--data", base, id, "--json")
	finish.Dir = claim.Worktree
	if out, err := finish.CombinedOutput(); err != nil {
		resolved, resolveErr := repocfg.FindRepo(claim.Worktree)
		logs, _ := filepath.Glob(filepath.Join(base, "repos", "*", "daemon.log"))
		var diagnostics strings.Builder
		for _, path := range logs {
			body, _ := os.ReadFile(path)
			fmt.Fprintf(&diagnostics, "\n%s:\n%s", path, body)
		}
		t.Fatalf("finish: %v: %s\nrepo=%q id=%s claim=%+v resolved=%q resolved_id=%s resolve_err=%v%s",
			err, out, repo, repocfg.RepoID(repo), claim, resolved, repocfg.RepoID(resolved), resolveErr,
			diagnostics.String())
	}

	deadline := time.Now().Add(8 * time.Second)
	for {
		var issues []store.IssueRow
		out := run(t, bin, repo, "issues", "--data", base, "--json")
		if err := json.Unmarshal([]byte(out), &issues); err != nil {
			t.Fatalf("issues --json = %q: %v", out, err)
		}
		state := ""
		for _, issue := range issues {
			if issue.ID == id {
				state = issue.State
			}
		}
		if state == "done" {
			break
		}
		if time.Now().After(deadline) {
			dbPath := filepath.Join(repocfg.RepoDataDir(base, repo), "watchtower.db")
			st, openErr := store.Open(dbPath)
			var integration store.IssueIntegration
			var integrationOK bool
			var integrationErr error
			if openErr == nil {
				integration, integrationOK, integrationErr = st.IssueIntegration(id)
				_ = st.Close()
			}
			logBody, _ := os.ReadFile(filepath.Join(repocfg.RepoDataDir(base, repo), "daemon.log"))
			t.Fatalf("issue %s did not finish; state=%s integration=%+v ok=%v open_err=%v integration_err=%v\n%s",
				id, state, integration, integrationOK, openErr, integrationErr, logBody)
		}
		time.Sleep(20 * time.Millisecond)
	}
	local := strings.TrimSpace(run(t, "git", repo, "rev-parse", "develop"))
	remoteHead := strings.TrimSpace(run(t, "git", remote, "rev-parse", "develop"))
	if local != remoteHead {
		cfg, cfgErr := repocfg.Load(repo)
		st, openErr := store.Open(filepath.Join(repocfg.RepoDataDir(base, repo), "watchtower.db"))
		var integration store.IssueIntegration
		var integrationOK bool
		var integrationErr error
		if openErr == nil {
			integration, integrationOK, integrationErr = st.IssueIntegration(id)
			_ = st.Close()
		}
		remoteURL := strings.TrimSpace(run(t, "git", repo, "remote", "get-url", "origin"))
		t.Fatalf("develop=%s origin/develop=%s remote=%q cfg=%+v cfg_err=%v integration=%+v ok=%v open_err=%v integration_err=%v",
			local, remoteHead, remoteURL, cfg, cfgErr, integration, integrationOK, openErr, integrationErr)
	}
	if _, err := os.Stat(claim.Worktree); !os.IsNotExist(err) {
		t.Fatalf("claimed worktree still exists: %v", err)
	}
}

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

func stopDaemons(base string) {
	pidFiles, _ := filepath.Glob(filepath.Join(base, "repos", "*", "daemon.pid"))
	var pids []int
	for _, path := range pidFiles {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
		if err != nil {
			continue
		}
		pids = append(pids, pid)
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		live := false
		for _, pid := range pids {
			if syscall.Kill(pid, 0) == nil {
				live = true
				break
			}
		}
		if !live {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func initRepo(t *testing.T, bin, base string) string {
	t.Helper()
	repo := t.TempDir()
	run(t, bin, repo, "init", "--data", base)
	cfg := filepath.Join(repo, ".watchtower", "config.yaml")
	if err := os.WriteFile(cfg, []byte("runner: fake\ntest_cmd: true\n"), 0o644); err != nil {
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
	t.Cleanup(func() { stopDaemons(base) })

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
	t.Cleanup(func() { stopDaemons(base) })
	return bin, base, repo
}

func TestDefaultCodexRunnerCompletesIssue(t *testing.T) {
	t.Setenv("TMPDIR", "/tmp")
	bin := buildBinary(t)
	base := t.TempDir()
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
		{"commit", "--allow-empty", "-qm", "base"},
	} {
		if output, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	run(t, bin, repo, "init", "--data", base)
	t.Cleanup(func() { stopDaemons(base) })

	stub := filepath.Join(t.TempDir(), "codex-stub")
	if err := os.WriteFile(stub, []byte(`#!/bin/sh
set -eu
printf '%s\n' '{"type":"thread.started","thread_id":"thr-daemon"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"stub complete"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":5}}'
`), 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(repo, ".watchtower", "config.yaml")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "runner: codex") {
		t.Fatalf("fresh config is not Codex-first:\n%s", config)
	}
	configured := strings.Replace(string(config), "codex_bin: codex", "codex_bin: "+stub, 1)
	configured = strings.Replace(configured, "pull: true", "pull: false", 1)
	if err := os.WriteFile(configPath, []byte(configured), 0o644); err != nil {
		t.Fatal(err)
	}
	flowBody := `name: default
stages:
  - name: execute
    agents: [{package: executor}]
    gate: auto
    workspace: worktree
`
	if err := os.WriteFile(filepath.Join(repo, ".watchtower", "flows", "default.yaml"), []byte(flowBody), 0o644); err != nil {
		t.Fatal(err)
	}

	issueID := strings.TrimSpace(lastLine(run(t, bin, repo, "new", "--data", base, "--title", "codex stub")))
	deadline := time.Now().Add(5 * time.Second)
	for {
		issues := run(t, bin, repo, "issues", "--data", base)
		if strings.Contains(issues, issueID+"  done") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Codex workflow did not finish:\n%s", issues)
		}
		time.Sleep(25 * time.Millisecond)
	}

	sockets, err := filepath.Glob(filepath.Join(base, "repos", "*", "watchtower.sock"))
	if err != nil || len(sockets) != 1 {
		t.Fatalf("sockets = %v err %v", sockets, err)
	}
	client, err := proto.Dial(sockets[0])
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	response, err := client.Do(proto.Command{Op: "setup_outline"})
	if err != nil || !response.OK || response.Setup == nil {
		t.Fatalf("setup = %+v err %v", response, err)
	}
	repoSetup := response.Setup.Repo
	if repoSetup.Runner != "codex" || repoSetup.CodexBin != stub ||
		repoSetup.CodexModel != "gpt-5.6-luna" || repoSetup.CodexEffort != "xhigh" {
		t.Fatalf("repo setup = %+v", repoSetup)
	}

	databases, err := filepath.Glob(filepath.Join(base, "repos", "*", "watchtower.db"))
	if err != nil || len(databases) != 1 {
		t.Fatalf("database paths = %v err %v", databases, err)
	}
	st, err := store.Open(databases[0])
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runs, err := st.StageRuns(issueID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("stage runs = %+v err %v", runs, err)
	}
	if runs[0].SessionID != "thr-daemon" || runs[0].Tokens != 15 || runs[0].Status != "succeeded" {
		t.Fatalf("stage run = %+v", runs[0])
	}
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
	if err := os.WriteFile(config, []byte("runner: fake\ntest_cmd: true\nslots: 99\n"), 0o644); err != nil {
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
	if err != nil || strings.Contains(string(body), "slots: 99") ||
		!strings.Contains(string(body), `test_cmd: "true"`) {
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
	fresh := t.TempDir()
	run(t, bin, fresh, "init", "--data", base)
	if output, err := exec.Command(
		"diff", "-ru", "-x", "config.yaml",
		filepath.Join(fresh, ".watchtower"), filepath.Join(repo, ".watchtower"),
	).CombinedOutput(); err != nil {
		t.Fatalf("reset differs from fresh init: %v\n%s", err, output)
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

func TestDependencyWorkflowUsesIsolatedSessionsAndLandedBase(t *testing.T) {
	t.Setenv("TMPDIR", "/tmp")
	bin := buildBinary(t)
	base, err := os.MkdirTemp("/tmp", "wt-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
		{"commit", "--allow-empty", "-qm", "base"},
	} {
		if output, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	run(t, bin, repo, "init", "--data", base)
	if err := os.WriteFile(
		filepath.Join(repo, ".watchtower", "config.yaml"),
		[]byte("runner: fake\ntest_cmd: true\npull: false\npush: false\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	configuredFlow, err := flow.Load(filepath.Join(repo, ".watchtower", "flows", "default.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	expectedRuns := 0
	for _, stage := range configuredFlow.Stages {
		expectedRuns += len(stage.Agents)
	}
	t.Cleanup(func() { stopDaemons(base) })
	parent := strings.TrimSpace(lastLine(run(t, bin, repo, "new", "--data", base,
		"--draft", "--title", "parent")))
	child := strings.TrimSpace(lastLine(run(t, bin, repo, "new", "--data", base,
		"--draft", "--title", "child", "--depends-on", parent)))
	run(t, bin, repo, "launch", "--data", base, child)
	run(t, bin, repo, "launch", "--data", base, parent)
	deadline := time.Now().Add(15 * time.Second)
	for {
		issues := run(t, bin, repo, "issues", "--data", base)
		if strings.Contains(issues, parent+"  done") && strings.Contains(issues, child+"  done") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("workflow did not finish:\n%s\n%s", issues,
				run(t, bin, repo, "tail", "--data", base))
		}
		time.Sleep(25 * time.Millisecond)
	}
	if tail := run(t, bin, repo, "tail", "--data", base); !strings.Contains(tail, child+" issue_waiting_dependencies") {
		t.Fatalf("dependent never waited:\n%s", tail)
	}
	databases, err := filepath.Glob(filepath.Join(base, "repos", "*", "watchtower.db"))
	if err != nil || len(databases) != 1 {
		t.Fatalf("database paths = %v err %v", databases, err)
	}
	st, err := store.Open(databases[0])
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, issueID := range []string{parent, child} {
		runs, err := st.StageRuns(issueID)
		if err != nil || len(runs) != expectedRuns {
			t.Fatalf("%s stage runs = %+v err %v", issueID, runs, err)
		}
		sessions := map[string]bool{}
		for _, stageRun := range runs {
			sessions[stageRun.SessionID] = true
		}
		if len(sessions) != expectedRuns {
			t.Fatalf("%s sessions not stage-isolated: %+v", issueID, runs)
		}
		if output, err := exec.Command(
			"git", "-C", repo, "branch", "--list", "issue/"+issueID,
		).CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != "" {
			t.Fatalf("%s branch remains: %q err %v", issueID, output, err)
		}
		if body, err := os.ReadFile(filepath.Join(repo, "watchtower-fake", issueID+".txt")); err != nil || strings.TrimSpace(string(body)) != issueID {
			t.Fatalf("%s landed file = %q err %v", issueID, body, err)
		}
	}
	parentIntegration, ok, err := st.IssueIntegration(parent)
	if err != nil || !ok {
		t.Fatalf("parent integration = %+v ok %v err %v", parentIntegration, ok, err)
	}
	childCheckpoints, err := st.StageCheckpoints(child)
	if err != nil || len(childCheckpoints) == 0 ||
		childCheckpoints[0].StartCommit != parentIntegration.LandedSHA {
		t.Fatalf("child did not start from parent landing: checkpoints=%+v parent=%+v err=%v",
			childCheckpoints, parentIntegration, err)
	}
}

func TestFakeRunnerChangesFirstMutableStageRegardlessOfName(t *testing.T) {
	t.Setenv("TMPDIR", "/tmp")
	bin := buildBinary(t)
	base, err := os.MkdirTemp("/tmp", "wt-fake-name-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
		{"commit", "--allow-empty", "-qm", "base"},
	} {
		run(t, "git", repo, args...)
	}
	run(t, bin, repo, "init", "--data", base)
	t.Cleanup(func() { stopDaemons(base) })
	if err := os.WriteFile(filepath.Join(repo, ".watchtower", "config.yaml"), []byte(
		"runner: fake\ntest_cmd: true\npull: false\npush: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	flowBody := `name: synthetic
stages:
  - name: shape-change
    agents: [{package: executor}]
    workspace: worktree
    gate: auto
  - name: ship-safely
    agents: [{package: merge-verifier}]
    workspace: worktree
    gate: auto
    merge_barrier: true
    artifacts: [merge-report.md, merge-decision.json, verification.json]
`
	if err := os.WriteFile(filepath.Join(repo, ".watchtower", "flows", "synthetic.yaml"), []byte(flowBody), 0o644); err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(lastLine(run(t, bin, repo, "new", "--data", base,
		"--flow", "synthetic", "--title", "custom fake flow")))
	deadline := time.Now().Add(10 * time.Second)
	for {
		issues := run(t, bin, repo, "issues", "--data", base)
		if strings.Contains(issues, id+"  done") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("custom fake flow did not finish:\n%s", issues)
		}
		time.Sleep(25 * time.Millisecond)
	}
	body, err := os.ReadFile(filepath.Join(repo, "watchtower-fake", id+".txt"))
	if err != nil || strings.TrimSpace(string(body)) != id {
		t.Fatalf("landed fake change = %q err %v", body, err)
	}
}

func TestIntegratingFlowRefusesMissingVerificationCommand(t *testing.T) {
	t.Setenv("TMPDIR", "/tmp")
	bin := buildBinary(t)
	base, err := os.MkdirTemp("/tmp", "wt-missing-gate-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopDaemons(base)
		_ = os.RemoveAll(base)
	})
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
		{"commit", "--allow-empty", "-qm", "base"},
	} {
		run(t, "git", repo, args...)
	}
	run(t, bin, repo, "init", "--data", base)
	out := runErr(t, bin, repo, "status", "--data", base)
	if !strings.Contains(out, `flow "default" requires test_cmd`) {
		t.Fatalf("status error:\n%s", out)
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
