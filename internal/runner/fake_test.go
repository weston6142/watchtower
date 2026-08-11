package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/plannerartifact"
)

func TestRunnerRejectsMismatchedContractAndPlan(t *testing.T) {
	started := false
	r := &FakeRunner{
		Scripts: map[string]Script{"execute/executor": {}},
		OnStart: func(_, _, _, _ string) error { started = true; return nil },
	}
	contract := capability.CompiledContract{
		ContractID: strings.Repeat("a", 64), AuthorityDigest: strings.Repeat("b", 64),
		Contract: capability.Contract{Version: capability.ContractVersion, IssueID: "GH-68", Stage: "execute", AttemptID: "checkpoint-1"},
	}
	plan, err := r.Preflight(context.Background(), PreflightRequest{
		IssueID: "GH-68", Stage: "execute", Agent: "executor", Workdir: t.TempDir(), Contract: contract,
	})
	if err != nil {
		t.Fatal(err)
	}
	changed := contract
	changed.ContractID = strings.Repeat("c", 64)
	result := <-r.Run(context.Background(), StageRequest{
		IssueID: "GH-68", Stage: "execute", Agent: "executor", Workdir: t.TempDir(),
		Contract: changed, Plan: plan,
	}, make(chan Ask))
	if result.Err == nil || started {
		t.Fatalf("mismatched request result=%+v started=%t", result, started)
	}
}

func TestFakeRunnerScriptsPermittedAndDeniedOperations(t *testing.T) {
	workdir := t.TempDir()
	r := &FakeRunner{Scripts: map[string]Script{
		"execute/executor": {OperationAttempts: []OperationAttempt{
			{Operation: capability.OpWorkspaceMutate, Mutation: capability.MutationCreate, Path: "allowed.txt", Content: "allowed\n"},
			{Operation: capability.OpWorkspaceMutate, Mutation: capability.MutationCreate, Path: "outside.txt", Content: "denied\n"},
		}},
	}}
	contract := capability.CompiledContract{
		ContractID: strings.Repeat("d", 64), AuthorityDigest: strings.Repeat("e", 64),
		Contract: capability.Contract{
			Version: capability.ContractVersion, IssueID: "GH-68", Stage: "execute", AttemptID: "checkpoint-1", WorkspaceRoot: workdir,
			Operations: []capability.OperationClass{capability.OpWorkspaceMutate},
			Writes:     []capability.PathGrant{{Path: "allowed.txt", Mutations: []capability.MutationClass{capability.MutationCreate}}},
		},
	}
	plan, err := r.Preflight(context.Background(), PreflightRequest{IssueID: "GH-68", Stage: "execute", Agent: "executor", Workdir: workdir, Contract: contract})
	if err != nil {
		t.Fatal(err)
	}
	result := <-r.Run(context.Background(), StageRequest{
		IssueID: "GH-68", Stage: "execute", Agent: "executor", Workdir: workdir, Contract: contract, Plan: plan,
	}, make(chan Ask))
	if result.Err == nil || len(result.RuntimeAudit) != 2 || result.RuntimeAudit[1].Reason != capability.ReasonRuntimeDenied {
		t.Fatalf("result=%+v", result)
	}
	if body, err := os.ReadFile(filepath.Join(workdir, "allowed.txt")); err != nil || string(body) != "allowed\n" {
		t.Fatalf("permitted effect body=%q err=%v", body, err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "outside.txt")); !os.IsNotExist(err) {
		t.Fatalf("denied operation had a side effect: %v", err)
	}
}

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
	workdir := t.TempDir()
	result := <-fr.RunPlanner(context.Background(), fakeStageRequest(t, fr, "GH-39", "plan", "planner", workdir), make(chan Ask), g)
	if result.Err != nil || len(result.Artifacts) != 2 || g.Started != 1 {
		t.Fatalf("result=%+v gate=%+v", result, g)
	}
}

func TestManagedEnvironmentOverlayReachesFakeRunner(t *testing.T) {
	var observed []string
	fr := &FakeRunner{
		Scripts: map[string]Script{"run/agent": {}},
		OnEnvironment: func(_, _, _, _ string, env []string) {
			observed = append([]string(nil), env...)
		},
	}
	ctx := WithManagedEnvironment(context.Background(), []string{"GOCACHE=lease-cache", "GOMODCACHE=lease-mod"})
	workdir := t.TempDir()
	if result := <-fr.Run(ctx, fakeStageRequest(t, fr, "GH-48", "run", "agent", workdir), make(chan Ask)); result.Err != nil {
		t.Fatal(result.Err)
	}
	if strings.Join(observed, "|") != "GOCACHE=lease-cache|GOMODCACHE=lease-mod" {
		t.Fatalf("fake runner observed %v, want lease overlay", observed)
	}
}

