package runner

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/weston6142/watchtower/internal/plannerartifact"
)

type typedPlannerAuthorityProbe struct {
	requests []any
}

type runnerAuthorityRegistry struct {
	mu       sync.Mutex
	status   string
	digest   []byte
	manifest []byte
	sections []byte
	found    bool
}

func (r *runnerAuthorityRegistry) LoadPlannerArtifact(string, string, int, string) (string, []byte, []byte, []byte, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status, append([]byte(nil), r.digest...), append([]byte(nil), r.manifest...), append([]byte(nil), r.sections...), r.found, nil
}

func (r *runnerAuthorityRegistry) CreatePlannerArtifact(_ string, _ string, _ int, _ string, status string, digest, manifest, sections []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status, r.digest, r.manifest, r.sections, r.found = status, append([]byte(nil), digest...), append([]byte(nil), manifest...), append([]byte(nil), sections...), true
	return nil
}

func (r *runnerAuthorityRegistry) UpdatePlannerArtifact(issue, stage string, attempt int, worktree, status string, digest, manifest, sections []byte) error {
	return r.CreatePlannerArtifact(issue, stage, attempt, worktree, status, digest, manifest, sections)
}

func (r *runnerAuthorityRegistry) ExpirePlannerArtifact(string, string, int, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = "expired"
	return nil
}

func (p *typedPlannerAuthorityProbe) ApplyPlannerArtifact(request any) error {
	p.requests = append(p.requests, request)
	return nil
}

func TestFakePlannerUsesTypedAuthorityInsteadOfAmbientSession(t *testing.T) {
	manifest := plannerartifact.Manifest{Sections: []plannerartifact.ManifestEntry{
		{Key: "goal", Globs: []string{"internal/goal/**"}},
		{Key: "architecture", Globs: []string{"internal/architecture/**"}},
		{Key: "technology-stack", Globs: []string{"internal/technology/**"}},
		{Key: "execution-contract", Globs: []string{"internal/contract/**"}},
		{Key: "file-structure", Globs: []string{"internal/files/**"}},
		{Key: "task-0001", Globs: []string{"internal/task/**"}},
		{Key: "verification", Globs: []string{"internal/verification/**"}},
	}}
	request := plannerartifact.WriteRequest{Manifest: manifest, Key: "goal", Markdown: "goal", Globs: manifest.Sections[0].Globs}
	probe := &typedPlannerAuthorityProbe{}
	fr := &FakeRunner{Scripts: map[string]Script{"plan/planner": {PlannerRequests: []plannerartifact.WriteRequest{request}}}}
	t.Setenv("WATCHTOWER_PLANNER_SESSION", "agent-private-session")
	workdir := t.TempDir()
	result := <-fr.RunPlanner(WithPlannerArtifactAuthority(context.Background(), probe),
		fakeStageRequest(t, fr, "GH-62", "plan", "planner", workdir), make(chan Ask), &scriptedTestGate{})
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if len(probe.requests) != 1 {
		t.Fatalf("typed authority requests = %d, want 1", len(probe.requests))
	}
}

func TestPlannerArtifactEnvironmentStripsPrivateAuthorityValues(t *testing.T) {
	merged := MergeEnvironment(
		[]string{"WATCHTOWER_PLANNER_SESSION=private", "WATCHTOWER_PLANNER_CAPABILITY=secret", "KEEP=inherited"},
		[]string{"WATCHTOWER_PLANNER_DESCRIPTOR=3", "KEEP=extra"},
		[]string{"WATCHTOWER_PLANNER_SESSION=overlay", "WATCHTOWER_PLANNER_CAPABILITY=overlay-secret", "SAFE=managed"},
	)
	joined := strings.Join(merged, "\n")
	for _, forbidden := range []string{"WATCHTOWER_PLANNER_SESSION", "WATCHTOWER_PLANNER_CAPABILITY", "WATCHTOWER_PLANNER_DESCRIPTOR"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("private planner authority value reached child environment: %q", joined)
		}
	}
	if !strings.Contains(joined, "KEEP=extra") || !strings.Contains(joined, "SAFE=managed") {
		t.Fatalf("ordinary environment values were lost: %q", joined)
	}
}

type scriptedTestGate struct{}

func (*scriptedTestGate) Admit(context.Context, ToolCall) (ToolDecision, error) {
	return ToolDecision{Allowed: true}, nil
}

func (*scriptedTestGate) Complete(context.Context, ToolDecision, *int64, error) error { return nil }
