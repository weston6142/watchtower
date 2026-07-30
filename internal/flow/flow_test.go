package flow

import (
	"slices"
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

func TestShippedDefaultFlowHasApprovedStageOrder(t *testing.T) {
	f, err := Load("../scaffold/defaults/flows/default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, stage := range f.Stages {
		names = append(names, stage.Name)
		if len(stage.Agents) != 1 || stage.Parallel {
			t.Fatalf("stage is not sequential: %+v", stage)
		}
	}
	want := []string{
		"brainstorm", "spec", "plan", "execute", "correctness-review",
		"clean-code-review", "librarian", "merge-verification",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("stages = %v, want %v", names, want)
	}
}
