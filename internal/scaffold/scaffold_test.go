package scaffold

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/decision"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/pkgs"
	"github.com/weston6142/watchtower/internal/repocfg"
	"gopkg.in/yaml.v3"
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
		"planner_budget:",
		"warn: 24",
		"hard: 32",
	} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("generated config missing %q:\n%s", want, cfg)
		}
	}
}

func TestScaffoldCodexDefaultsAreExplicitAndTerminal(t *testing.T) {
	root := t.TempDir()
	if _, _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	generatedBytes, err := os.ReadFile(filepath.Join(root, ".watchtower", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	generated, err := repocfg.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if generated.CodexPolicy() != repocfg.CodexPolicyTerminal || generated.Codex.Fallback != nil || len(generated.Codex.Primary.FeatureOverrides) != 0 {
		t.Fatalf("generated Codex defaults = %+v, policy=%q", generated.Codex, generated.CodexPolicy())
	}
	if !strings.Contains(string(generatedBytes), "codex:\n") || !strings.Contains(string(generatedBytes), "feature_overrides: {}") {
		t.Fatalf("generated config omits explicit primary feature map:\n%s", generatedBytes)
	}
	shippedBytes, err := os.ReadFile(filepath.Join("..", "..", "dist", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(shippedBytes), "codex:\n") || !strings.Contains(string(shippedBytes), "feature_overrides: {}") || strings.Contains(string(shippedBytes), "fallback:") {
		t.Fatalf("shipped config does not document terminal Codex defaults:\n%s", shippedBytes)
	}
}

func TestScaffoldShipsPlannerBudgetDefaults(t *testing.T) {
	root := t.TempDir()
	if _, _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	generated, err := repocfg.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	shippedBytes, err := os.ReadFile(filepath.Join("..", "..", "dist", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	shippedRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(shippedRoot, ".watchtower"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shippedRoot, ".watchtower", "config.yaml"), shippedBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	shipped, err := repocfg.Load(shippedRoot)
	if err != nil {
		t.Fatal(err)
	}
	if generated.PlannerBudget != shipped.PlannerBudget {
		t.Fatalf("generated planner budget=%+v shipped=%+v", generated.PlannerBudget, shipped.PlannerBudget)
	}
}

func TestScaffoldShipsManualPlanReviewDefaults(t *testing.T) {
	root := t.TempDir()
	if _, _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	generatedCfg, err := repocfg.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := generatedCfg.PlanReviewSettings(); !got.Valid || got.AutoApproveRegular ||
		got.ID != "manual-default" || got.Version != "1" {
		t.Fatalf("generated plan review settings = %+v", got)
	}

	shippedConfigBytes, err := os.ReadFile(filepath.Join("..", "..", "dist", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var shippedCfg repocfg.Config
	if err := yaml.Unmarshal(shippedConfigBytes, &shippedCfg); err != nil {
		t.Fatal(err)
	}
	if got := shippedCfg.PlanReviewSettings(); !got.Valid || got.AutoApproveRegular ||
		got.ID != "manual-default" || got.Version != "1" {
		t.Fatalf("shipped plan review settings = %+v", got)
	}

	generatedFlow, err := flow.Load(filepath.Join(root, ".watchtower", "flows", "default.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	shippedFlow, err := flow.Load(filepath.Join("..", "..", "dist", "flows", "default.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	stageGate := func(f flow.Flow, name string) flow.Gate {
		for _, stage := range f.Stages {
			if stage.Name == name {
				return stage.Gate
			}
		}
		return ""
	}
	for label, f := range map[string]flow.Flow{"generated": generatedFlow, "shipped": shippedFlow} {
		if got := stageGate(f, "plan"); got != flow.GatePlanReview {
			t.Fatalf("%s plan gate = %q, want %q", label, got, flow.GatePlanReview)
		}
		if got := stageGate(f, "spec"); got != flow.GateApproveArtifact {
			t.Fatalf("%s spec gate = %q, want %q", label, got, flow.GateApproveArtifact)
		}
	}

	for label, path := range map[string]string{
		"generated": filepath.Join(root, ".watchtower", "packages", "planner", "prompt.md"),
		"shipped":   filepath.Join("..", "..", "dist", "packages", "planner", "prompt.md"),
	} {
		prompt, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(prompt)
		if !strings.Contains(text, "stage gate owns plan authorization") ||
			strings.Contains(text, "emit a freeform decision") {
			t.Fatalf("%s planner prompt does not delegate plan authorization:\n%s", label, text)
		}
	}
}

func TestScaffoldPlannerPromptContractAndParity(t *testing.T) {
	root := t.TempDir()
	if _, _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join("defaults", "packages", "planner", "prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	shipped, err := os.ReadFile(filepath.Join("..", "..", "dist", "packages", "planner", "prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	generated, err := os.ReadFile(filepath.Join(root, ".watchtower", "packages", "planner", "prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(source) != string(shipped) || string(source) != string(generated) {
		t.Fatal("planner prompt source, shipped, and generated copies differ")
	}
	text := string(source)
	compact := strings.Join(strings.Fields(text), " ")
	for _, required := range []string{
		"The engine initializes plan.md and touchset.json before your turn",
		"goal, architecture, technology-stack, execution-contract, file-structure",
		"task-NNNN",
		"verification",
		"one JSON request containing the full manifest",
		"MaxOperationBytes",
		"65,536",
		"request file or send it on standard input",
		"must never be a command-line argument",
		"generated patch",
		"one-shot replacement",
		"section-validated",
		"Read existing anchors on retry",
		"Never rewrite an accepted section",
		"are the only durable outputs",
		"do not emit another plan-approval decision",
	} {
		if !strings.Contains(compact, required) {
			t.Errorf("planner prompt missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"write the complete plan.md and touchset.json in one",
		"replace the entire plan.md",
		"one-shot patch for plan.md",
	} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(forbidden)) {
			t.Errorf("planner prompt still contains one-shot instruction %q", forbidden)
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
	planner := packages["planner"].Prompt
	for _, required := range []string{
		"stage gate owns plan authorization",
		"do not emit a second `watchtower_decision` for plan approval.",
	} {
		if !strings.Contains(planner, required) {
			t.Errorf("planner prompt missing %q", required)
		}
	}
	if strings.Contains(planner, "emit a freeform decision") || strings.Contains(planner, "Approve plan.md as written.") {
		t.Error("planner prompt still emits a duplicate plan approval decision")
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
	byName := map[string]flow.Stage{}
	for _, stage := range defaultFlow.Stages {
		byName[stage.Name] = stage
	}
	if got := byName["spec"]; got.Gate != flow.GateApproveArtifact || len(got.Artifacts) != 1 || got.Artifacts[0] != "spec.md" {
		t.Fatalf("generated spec stage = %+v, want approve_artifact with spec.md", got)
	}
	if got := byName["plan"]; got.Gate != flow.GatePlanReview || len(got.Artifacts) != 2 || got.Artifacts[0] != "plan.md" || got.Artifacts[1] != "touchset.json" {
		t.Fatalf("generated plan stage = %+v, want plan_review with plan.md and touchset.json", got)
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
		`"proof":[{"claim":`, `"cite":`,
		"two or three meaningful options", "one question at",
		"one concrete consequence per option",
		"agent-authored choices",
		"engine-owned review and token-budget decisions",
		"require an option and do not accept freeform feedback",
		"Watchtower derives the operator action",
		"Watchtower derives what happens after the answer",
		"directly in your assistant response text", "Never emit it through a tool",
	} {
		if !strings.Contains(string(protocol), required) {
			t.Errorf("decision protocol missing %q", required)
		}
	}
	sourceProtocol, err := os.ReadFile(filepath.Join("defaults", "shared", "decision-protocol.md"))
	if err != nil {
		t.Fatal(err)
	}
	shippedProtocol, err := os.ReadFile(filepath.Join("..", "..", "dist", "shared", "decision-protocol.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sourceProtocol, shippedProtocol) {
		t.Fatal("embedded and shipped decision protocols differ")
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
