package flow

import (
	"reflect"
	"strings"
	"testing"
)

func TestCanonicalStageNames(t *testing.T) {
	f, err := Load("../scaffold/defaults/flows/default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"brainstorm", "spec", "plan", "execute", "correctness-review",
		"clean-code-review", "librarian", "merge-verification",
	}

	got := f.StageNames()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stage names = %v, want %v", got, want)
	}
	got[0] = "mutated"
	if again := f.StageNames(); !reflect.DeepEqual(again, want) {
		t.Fatalf("stage names after caller mutation = %v, want %v", again, want)
	}
}

func TestLoadRequiresCapabilityProfile(t *testing.T) {
	_, err := loadBytes([]byte("name: x\nstages:\n  - name: build\n    agents: [{package: p}]\n    gate: auto\n"))
	if err == nil || !strings.Contains(err.Error(), `stage "build" capability_profile`) {
		t.Fatalf("load error = %v", err)
	}

	for _, profile := range []CapabilityProfile{
		ProfileArtifact, ProfileInspect, ProfileImplementation, ProfileReview,
		ProfileLibrarian, ProfileFinalReview, ProfileConflictResolution,
	} {
		documentation := ""
		if profile == ProfileLibrarian {
			documentation = "    documentation_paths: [docs/**]\n"
		}
		input := "name: x\nstages:\n  - name: arbitrary-name\n    agents: [{package: p}]\n    gate: auto\n    capability_profile: " + string(profile) + "\n" + documentation
		flow, loadErr := loadBytes([]byte(input))
		if loadErr != nil {
			t.Errorf("profile %q rejected: %v", profile, loadErr)
			continue
		}
		if got := flow.Stages[0].CapabilityProfile; got != profile {
			t.Errorf("profile = %q, want %q", got, profile)
		}
	}
}

func TestLoadRejectsCapabilityProfileContradictions(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"unknown", "capability_profile: omnipotent\n", "capability_profile"},
		{"documentation on implementation", "capability_profile: implementation\n    documentation_paths: [docs/**]\n", "documentation_paths"},
		{"librarian without paths", "capability_profile: librarian\n", "documentation_paths"},
		{"librarian unsafe path", "capability_profile: librarian\n    documentation_paths: [.git/**]\n", "documentation_paths"},
		{"readonly output", "capability_profile: artifact\n    workspace: readonly\n    artifacts: [result.md]\n", "readonly"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := loadBytes([]byte("name: x\nstages:\n  - name: named-stage\n    agents: [{package: p}]\n    gate: auto\n    " + test.yaml))
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "named-stage") {
				t.Fatalf("load error = %v, want stage and %q", err, test.want)
			}
		})
	}
}

func TestShippedDefaultFlowDeclaresCapabilityProfiles(t *testing.T) {
	f, err := Load("../scaffold/defaults/flows/default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]CapabilityProfile{
		"brainstorm": ProfileArtifact, "spec": ProfileArtifact, "plan": ProfileArtifact,
		"execute": ProfileImplementation, "correctness-review": ProfileReview,
		"clean-code-review": ProfileReview, "librarian": ProfileLibrarian,
		"merge-verification": ProfileFinalReview,
	}
	for _, stage := range f.Stages {
		if stage.CapabilityProfile != want[stage.Name] {
			t.Errorf("stage %q profile = %q, want %q", stage.Name, stage.CapabilityProfile, want[stage.Name])
		}
		if stage.Name == "librarian" && !reflect.DeepEqual(stage.DocumentationPaths, []string{"docs/**", "docs-draft-*"}) {
			t.Errorf("librarian documentation paths = %v", stage.DocumentationPaths)
		}
	}
}

func TestLoadValidFlow(t *testing.T) {
	f, err := Load("testdata/default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "default" || len(f.Stages) != 4 {
		t.Fatalf("bad flow: %+v", f)
	}
	rev := f.Stages[3]
	if len(rev.Agents) != 3 || !rev.Parallel || rev.Completion != "all" {
		t.Fatalf("multi-agent stage wrong: %+v", rev)
	}
	if f.Stages[0].Workspace != "none" {
		t.Fatalf("workspace default not applied: %q", f.Stages[0].Workspace)
	}
}

func TestLoadRejectsBadGate(t *testing.T) {
	if _, err := loadBytes([]byte("name: x\nstages:\n  - name: a\n    agents: [{package: p}]\n    capability_profile: inspect\n    gate: bogus\n")); err == nil {
		t.Fatal("expected error for bad gate")
	}
}

