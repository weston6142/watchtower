package scaffold

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/decision"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/pkgs"
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

func TestDefaultWorkflowSatisfiesDeclaredContracts(t *testing.T) {
	root := t.TempDir()
	if _, _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	watchtower := filepath.Join(root, ".watchtower")
	defaultFlow, err := flow.Load(filepath.Join(watchtower, "flows", "default.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	barrier, barrierIndex, ok := defaultFlow.IntegrationStage()
	if !ok || barrierIndex != len(defaultFlow.Stages)-1 {
		t.Fatalf("default integration stage = %+v, %d, %v", barrier, barrierIndex, ok)
	}
	if err := defaultFlow.ValidateIntegration([]string{"scripts/verify"}); err != nil {
		t.Fatal(err)
	}
	packages, err := pkgs.LoadDir(filepath.Join(watchtower, "packages"))
	if err != nil {
		t.Fatal(err)
	}
	artifactSet := map[string]bool{}
	for _, stage := range defaultFlow.Stages {
		if len(stage.Agents) == 0 {
			t.Fatalf("stage %s has no agents", stage.Name)
		}
		for _, agent := range stage.Agents {
			if _, ok := packages[agent.Package]; !ok {
				t.Errorf("stage %s references missing package %s", stage.Name, agent.Package)
			}
		}
		for _, artifact := range stage.Artifacts {
			artifactSet[artifact] = true
		}
	}
	for _, artifact := range []string{
		"brainstorm.md", "spec.md", "plan.md", "touchset.json",
		"merge-report.md", "merge-decision.json", "verification.json",
	} {
		if !artifactSet[artifact] {
			t.Errorf("missing artifact %s", artifact)
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
		"directly in your assistant response text", "Never emit it through a tool",
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

func TestScaffoldDefaultIdentityMetadata(t *testing.T) {
	root := t.TempDir()
	if _, _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	generated, err := pkgs.LoadDir(filepath.Join(root, ".watchtower", "packages"))
	if err != nil {
		t.Fatal(err)
	}
	shipped, err := pkgs.LoadDir(filepath.Join("..", "..", "dist", "packages"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]decision.AgentIdentity{
		"brainstorm":           {Name: "Brainstorm", Color: "cyan", Symbol: "✦"},
		"spec-writer":          {Name: "Spec Writer", Color: "violet", Symbol: "✎"},
		"planner":              {Name: "Planner", Color: "blue", Symbol: "⌘"},
		"executor":             {Name: "Executor", Color: "green", Symbol: "⚙"},
		"clean-code-reviewer":  {Name: "Clean Code Reviewer", Color: "teal", Symbol: "◆"},
		"correctness-reviewer": {Name: "Correctness Reviewer", Color: "yellow", Symbol: "✓"},
		"conflict-resolver":    {Name: "Conflict Resolver", Color: "red", Symbol: "⚔"},
		"librarian":            {Name: "Librarian", Color: "slate", Symbol: "▤"},
		"merge-verifier":       {Name: "Merge Verifier", Color: "orange", Symbol: "⛨"},
	}
	for name, expected := range want {
		generatedPkg, generatedOK := generated[name]
		shippedPkg, shippedOK := shipped[name]
		if !generatedOK || !shippedOK {
			t.Fatalf("identity package %s generated=%v shipped=%v", name, generatedOK, shippedOK)
		}
		if generatedPkg.Identity != expected || shippedPkg.Identity != expected ||
			generatedPkg.Identity != shippedPkg.Identity {
			t.Fatalf("identity %s generated=%+v shipped=%+v expected=%+v", name, generatedPkg.Identity, shippedPkg.Identity, expected)
		}
	}
	defaultFlow, err := flow.Load(filepath.Join(root, ".watchtower", "flows", "default.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range defaultFlow.Stages {
		for _, agent := range stage.Agents {
			if _, ok := generated[agent.Package]; !ok {
				t.Fatalf("flow stage %s references package without identity: %s", stage.Name, agent.Package)
			}
		}
	}
}