func TestFakePlannerAppliesSectionRequestsAndRetriesPendingKey(t *testing.T) {
	dir := t.TempDir()
	registry := &runnerAuthorityRegistry{}
	authority, err := plannerartifact.CreateOrLoad(registry, plannerartifact.Binding{
		IssueID: "GH-40", Stage: "plan", Attempt: 1, Worktree: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	manifest := fakePlannerManifest()
	requests := make([]plannerartifact.WriteRequest, 0, len(manifest.Sections))
	for _, entry := range manifest.Sections {
		requests = append(requests, plannerartifact.WriteRequest{Manifest: manifest, Key: entry.Key, Markdown: "section " + entry.Key, Globs: entry.Globs})
	}
	fr := &FakeRunner{Scripts: map[string]Script{
		"plan/planner": {PlannerRequests: requests, PlannerFailureAt: 1, PlannerFailure: errors.New("stop at architecture")},
	}}
	ctx := WithPlannerArtifactAuthority(context.Background(), authority)
	request := fakeStageRequest(t, fr, "GH-40", "plan", "planner", dir)
	first := <-fr.RunPlanner(ctx, request, make(chan Ask), &scriptedGate{})
	if first.Err == nil || !strings.Contains(first.Err.Error(), "stop at architecture") {
		t.Fatalf("first result = %+v", first)
	}
	fr.Scripts["plan/planner"] = Script{PlannerRequests: requests}
	second := <-fr.RunPlanner(ctx, request, make(chan Ask), &scriptedGate{})
	if second.Err != nil {
		t.Fatal(second.Err)
	}
	plan, err := os.ReadFile(filepath.Join(dir, "plan.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range requests {
		if got := strings.Count(string(plan), "<!-- watchtower-section: key="+request.Key+" -->"); got != 1 {
			t.Fatalf("section %s count = %d", request.Key, got)
		}
	}
	if len(second.Artifacts) != 2 || second.Artifacts["plan.md"] != filepath.Join(dir, "plan.md") ||
		second.Artifacts["touchset.json"] != filepath.Join(dir, "touchset.json") {
		t.Fatalf("result artifacts = %+v", second.Artifacts)
	}
}

func fakePlannerManifest() plannerartifact.Manifest {
	return plannerartifact.Manifest{Sections: []plannerartifact.ManifestEntry{
		{Key: "goal", Globs: []string{"internal/gh40/goal/**"}},
		{Key: "architecture", Globs: []string{"internal/gh40/architecture/**"}},
		{Key: "technology-stack", Globs: []string{"internal/gh40/technology/**"}},
		{Key: "execution-contract", Globs: []string{"internal/gh40/contract/**"}},
		{Key: "file-structure", Globs: []string{"internal/gh40/files/**"}},
		{Key: "task-0001", Globs: []string{"internal/gh40/task-0001/**"}},
		{Key: "verification", Globs: []string{"internal/gh40/verification/**"}},
	}}
}

func TestFakeRunnerPropagatesAskFailure(t *testing.T) {
	r := &FakeRunner{Scripts: map[string]Script{
		"ask/agent": {Asks: []levers.Decision{{Question: "Proceed?", Options: []string{"yes"}, Recommended: 0}}},
	}}
	asks := make(chan Ask)
	workdir := t.TempDir()
	done := r.Run(context.Background(), fakeStageRequest(t, r, "GH-1", "ask", "agent", workdir), asks)
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
	done := fr.Run(context.Background(), fakeStageRequest(t, fr, "GH-1", "spec", "spec-writer", dir), asks)

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

func fakeStageRequest(t *testing.T, r *FakeRunner, issueID, stage, agent, workdir string) StageRequest {
	t.Helper()
	contract := capability.CompiledContract{
		ContractID: strings.Repeat("a", 64), AuthorityDigest: strings.Repeat("b", 64),
		Contract: capability.Contract{
			Version: capability.ContractVersion, IssueID: issueID, Stage: stage, AttemptID: "checkpoint-1",
			WorkspaceRoot: workdir, Operations: []capability.OperationClass{capability.OpWorkspaceRead, capability.OpWorkspaceMutate},
		},
	}
	plan, err := r.Preflight(context.Background(), PreflightRequest{IssueID: issueID, Stage: stage, Agent: agent, Workdir: workdir, Contract: contract})
	if err != nil {
		t.Fatal(err)
	}
	return StageRequest{IssueID: issueID, Stage: stage, Agent: agent, Workdir: workdir, Contract: contract, Plan: plan}
}
