package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/marshal"
)

type scriptedGate struct {
	Started   int
	denyAfter int
}

func (g *scriptedGate) Admit(_ context.Context, _ ToolCall) (ToolDecision, error) {
	if g.denyAfter > 0 && g.Started >= g.denyAfter {
		return ToolDecision{}, nil
	}
	g.Started++
	return ToolDecision{Allowed: true, LeaseID: "lease"}, nil
}

func (g *scriptedGate) Complete(_ context.Context, _ ToolDecision, _ *int64, _ error) error {
	return nil
}

func TestPlannerRunnerStopsAfterDeniedToolAndKeepsSynthesisResult(t *testing.T) {
	fr := &FakeRunner{Scripts: map[string]Script{
		"plan/planner": {
			Tools: []ToolCall{
				{SourceID: "ISSUE.md", Fingerprint: "v1", Reservation: 5},
				{SourceID: "internal/engine/engine.go", Fingerprint: "v1", Reservation: 5},
			},
			Artifacts: map[string]string{"plan.md": "bounded plan", "touchset.json": `{"globs":[]}`},
			Tokens:    7, TokensKnown: true,
		},
	}}
	g := &scriptedGate{denyAfter: 1}
	result := <-fr.RunPlanner(context.Background(), "GH-39", "plan", "planner", t.TempDir(), make(chan Ask), g)
	if result.Err != nil || len(result.Artifacts) != 2 || g.Started != 1 {
		t.Fatalf("result=%+v gate=%+v", result, g)
	}
}

func TestFakeRunnerPropagatesAskFailure(t *testing.T) {
	r := &FakeRunner{Scripts: map[string]Script{
		"ask/agent": {Asks: []levers.Decision{{Question: "Proceed?", Options: []string{"yes"}, Recommended: 0}}},
	}}
	asks := make(chan Ask)
	done := r.Run(context.Background(), "GH-1", "ask", "agent", t.TempDir(), asks)
	a := <-asks
	want := errors.New("invalid decision context: agent_color")
	a.Error <- want
	result := <-done
	if result.Err == nil || !strings.Contains(result.Err.Error(), want.Error()) {
		t.Fatalf("result error = %v, want %v", result.Err, want)
	}
}

func TestFakeFinalReceiptsPassProductionLoaders(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "develop"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-qm", "base"},
	} {
		if output, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	headOutput, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(string(headOutput))
	if err := os.WriteFile(filepath.Join(dir, "STAGE.md"),
		[]byte("# Stage brief\n\n- Base commit: "+head+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"merge-decision.json", "verification.json"} {
		body, err := generatedFakeArtifact(name, dir)
		if err != nil {
			t.Fatalf("generate %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	decision, err := marshal.LoadMergeDecision(filepath.Join(dir, "merge-decision.json"))
	if err != nil || decision.BranchCommit != head || decision.BaseCommit != head {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	verification, err := marshal.LoadVerification(filepath.Join(dir, "verification.json"))
	if err != nil || verification.BranchSHA != head || verification.BaseSHA != head {
		t.Fatalf("verification=%+v err=%v", verification, err)
	}
}

func TestFakeRunnerAsksThenProduces(t *testing.T) {
	dir := t.TempDir()
	var got levers.Response
	fr := &FakeRunner{Scripts: map[string]Script{
		"spec/spec-writer": {
			Asks:      []levers.Decision{{Question: "REST or GraphQL?", Options: []string{"REST", "GraphQL"}, Recommended: 0, Importance: 0.6}},
			Artifacts: map[string]string{"spec.md": ""},
			Tokens:    42,
		},
	}, OnResponse: func(_ string, _ string, response levers.Response) {
		got = response
	}}
	asks := make(chan Ask, 1)
	done := fr.Run(context.Background(), "GH-1", "spec", "spec-writer", dir, asks)

	a := <-asks
	if a.Decision.Question != "REST or GraphQL?" {
		t.Fatalf("wrong ask: %+v", a.Decision)
	}
	a.Reply <- levers.ChoiceResponse(0)

	res := <-done
	if res.Err != nil || res.Tokens != 42 {
		t.Fatalf("bad result: %+v", res)
	}
	p := res.Artifacts["spec.md"]
	if p != filepath.Join(dir, "spec.md") {
		t.Fatalf("artifact path wrong: %q", p)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("artifact not written: %v", err)
	}
	if got.Kind != levers.DecisionChoice || got.Option == nil || *got.Option != 0 {
		t.Fatalf("response = %#v", got)
	}
}