func TestLoadPlanReviewGate(t *testing.T) {
	f, err := loadBytes([]byte(`name: plan-review
stages:
  - name: plan
    agents: [{package: planner}]
    capability_profile: artifact
    gate: plan_review
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Stages[0].Gate; got != GatePlanReview {
		t.Fatalf("plan gate = %q, want %q", got, GatePlanReview)
	}
}

func TestLoadAllowsFlowWithoutMergeBarrier(t *testing.T) {
	f, err := loadBytes([]byte(`name: research
stages:
  - name: investigate
    agents: [{package: explorer}]
    capability_profile: inspect
    gate: auto
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := f.IntegrationStage(); ok {
		t.Fatal("non-integrating flow reported an integration stage")
	}
}

func TestLoadAllowsOneTerminalMergeBarrierWithRequiredArtifacts(t *testing.T) {
	f, err := loadBytes([]byte(`name: delivery
stages:
  - name: change
    agents: [{package: builder}]
    capability_profile: implementation
    gate: auto
  - name: ship-it
    agents: [{package: verifier}]
    capability_profile: final-review
    gate: auto
    merge_barrier: true
    artifacts: [merge-report.md, merge-decision.json, verification.json]
`))
	if err != nil {
		t.Fatal(err)
	}
	stage, index, ok := f.IntegrationStage()
	if !ok || index != 1 || stage.Name != "ship-it" {
		t.Fatalf("integration stage = %+v, %d, %v", stage, index, ok)
	}
}

func TestLoadRejectsMultipleOrNonTerminalMergeBarriers(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "multiple",
			yaml: `name: bad
stages:
  - name: first
    agents: [{package: verifier}]
    capability_profile: final-review
    gate: auto
    merge_barrier: true
    artifacts: [merge-report.md, merge-decision.json, verification.json]
  - name: second
    agents: [{package: verifier}]
    capability_profile: final-review
    gate: auto
    merge_barrier: true
    artifacts: [merge-report.md, merge-decision.json, verification.json]
`,
			want: `flow "bad" has more than one merge barrier`,
		},
		{
			name: "non-terminal",
			yaml: `name: bad
stages:
  - name: integrate
    agents: [{package: verifier}]
    capability_profile: final-review
    gate: auto
    merge_barrier: true
    artifacts: [merge-report.md, merge-decision.json, verification.json]
  - name: mutate-afterward
    agents: [{package: builder}]
    capability_profile: implementation
    gate: auto
`,
			want: `flow "bad" merge barrier "integrate" must be the final stage`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := loadBytes([]byte(test.yaml))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("load error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadRejectsBarrierMissingFinalizationArtifacts(t *testing.T) {
	_, err := loadBytes([]byte(`name: bad
stages:
  - name: integrate
    agents: [{package: verifier}]
    capability_profile: final-review
    gate: auto
    merge_barrier: true
    artifacts: [verification.json]
`))
	if err == nil || !strings.Contains(err.Error(), "merge-report.md, merge-decision.json") {
		t.Fatalf("load error = %v", err)
	}
}

func TestIntegratingFlowRequiresConfiguredVerificationCommand(t *testing.T) {
	f, err := loadBytes([]byte(`name: delivery
stages:
  - name: integrate
    agents: [{package: verifier}]
    capability_profile: final-review
    gate: auto
    merge_barrier: true
    artifacts: [merge-report.md, merge-decision.json, verification.json]
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.ValidateIntegration(nil); err == nil ||
		!strings.Contains(err.Error(), `flow "delivery" requires test_cmd`) {
		t.Fatalf("ValidateIntegration error = %v", err)
	}
	if err := f.ValidateIntegration([]string{"scripts/verify"}); err != nil {
		t.Fatal(err)
	}
}

func TestShippedDefaultFlowSatisfiesIntegrationContract(t *testing.T) {
	f, err := Load("../scaffold/defaults/flows/default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	stage, index, ok := f.IntegrationStage()
	if !ok || index != len(f.Stages)-1 || !stage.MergeBarrier {
		t.Fatalf("default integration stage = %+v, %d, %v", stage, index, ok)
	}
	if err := f.ValidateIntegration([]string{"scripts/verify"}); err != nil {
		t.Fatal(err)
	}
}

func TestShippedDefaultFlowRequiresArtifactAndPlanReview(t *testing.T) {
	f, err := Load("../scaffold/defaults/flows/default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]Stage, len(f.Stages))
	for _, stage := range f.Stages {
		byName[stage.Name] = stage
	}

	if got := byName["spec"]; got.Gate != GateApproveArtifact || len(got.Artifacts) != 1 || got.Artifacts[0] != "spec.md" {
		t.Fatalf("spec stage = %+v, want approve_artifact with spec.md", got)
	}
	if got := byName["plan"]; got.Gate != GatePlanReview || len(got.Artifacts) != 2 || got.Artifacts[0] != "plan.md" || got.Artifacts[1] != "touchset.json" {
		t.Fatalf("plan stage = %+v, want plan_review with plan.md and touchset.json", got)
	}

	wantGates := map[string]Gate{
		"brainstorm":         GateAuto,
		"execute":            GateAuto,
		"correctness-review": GateAuto,
		"clean-code-review":  GateAuto,
		"librarian":          GateAuto,
		"merge-verification": GateAuto,
	}
	for name, want := range wantGates {
		if got := byName[name].Gate; got != want {
			t.Errorf("%s gate = %q, want %q", name, got, want)
		}
	}
}
