package scaffold

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/pkgs"
)

func TestMergeVerifierPromptCopiesMatch(t *testing.T) {
	root := filepath.Join("..", "..")
	paths := []string{
		filepath.Join(root, ".watchtower", "packages", "merge-verifier", "prompt.md"),
		filepath.Join(root, "internal", "scaffold", "defaults", "packages", "merge-verifier", "prompt.md"),
		filepath.Join(root, "dist", "packages", "merge-verifier", "prompt.md"),
	}
	first, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths[1:] {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(body, first) {
			t.Fatalf("merge verifier prompt %s differs from %s", path, paths[0])
		}
	}
}

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
		filepath.Join(root, ".watchtower", "config.yaml"),
		filepath.Join(root, ".watchtower", "flows", "default.yaml"),
		filepath.Join(root, ".watchtower", "packages", "executor", "package.yaml"),
		filepath.Join(root, ".watchtower", "packages", "executor", "prompt.md"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing %s: %v", p, err)
		}
	}
	cfg, err := os.ReadFile(filepath.Join(root, ".watchtower", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"runner: codex",
		"codex_bin: codex",
		"codex_model: gpt-5.6-luna",
		"codex_effort: xhigh",
	} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("generated config missing %q:\n%s", want, cfg)
		}
	}
}

func TestInitIdempotentAndNonDestructive(t *testing.T) {
	root := t.TempDir()
	if _, _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(root, ".watchtower", "config.yaml")
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

func TestDefaultWorkflowIsSequentialAndUsesSharedDecisionProtocol(t *testing.T) {
	root := t.TempDir()
	if _, _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	watchtower := filepath.Join(root, ".watchtower")
	defaultFlow, err := flow.Load(filepath.Join(watchtower, "flows", "default.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	wantStages := []string{
		"brainstorm", "spec", "plan", "execute", "correctness-review",
		"clean-code-review", "librarian", "merge-verification",
	}
	var gotStages []string
	artifactSet := map[string]bool{}
	for index, stage := range defaultFlow.Stages {
		gotStages = append(gotStages, stage.Name)
		if len(stage.Agents) != 1 || stage.Parallel {
			t.Fatalf("stage %s is not single-agent sequential: %+v", stage.Name, stage)
		}
		if stage.Gate != flow.GateAuto {
			t.Fatalf("stage %s has an extra engine gate %q", stage.Name, stage.Gate)
		}
		if index < 3 && stage.Workspace != "worktree" {
			t.Fatalf("early stage %s workspace = %q, want worktree", stage.Name, stage.Workspace)
		}
		for _, artifact := range stage.Artifacts {
			artifactSet[artifact] = true
		}
	}
	if !slices.Equal(gotStages, wantStages) {
		t.Fatalf("stages = %v, want %v", gotStages, wantStages)
	}
	for _, artifact := range []string{
		"brainstorm.md", "spec.md", "plan.md", "touchset.json",
		"merge-report.md", "merge-decision.json", "verification.json",
	} {
		if !artifactSet[artifact] {
			t.Errorf("missing artifact %s", artifact)
		}
	}

	packages, err := pkgs.LoadDir(filepath.Join(watchtower, "packages"))
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range defaultFlow.Stages {
		name := stage.Agents[0].Package
		if _, ok := packages[name]; !ok {
			t.Errorf("stage %s references missing package %s", stage.Name, name)
		}
	}
	for _, removed := range []string{"reviewer", "doc-writer"} {
		if _, ok := packages[removed]; ok {
			t.Errorf("obsolete package %s is still shipped", removed)
		}
	}
	for _, early := range []string{"brainstorm", "spec-writer", "planner"} {
		if !slices.Contains(packages[early].AllowedTools, "Bash") {
			t.Errorf("early package %s cannot inspect the repository", early)
		}
	}
	for name, pkg := range packages {
		if !slices.Contains(pkg.Includes, "decision-protocol") {
			t.Errorf("package %s does not include decision-protocol", name)
		}
		lower := strings.ToLower(pkg.Prompt)
		for _, banned := range []string{
			"guildhall_decision", "superpowers", "visual companion",
			"choose how to execute", "git add .", "git add -a", "git add --all",
		} {
			if strings.Contains(lower, banned) {
				t.Errorf("package %s contains banned text %q", name, banned)
			}
		}
	}
	protocol, err := os.ReadFile(filepath.Join(watchtower, "shared", "decision-protocol.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`"kind":"choice"`, `"kind":"freeform"`, `"allow_freeform":true`,
		"two or three meaningful options", "one question at",
	} {
		if !strings.Contains(string(protocol), required) {
			t.Errorf("decision protocol missing %q", required)
		}
	}
	librarian := strings.ToLower(packages["librarian"].Prompt)
	for _, postMerge := range []string{"after merge", "just merged", "post-merge"} {
		if strings.Contains(librarian, postMerge) {
			t.Errorf("librarian contains post-merge instruction %q", postMerge)
		}
	}
}
