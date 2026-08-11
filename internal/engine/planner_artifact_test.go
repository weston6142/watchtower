package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/plannerartifact"
	"github.com/weston6142/watchtower/internal/review"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/store"
)

type plannerArtifactEngineRunner struct {
	requests  []plannerartifact.WriteRequest
	failAt    int
	failOnce  bool
	mutate    func(string) error
	artifacts map[string]map[string]string
	stageRuns map[string]int
	started   bool
	callCount int
}

func (r *plannerArtifactEngineRunner) Preflight(_ context.Context, request runner.PreflightRequest) (capability.EnforcementPlan, error) {
	return testRunnerPreflight(request)
}

func (r *plannerArtifactEngineRunner) Run(_ context.Context, request runner.StageRequest, _ chan<- runner.Ask) <-chan runner.Result {
	done := make(chan runner.Result, 1)
	if artifacts, ok := r.artifacts[request.Stage]; ok {
		if r.stageRuns == nil {
			r.stageRuns = make(map[string]int)
		}
		r.stageRuns[request.Stage]++
		for name, content := range artifacts {
			if err := os.WriteFile(filepath.Join(request.Workdir, name), []byte(content), 0o644); err != nil {
				done <- runner.Result{Err: err}
				return done
			}
		}
		done <- runner.Result{Artifacts: artifacts}
		return done
	}
	done <- runner.Result{Err: fmt.Errorf("non-planner run requested")}
	return done
}

func (r *plannerArtifactEngineRunner) RunPlanner(ctx context.Context, request runner.StageRequest,
	_ chan<- runner.Ask, _ runner.ExplorationGate) <-chan runner.Result {
	done := make(chan runner.Result, 1)
	go func() {
		workdir := request.Workdir
		r.started = true
		r.callCount++
		authority := runner.PlannerArtifactAuthorityFromContext(ctx)
		if authority == nil {
			done <- runner.Result{Err: fmt.Errorf("planner authority unavailable")}
			return
		}
		for index, request := range r.requests {
			if r.failOnce && r.callCount == 1 && index == r.failAt {
				done <- runner.Result{Err: fmt.Errorf("transport failure at %s", request.Key)}
				return
			}
			if err := authority.ApplyPlannerArtifact(request); err != nil {
				done <- runner.Result{Err: err}
				return
			}
		}
		if r.mutate != nil {
			if err := r.mutate(workdir); err != nil {
				done <- runner.Result{Err: err}
				return
			}
		}
		done <- runner.Result{}
	}()
	return done
}

func plannerArtifactEngineFlow() flow.Flow {
	return flow.Flow{Name: "planner-artifact-engine", Stages: []flow.Stage{{
		Name: "plan", Agents: []flow.AgentRef{{Package: "planner"}}, Workspace: "none",
		Completion: flow.CompletionAll, Gate: flow.GatePlanReview, Retries: 1,
		CapabilityProfile: flow.ProfileArtifact,
		Artifacts:         []string{"plan.md", "touchset.json"},
	}}}
}

func plannerArtifactRecoveryFlow() flow.Flow {
	return flow.Flow{Name: "planner-artifact-recovery", Stages: []flow.Stage{
		{
			Name: "brainstorm", Agents: []flow.AgentRef{{Package: "brainstorm"}}, Workspace: "none",
			Completion: flow.CompletionAll, Gate: flow.GateAuto, CapabilityProfile: flow.ProfileArtifact,
			Artifacts: []string{"brainstorm.md"},
		},
		{
			Name: "spec", Agents: []flow.AgentRef{{Package: "spec-writer"}}, Workspace: "none",
			Completion: flow.CompletionAll, Gate: flow.GateAuto, CapabilityProfile: flow.ProfileArtifact,
			Artifacts: []string{"spec.md"},
		},
		{
			Name: "plan", Agents: []flow.AgentRef{{Package: "planner"}}, Workspace: "none",
			Completion: flow.CompletionAll, Gate: flow.GatePlanReview, CapabilityProfile: flow.ProfileArtifact,
			Artifacts: []string{"plan.md", "touchset.json"},
		},
	}}
}

func plannerArtifactRequests() []plannerartifact.WriteRequest {
	manifest := plannerArtifactEngineManifest()
	requests := make([]plannerartifact.WriteRequest, 0, len(manifest.Sections))
	for _, entry := range manifest.Sections {
		requests = append(requests, plannerartifact.WriteRequest{
			Manifest: manifest, Key: entry.Key, Markdown: "section " + entry.Key, Globs: entry.Globs,
		})
	}
	return requests
}

