package flow

import "testing"

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
