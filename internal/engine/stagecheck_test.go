package engine

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/workspace"
)

func TestReplayWithoutWorkflowInputsHidesAndRestoresInputs(t *testing.T) {
	workdir := t.TempDir()
	workflow := map[string]string{
		"ISSUE.md":          "issue\n",
		"STAGE.md":          "stage\n",
		"decisions.md":      "decisions\n",
		"attachments/a.log": "attachment\n",
		"review.md":         "review\n",
	}
	for name, content := range workflow {
		path := filepath.Join(workdir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workdir, "product.txt"), []byte("product\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	commands := [][]string{{"sh", "-c", "test ! -e ISSUE.md && test ! -e STAGE.md && test ! -e decisions.md && test ! -e attachments && test ! -e review.md && test -f product.txt"}}
	paths := []string{"ISSUE.md", "STAGE.md", "decisions.md", "attachments", "review.md"}
	if err := replayWithoutWorkflowInputs(context.Background(), workdir, commands, paths); err != nil {
		t.Fatal(err)
	}
	for name, want := range workflow {
		got, err := os.ReadFile(filepath.Join(workdir, name))
		if err != nil || string(got) != want {
			t.Fatalf("restored %s = %q, err = %v; want %q", name, got, err, want)
		}
	}
	if matches, err := filepath.Glob(filepath.Join(filepath.Dir(workdir), ".watchtower-verification-*")); err != nil || len(matches) != 0 {
		t.Fatalf("verification shelves remain: %v, err = %v", matches, err)
	}
}

func TestReplayWithoutWorkflowInputsRestoresAfterFailure(t *testing.T) {
	workdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workdir, "ISSUE.md"), []byte("issue\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := replayWithoutWorkflowInputs(
		context.Background(), workdir,
		[][]string{{"sh", "-c", "test ! -e ISSUE.md; exit 17"}},
		[]string{"ISSUE.md"},
	)
	if err == nil || !strings.Contains(err.Error(), "exit status 17") {
		t.Fatalf("error = %v, want command failure", err)
	}
	got, readErr := os.ReadFile(filepath.Join(workdir, "ISSUE.md"))
	if readErr != nil || string(got) != "issue\n" {
		t.Fatalf("restored ISSUE.md = %q, err = %v", got, readErr)
	}
}

func TestChangedStageRunsCheckWithoutWorkflowInputs(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	stage := flow.Stage{
		Name: "review", Agents: []flow.AgentRef{{Package: "reviewer"}},
		Workspace: "worktree", Gate: flow.GateAuto, Completion: flow.CompletionAll,
		Artifacts: []string{"review.md"}, VerifyAfterChange: "quick",
	}
	r := &runner.FakeRunner{
		Scripts: map[string]runner.Script{"review/reviewer": {Artifacts: map[string]string{"review.md": "review\n"}}},
		OnStart: func(_, _, _, workdir string) error {
			return os.WriteFile(filepath.Join(workdir, "product.txt"), []byte("changed\n"), 0o644)
		},
	}
	e, _ := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{"checks": {Name: "checks", Stages: []flow.Stage{stage}}}
		cfg.Workspace = workspace.GitWorktree{Repo: repo}
		cfg.Checks = map[string][]string{"quick": {
			"sh", "-c", "test ! -e ISSUE.md && test ! -e STAGE.md && test ! -e decisions.md && test ! -e attachments && test ! -e review.md && test -f product.txt && printf checked > .checked",
		}}
	})
	id, err := e.CreateIssue("check changed stage", "", "checks", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	is := e.issues[id]
	is.wsPath = repo
	is.baseRef = strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD"))
	if err := e.runStageOnce(context.Background(), is, stage, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(repo, ".checked")); err != nil || string(got) != "checked" {
		t.Fatalf("check marker = %q, err = %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(repo, "review.md")); err != nil || string(got) != "review\n" {
		t.Fatalf("restored artifact = %q, err = %v", got, err)
	}
}

func TestUnchangedStageSkipsCheck(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	stage := flow.Stage{
		Name: "review", Agents: []flow.AgentRef{{Package: "reviewer"}},
		Workspace: "worktree", Gate: flow.GateAuto, Completion: flow.CompletionAll,
		VerifyAfterChange: "quick",
	}
	e, _ := newEngineCfg(t, &runner.FakeRunner{Scripts: map[string]runner.Script{"review/reviewer": {}}}, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{"checks": {Name: "checks", Stages: []flow.Stage{stage}}}
		cfg.Workspace = workspace.GitWorktree{Repo: repo}
		cfg.Checks = map[string][]string{"quick": {"sh", "-c", "printf checked > .checked"}}
	})
	id, err := e.CreateIssue("skip unchanged stage", "", "checks", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	is := e.issues[id]
	is.wsPath = repo
	is.baseRef = strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD"))
	if err := e.runStageOnce(context.Background(), is, stage, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".checked")); !os.IsNotExist(err) {
		t.Fatalf("unchanged stage ran check: %v", err)
	}
}

func TestFailedStageCheckUsesStageRetry(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	counter := filepath.Join(t.TempDir(), "check-count")
	stage := flow.Stage{
		Name: "review", Agents: []flow.AgentRef{{Package: "reviewer"}},
		Workspace: "worktree", Gate: flow.GateAuto, Completion: flow.CompletionAll,
		VerifyAfterChange: "quick", Retries: 1,
	}
	starts := 0
	r := &runner.FakeRunner{
		Scripts: map[string]runner.Script{"review/reviewer": {}},
		OnStart: func(_, _, _, workdir string) error {
			starts++
			if starts == 1 {
				return os.WriteFile(filepath.Join(workdir, "product.txt"), []byte("changed\n"), 0o644)
			}
			return nil
		},
	}
	checkScript := `count=0; test ! -f "$1" || count=$(cat "$1"); count=$((count + 1)); printf %s "$count" > "$1"; test "$count" -gt 1`
	e, _ := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{"checks": {Name: "checks", Stages: []flow.Stage{stage}}}
		cfg.Workspace = workspace.GitWorktree{Repo: repo}
		cfg.Checks = map[string][]string{"quick": {"sh", "-c", checkScript, "watchtower-check", counter}}
	})
	id, err := e.CreateIssue("retry failed check", "", "checks", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	is := e.issues[id]
	is.wsPath = repo
	is.baseRef = strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD"))
	if err := e.runStage(context.Background(), is, stage, nil); err != nil {
		t.Fatal(err)
	}
	countBody, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	count, err := strconv.Atoi(string(countBody))
	if err != nil || count != 2 || starts != 2 {
		t.Fatalf("checks = %d, starts = %d, parse err = %v; want two attempts", count, starts, err)
	}
}