func plannerArtifactEngineManifest() plannerartifact.Manifest {
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

func TestPlannerArtifactEngineValidatesBeforeReview(t *testing.T) {
	f := plannerArtifactEngineFlow()
	r := &plannerArtifactEngineRunner{requests: plannerArtifactRequests()}
	e, s := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
		cfg.PlanReview = review.PolicySettings{ID: "planner-artifact-test", Version: "1", AutoApproveRegular: true, Valid: true}
	})
	id, err := e.CreateIssue("planner artifact engine", "", f.Name, levers.Preset(f, flow.LeverRegular), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if !r.started {
		t.Fatal("planner runner did not start")
	}
	if got := countEventType(t, s, id, core.EvPlanReviewRequested); got != 1 {
		t.Fatalf("plan review requests = %d, want 1", got)
	}
	if got := countPlannerArtifactEvents(t, s, id); got != 2 {
		t.Fatalf("artifact-produced events = %d, want 2", got)
	}
	artifacts, err := s.ArtifactPaths(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 2 {
		t.Fatalf("archived artifacts = %v", artifacts)
	}
}

func TestPlannerArtifactInvalidFinalPairBlocksArchiveAndReview(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(string) error
		want   string
	}{
		{name: "plan", mutate: func(workdir string) error {
			return os.WriteFile(filepath.Join(workdir, "plan.md"), []byte("# invalid\n"), 0o644)
		}, want: "plan.md"},
		{name: "touchset", mutate: func(workdir string) error {
			return os.WriteFile(filepath.Join(workdir, "touchset.json"), []byte(`{}`), 0o644)
		}, want: "touchset.json"},
		{name: "pair", mutate: func(workdir string) error {
			return os.WriteFile(filepath.Join(workdir, "touchset.json"), []byte(`{"globs":[]}`), 0o644)
		}, want: "pair"},
		{name: "unauthorized-content", mutate: func(workdir string) error {
			path := filepath.Join(workdir, "plan.md")
			plan, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			mutated := strings.Replace(string(plan), "section goal", "tampered goal", 1)
			if mutated == string(plan) {
				return fmt.Errorf("accepted goal section was not found")
			}
			return os.WriteFile(path, []byte(mutated), 0o644)
		}, want: "plan.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := plannerArtifactEngineFlow()
			r := &plannerArtifactEngineRunner{requests: plannerArtifactRequests(), mutate: tc.mutate}
			e, s := newEngineCfg(t, r, func(cfg *Config) {
				cfg.Flows = map[string]flow.Flow{f.Name: f}
				cfg.PlanReview = review.PolicySettings{ID: "planner-artifact-test", Version: "1", AutoApproveRegular: true, Valid: true}
			})
			id, err := e.CreateIssue("invalid planner artifact", "", f.Name, levers.Preset(f, flow.LeverRegular), 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := e.StartIssue(context.Background(), id); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("StartIssue error = %v, want validation scope %s", err, tc.want)
			}
			if got := countEventType(t, s, id, core.EvPlanReviewRequested); got != 0 {
				t.Fatalf("plan review requests = %d", got)
			}
			if got := countPlannerArtifactEvents(t, s, id); got != 0 {
				t.Fatalf("artifact-produced events = %d", got)
			}
			artifacts, err := s.ArtifactPaths(id)
			if err != nil {
				t.Fatal(err)
			}
			if len(artifacts) != 0 {
				t.Fatalf("archived artifacts = %v", artifacts)
			}
		})
	}
}

