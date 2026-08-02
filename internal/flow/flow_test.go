package flow

import (
	"strings"
	"testing"
)

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
	if _, err := loadBytes([]byte("name: x\nstages:\n  - name: a\n    agents: [{package: p}]\n    gate: bogus\n")); err == nil {
		t.Fatal("expected error for bad gate")
	}
}

func TestLoadAllowsFlowWithoutMergeBarrier(t *testing.T) {
	f, err := loadBytes([]byte(`name: research
stages:
  - name: investigate
    agents: [{package: explorer}]
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
    gate: auto
  - name: ship-it
    agents: [{package: verifier}]
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
    gate: auto
    merge_barrier: true
    artifacts: [merge-report.md, merge-decision.json, verification.json]
  - name: second
    agents: [{package: verifier}]
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
    gate: auto
    merge_barrier: true
    artifacts: [merge-report.md, merge-decision.json, verification.json]
  - name: mutate-afterward
    agents: [{package: builder}]
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