func countPlannerArtifactEvents(t *testing.T, s *store.Store, issueID string) int {
	t.Helper()
	count := 0
	for _, event := range mustEvents(t, s, issueID) {
		if event.Type != core.EvArtifactProduced {
			continue
		}
		var payload struct {
			Artifact string `json:"artifact"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Artifact == "plan.md" || payload.Artifact == "touchset.json" {
			count++
		}
	}
	return count
}

func TestPlannerArtifactRetriesOnlyPendingSection(t *testing.T) {
	for _, failAt := range []int{0, 3, 6} {
		t.Run(fmt.Sprintf("section-%d", failAt), func(t *testing.T) {
			f := plannerArtifactEngineFlow()
			r := &plannerArtifactEngineRunner{requests: plannerArtifactRequests(), failAt: failAt, failOnce: true}
			e, s := newEngineCfg(t, r, func(cfg *Config) {
				cfg.Flows = map[string]flow.Flow{f.Name: f}
				cfg.PlanReview = review.PolicySettings{ID: "planner-artifact-test", Version: "1", AutoApproveRegular: true, Valid: true}
			})
			id, err := e.CreateIssue("retry planner artifact", "", f.Name, levers.Preset(f, flow.LeverRegular), 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := e.StartIssue(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			if r.callCount != 2 {
				t.Fatalf("planner calls = %d, want retry", r.callCount)
			}
			if got := countEventType(t, s, id, core.EvPlanReviewRequested); got != 1 {
				t.Fatalf("plan review requests = %d", got)
			}
			plan, err := os.ReadFile(filepath.Join(e.cfg.DataDir, id, "plan.md"))
			if err != nil {
				t.Fatal(err)
			}
			for _, request := range r.requests {
				anchor := "<!-- watchtower-section: key=" + request.Key + " -->"
				if got := strings.Count(string(plan), anchor); got != 1 {
					t.Fatalf("anchor %s count = %d", request.Key, got)
				}
			}
			var touchset struct {
				Globs []string `json:"globs"`
			}
			if err := json.Unmarshal(mustReadEngine(t, filepath.Join(e.cfg.DataDir, id, "touchset.json")), &touchset); err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			for _, glob := range touchset.Globs {
				if seen[glob] {
					t.Fatalf("duplicate glob %s", glob)
				}
				seen[glob] = true
			}
			artifacts, err := s.ArtifactPaths(id)
			if err != nil || len(artifacts) != 2 {
				t.Fatalf("archived artifacts = %v err=%v", artifacts, err)
			}
		})
	}
}

func mustReadEngine(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPlannerArtifactInitializationFailsBeforeRunner(t *testing.T) {
	f := plannerArtifactEngineFlow()
	f.Stages[0].Retries = 0
	r := &plannerArtifactEngineRunner{requests: plannerArtifactRequests()}
	e, s := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
		cfg.PlanReview = review.PolicySettings{ID: "planner-artifact-test", Version: "1", AutoApproveRegular: true, Valid: true}
	})
	id, err := e.CreateIssue("legacy planner artifact", "", f.Name, levers.Preset(f, flow.LeverRegular), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	workdir := filepath.Join(e.cfg.DataDir, id)
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "plan.md"), []byte("legacy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "touchset.json"), []byte(`{"globs":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err == nil || !strings.Contains(err.Error(), "malformed-starting-artifact") {
		t.Fatalf("StartIssue error = %v, want malformed starting artifact", err)
	}
	if r.started {
		t.Fatal("planner runner started before artifact initialization succeeded")
	}
	if got := countEventType(t, s, id, core.EvPlanReviewRequested); got != 0 {
		t.Fatalf("plan review requests = %d", got)
	}
}

func TestEnginePlannerAuthorityBindsExactScopeAndValidatesPair(t *testing.T) {
	coordinator, err := store.Open(filepath.Join(t.TempDir(), "coordinator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	worktree := t.TempDir()
	binding := plannerartifact.Binding{IssueID: "GH-72", Stage: "plan", Attempt: 1, Worktree: worktree}
	authority, err := plannerartifact.CreateOrLoad(coordinator, binding)
	if err != nil {
		t.Fatal(err)
	}
	e := New(Config{Store: coordinator, DataDir: t.TempDir()})
	e.RegisterPlannerAuthority(authority)

	handle, gotBinding, err := e.IssuePlannerAuthority(plannerartifact.Binding{Worktree: worktree})
	if err != nil || handle == "" || gotBinding != authority.Binding() {
		t.Fatalf("issued planner authority = handle %q binding %+v err %v", handle, gotBinding, err)
	}
	if _, _, err := e.IssuePlannerAuthority(plannerartifact.Binding{IssueID: "GH-71", Worktree: worktree}); plannerartifact.ErrorClassOf(err) != plannerartifact.ErrorScopeMismatch {
		t.Fatalf("mismatched planner scope error = %v", err)
	}
	for _, request := range plannerArtifactRequests() {
		if _, err := e.ApplyPlannerArtifact(binding, handle, request); err != nil {
			t.Fatalf("engine apply %s: %v", request.Key, err)
		}
	}
	if err := e.ValidatePlannerAuthority(binding, handle); err != nil {
		t.Fatalf("engine final validation: %v", err)
	}
}

func TestEnginePlannerAuthorityRecoversFromDurableStoreAfterRestart(t *testing.T) {
	coordinator, err := store.Open(filepath.Join(t.TempDir(), "coordinator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	worktree := t.TempDir()
	binding := plannerartifact.Binding{IssueID: "GH-72", Stage: "plan", Attempt: 1, Worktree: worktree}
	authority, err := plannerartifact.CreateOrLoad(coordinator, binding)
	if err != nil {
		t.Fatal(err)
	}
	manifest := plannerArtifactRequests()[0].Manifest
	if err := authority.Apply(plannerartifact.WriteRequest{Manifest: manifest, Key: "goal", Markdown: "retained after restart", Globs: manifest.Sections[0].Globs}); err != nil {
		t.Fatal(err)
	}

	restarted := New(Config{Store: coordinator, DataDir: t.TempDir()})
	handle, recoveredBinding, err := restarted.IssuePlannerAuthority(plannerartifact.Binding{Worktree: worktree})
	if err != nil || handle == "" || recoveredBinding != authority.Binding() {
		t.Fatalf("restart recovery = handle %q binding %+v err %v", handle, recoveredBinding, err)
	}
	if _, err := restarted.ApplyPlannerArtifact(binding, handle, plannerartifact.WriteRequest{
		Manifest: manifest, Key: "architecture", Markdown: "next section after restart", Globs: manifest.Sections[1].Globs,
	}); err != nil {
		t.Fatalf("apply after restart: %v", err)
	}
}

func TestPlannerArtifactExplicitRetryAfterRestartRestoresDurablePrefix(t *testing.T) {
	f := plannerArtifactRecoveryFlow()
	first := &plannerArtifactEngineRunner{
		requests: plannerArtifactRequests(), failAt: 1, failOnce: true,
		artifacts: map[string]map[string]string{
			"brainstorm": {"brainstorm.md": "durable brainstorm\n"},
			"spec":       {"spec.md": "durable spec\n"},
		},
	}
	e, s := newEngineCfg(t, first, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
		cfg.PlanReview = review.PolicySettings{ID: "planner-artifact-test", Version: "1", AutoApproveRegular: true, Valid: true}
	})
	id, err := e.CreateIssue("retry planner after restart", "", f.Name, levers.Preset(f, flow.LeverRegular), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err == nil || !strings.Contains(err.Error(), "transport failure") {
		t.Fatalf("StartIssue error = %v, want planner transport failure", err)
	}
	workdir := filepath.Join(e.cfg.DataDir, id)
	canonicalWorkdir, err := filepath.EvalSymlinks(workdir)
	if err != nil {
		t.Fatal(err)
	}
	_, beforeDigest, beforeManifest, beforeSections, found, err := s.LoadPlannerArtifact(id, "plan", 1, canonicalWorkdir)
	if err != nil || !found {
		t.Fatalf("durable prefix before retry = found %v err %v", found, err)
	}
	var beforeAccepted []json.RawMessage
	if err := json.Unmarshal(beforeSections, &beforeAccepted); err != nil || len(beforeAccepted) != 1 {
		t.Fatalf("accepted prefix before retry = %s err %v", beforeSections, err)
	}
	if got := countArtifactEvents(t, s, id, "brainstorm.md"); got != 1 {
		t.Fatalf("brainstorm archive events before retry = %d", got)
	}
	if got := countArtifactEvents(t, s, id, "spec.md"); got != 1 {
		t.Fatalf("spec archive events before retry = %d", got)
	}
	if err := os.WriteFile(filepath.Join(workdir, "brainstorm.md"), []byte("tampered brainstorm\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "spec.md"), []byte("tampered spec\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(workdir, "plan.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "touchset.json"), []byte(`{"globs":["tampered/**"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	retry := &plannerArtifactEngineRunner{requests: plannerArtifactRequests(), artifacts: first.artifacts}
	restarted := newEngineOnFileWithFlow(t, s, retry, e.cfg.DataDir, f)
	restarted.cfg.PlanReview = e.cfg.PlanReview
	if err := restarted.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if err := restarted.RetryStage(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if retry.stageRuns["brainstorm"] != 0 || retry.stageRuns["spec"] != 0 {
		t.Fatalf("successful prior stages reran after retry: %+v", retry.stageRuns)
	}
	if got := string(mustReadEngine(t, filepath.Join(workdir, "brainstorm.md"))); got != "durable brainstorm\n" {
		t.Fatalf("restored brainstorm = %q", got)
	}
	if got := string(mustReadEngine(t, filepath.Join(workdir, "spec.md"))); got != "durable spec\n" {
		t.Fatalf("restored spec = %q", got)
	}
	_, afterDigest, afterManifest, afterSections, found, err := s.LoadPlannerArtifact(id, "plan", 1, canonicalWorkdir)
	if err != nil || !found {
		t.Fatalf("durable authority after retry = found %v err %v", found, err)
	}
	if bytes.Equal(beforeDigest, afterDigest) {
		t.Fatal("retry did not rotate the durable capability digest")
	}
	if !bytes.Equal(beforeManifest, afterManifest) {
		t.Fatalf("retry changed durable manifest identity:\nbefore %s\nafter  %s", beforeManifest, afterManifest)
	}
	var afterAccepted []json.RawMessage
	if err := json.Unmarshal(afterSections, &afterAccepted); err != nil || len(afterAccepted) != len(plannerArtifactRequests()) {
		t.Fatalf("accepted sections after retry = %s err %v", afterSections, err)
	}
	if !bytes.Equal(beforeAccepted[0], afterAccepted[0]) {
		t.Fatalf("retry changed accepted prefix:\nbefore %s\nafter  %s", beforeAccepted[0], afterAccepted[0])
	}
	plan := string(mustReadEngine(t, filepath.Join(workdir, "plan.md")))
	for _, request := range plannerArtifactRequests() {
		anchor := "<!-- watchtower-section: key=" + request.Key + " -->"
		if got := strings.Count(plan, anchor); got != 1 {
			t.Fatalf("anchor %s count = %d after recovery", request.Key, got)
		}
	}
	for _, artifact := range []string{"brainstorm.md", "spec.md", "plan.md", "touchset.json"} {
		if got := countArtifactEvents(t, s, id, artifact); got != 1 {
			t.Fatalf("%s archive events after retry = %d", artifact, got)
		}
	}
	events := mustEvents(t, s, id)
	reviewIndex := -1
	artifactIndex := map[string]int{"plan.md": -1, "touchset.json": -1}
	for index, event := range events {
		if event.Type == core.EvPlanReviewRequested {
			reviewIndex = index
		}
		if event.Type != core.EvArtifactProduced {
			continue
		}
		var payload struct {
			Artifact string `json:"artifact"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if _, tracked := artifactIndex[payload.Artifact]; tracked {
			artifactIndex[payload.Artifact] = index
		}
	}
	if reviewIndex < 0 || artifactIndex["plan.md"] < 0 || artifactIndex["touchset.json"] < 0 ||
		artifactIndex["plan.md"] >= reviewIndex || artifactIndex["touchset.json"] >= reviewIndex {
		t.Fatalf("artifact/review event order = plan %d touchset %d review %d", artifactIndex["plan.md"], artifactIndex["touchset.json"], reviewIndex)
	}
}

func countArtifactEvents(t *testing.T, s *store.Store, issueID, artifact string) int {
	t.Helper()
	count := 0
	for _, event := range mustEvents(t, s, issueID) {
		if event.Type != core.EvArtifactProduced {
			continue
		}
		var payload struct {
			Artifact string `json:"artifact"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Artifact == artifact {
			count++
		}
	}
	return count
}

func TestPlannerArtifactExplicitRetryWithoutDurableRecordFailsClosed(t *testing.T) {
	f := plannerArtifactEngineFlow()
	f.Stages[0].Retries = 0
	r := &plannerArtifactEngineRunner{requests: plannerArtifactRequests()}
	e, s := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
		cfg.PlanReview = review.PolicySettings{ID: "planner-artifact-test", Version: "1", AutoApproveRegular: true, Valid: true}
	})
	id, err := e.CreateIssue("retry planner without durable state", "", f.Name, levers.Preset(f, flow.LeverRegular), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.FailNextPlannerArtifactReadForTest()
	if err := e.StartIssue(context.Background(), id); err == nil {
		t.Fatal("initial planner authority read failure unexpectedly succeeded")
	}
	if r.started {
		t.Fatal("planner runner started before the failed authority read")
	}
	if err := e.RetryStage(context.Background(), id); plannerartifact.ErrorClassOf(err) != plannerartifact.ErrorAuthorityState {
		t.Fatalf("RetryStage error = %v, want %s", err, plannerartifact.ErrorAuthorityState)
	}
	if r.started {
		t.Fatal("planner runner started without durable retry authority")
	}
	canonicalWorkdir, err := filepath.EvalSymlinks(filepath.Join(e.cfg.DataDir, id))
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []int{1, 2} {
		_, _, _, _, found, loadErr := s.LoadPlannerArtifact(id, "plan", attempt, canonicalWorkdir)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if found {
			t.Fatalf("durable planner row exists without recoverable authority at attempt %d", attempt)
		}
	}
	if got := countEventType(t, s, id, core.EvPlanReviewRequested); got != 0 {
		t.Fatalf("plan review requests without durable authority = %d", got)
	}
	if got := countPlannerArtifactEvents(t, s, id); got != 0 {
		t.Fatalf("planner artifact events without durable authority = %d", got)
	}
	artifacts, err := s.ArtifactPaths(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 0 {
		t.Fatalf("archived artifacts without durable authority = %v", artifacts)
	}
}
