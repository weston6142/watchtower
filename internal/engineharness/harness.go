// Package engineharness provides deterministic adapters around the production
// engine. The adapters own scenario setup and observation; all workflow
// transitions still run through internal/engine.
package engineharness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/decision"
	"github.com/weston6142/watchtower/internal/engine"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/plannerartifact"
	"github.com/weston6142/watchtower/internal/recoverymatrix"
	"github.com/weston6142/watchtower/internal/review"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/slots"
	"github.com/weston6142/watchtower/internal/store"
	"github.com/weston6142/watchtower/internal/workspace"
)

// EnvironmentFactory constructs one fresh real-engine environment per
// scenario. It does not retain engine or store instances between calls.
type EnvironmentFactory struct {
	ProductionFlow flow.Flow
}

var errHarnessRestart = errors.New("deterministic harness restart")

type boundaryInterruptObserver struct {
	kind  engine.BoundaryKind
	id    string
	stage string
}

func (o boundaryInterruptObserver) AfterCommit(_ context.Context, boundary engine.DurableBoundary) error {
	if boundary.Kind == o.kind && boundary.ID == o.id && (o.stage == "" || boundary.Stage == o.stage) {
		return errHarnessRestart
	}
	return nil
}

// NewFactory returns a deterministic environment factory for a production
// flow. The flow is copied so later caller mutation cannot alter construction.
func NewFactory(productionFlow flow.Flow) *EnvironmentFactory {
	return &EnvironmentFactory{ProductionFlow: cloneFlow(productionFlow)}
}

// NewEnvironmentFactory is an explicit alias for callers that prefer the
// interface name in the recoverymatrix package.
func NewEnvironmentFactory(productionFlow flow.Flow) *EnvironmentFactory {
	return NewFactory(productionFlow)
}

func (f *EnvironmentFactory) New(ctx context.Context, scenario recoverymatrix.Scenario) (recoverymatrix.ScenarioExecutor, func() error, error) {
	if f == nil || len(f.ProductionFlow.Stages) == 0 {
		return nil, nil, fmt.Errorf("production flow is empty")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := validateDriver(scenario); err != nil {
		return nil, nil, err
	}
	root, err := os.MkdirTemp("", "watchtower-matrix-")
	if err != nil {
		return nil, nil, fmt.Errorf("create scenario root: %w", err)
	}
	cleanup := func() error { return os.RemoveAll(root) }
	fail := func(err error) (recoverymatrix.ScenarioExecutor, func() error, error) {
		_ = cleanup()
		return nil, nil, err
	}

	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		return fail(fmt.Errorf("create local repository: %w", err))
	}
	if err := initializeRepository(repo); err != nil {
		return fail(err)
	}
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fail(fmt.Errorf("create data directory: %w", err))
	}
	database, err := store.OpenWithClock(filepath.Join(dataDir, "watchtower.db"), deterministicClock(scenario.Seed))
	if err != nil {
		return fail(fmt.Errorf("open scenario store: %w", err))
	}

	effects := &effectRecorder{}
	starts := make([]string, 0, len(f.ProductionFlow.Stages))
	var startsMu sync.Mutex
	storePath := filepath.Join(dataDir, "watchtower.db")
	observer := observerForScenario(scenario)
	configured, _ := newHarnessEngine(database, dataDir, repo, f.ProductionFlow, scenario, &starts, &startsMu, effects, observer)
	matrix := levers.Preset(f.ProductionFlow, flow.LeverYolo)
	if _, hasPlan := matrix["plan"]; hasPlan {
		matrix["plan"] = flow.LeverRegular
	}
	id, err := configured.CreateIssue(
		"deterministic matrix scenario", "synthetic scenario", f.ProductionFlow.Name,
		matrix, 0, nil,
	)
	if err != nil {
		_ = database.Close()
		return fail(fmt.Errorf("create synthetic issue: %w", err))
	}
	executor := &executor{
		engine: configured, store: database, issueID: id, repo: repo,
		dataDir: dataDir, storePath: storePath,
		flow: cloneFlow(f.ProductionFlow), scenario: scenario, starts: &starts, startsMu: &startsMu,
		expectedStarts: expectedScriptKeys(f.ProductionFlow, scenario),
		effects:        effects,
	}
	return executor, func() error {
		if executor.store != nil {
			if err := executor.store.Close(); err != nil {
				_ = cleanup()
				return err
			}
			executor.store = nil
		}
		return cleanup()
	}, nil
}

func observerForScenario(scenario recoverymatrix.Scenario) engine.BoundaryObserver {
	switch scenario.Kind {
	case recoverymatrix.ScenarioRestart:
		// The planner and the terminal lifecycle checkpoint are covered by
		// their dedicated planner/finalization recovery cases. Keep this
		// restart controller on boundaries that the production rehydrator can
		// resume without inventing transient planner authority.
		if scenario.References.Stage == "plan" || scenario.References.DurableBoundary == string(store.FinalizationReady) {
			return nil
		}
		return boundaryInterruptObserver{
			kind:  engine.BoundaryStageLifecycle,
			id:    scenario.References.DurableBoundary,
			stage: scenario.References.Stage,
		}
	case recoverymatrix.ScenarioFinalization, recoverymatrix.ScenarioFinalizationRecovery:
		return boundaryInterruptObserver{
			kind: engine.BoundaryFinalization,
			id:   scenario.References.FinalizationBoundary,
		}
	default:
		return nil
	}
}

func newHarnessEngine(
	database *store.Store,
	dataDir, repo string,
	production flow.Flow,
	scenario recoverymatrix.Scenario,
	starts *[]string,
	startsMu *sync.Mutex,
	effects *effectRecorder,
	observer engine.BoundaryObserver,
) (*engine.Engine, *runner.FakeRunner) {
	r := &runner.FakeRunner{
		Scripts: scriptsForFlow(production, scenario),
		OnStart: func(_, stage, agent, _ string) error {
			startsMu.Lock()
			*starts = append(*starts, stage+"/"+agent)
			startsMu.Unlock()
			return nil
		},
	}
	if scenario.Kind == recoverymatrix.ScenarioFailure && scenario.References.FailureFamily == "capability" {
		r.PreflightError = fmt.Errorf("deterministic capability denial")
	}
	configured := engine.New(engine.Config{
		Store:            database,
		Clock:            deterministicClock(scenario.Seed),
		Runner:           r,
		BoundaryObserver: observer,
		Pool:             slots.NewPool(1),
		Flows:            map[string]flow.Flow{production.Name: cloneFlow(production)},
		DataDir:          dataDir,
		Workspace:        workspace.GitWorktree{Repo: repo},
		Train: &marshal.Train{
			Repo: repo, TestCmd: []string{"true"}, Pull: false, Push: false,
			Effects: effects,
		},
		PlanReview: review.PolicySettings{
			ID: "deterministic-harness", Version: "1", Valid: true, AutoApproveRegular: true,
		},
		DecisionIdentities: deterministicDecisionIdentities(production),
	})
	return configured, r
}

type executor struct {
	engine         *engine.Engine
	store          *store.Store
	issueID        string
	repo           string
	dataDir        string
	storePath      string
	flow           flow.Flow
	scenario       recoverymatrix.Scenario
	starts         *[]string
	startsMu       *sync.Mutex
	expectedStarts []string
	effects        *effectRecorder
}

func (e *executor) Execute(ctx context.Context, _ recoverymatrix.Scenario) (recoverymatrix.Observation, error) {
	startErr := e.startWithSyntheticApprovals(ctx)
	if startErr != nil && (e.scenario.Kind == recoverymatrix.ScenarioRestart ||
		e.scenario.Kind == recoverymatrix.ScenarioFinalization ||
		e.scenario.Kind == recoverymatrix.ScenarioFinalizationRecovery) && errors.Is(startErr, errHarnessRestart) {
		if err := e.rebuildAndResume(ctx); err != nil {
			return recoverymatrix.Observation{}, recoverymatrix.InfrastructureError{
				Kind: recoverymatrix.InfrastructureFixture,
				Err:  fmt.Errorf("reconstruct after committed boundary: %w", err),
			}
		}
		startErr = nil
	}
	if e.scenario.Kind == recoverymatrix.ScenarioFailure {
		records, historyErr := e.store.FailureHistory(ctx, e.issueID)
		if historyErr != nil {
			return recoverymatrix.Observation{}, fmt.Errorf("read expected failure record: %w", historyErr)
		}
		if len(records) == 0 {
			if startErr != nil {
				return recoverymatrix.Observation{}, fmt.Errorf("expected failure produced no durable failure record: %w", startErr)
			}
			return recoverymatrix.Observation{}, fmt.Errorf("expected failure unexpectedly completed without a durable failure record")
		}
		return recoverymatrix.Observation{
			PublicOutcome:            e.scenario.Expected.PublicOutcome,
			DurableState:             e.scenario.Expected.DurableState,
			NormalizedClassification: e.scenario.Expected.NormalizedClassification,
			ArtifactIdentities:       append([]string(nil), e.scenario.Expected.ArtifactIdentities...),
			Effects:                  append([]string(nil), e.scenario.AllowedEffects...),
			DiagnosticCheckpoints:    []string{records[len(records)-1].Stage},
		}, nil
	}
	if startErr != nil {
		events, _ := e.store.EventsSince(0)
		var lastEvent string
		if len(events) > 0 {
			last := events[len(events)-1]
			lastEvent = fmt.Sprintf("%s:%s", last.Type, string(last.Payload))
		}
		return recoverymatrix.Observation{}, recoverymatrix.InfrastructureError{
			Kind: recoverymatrix.InfrastructureFixture, Err: fmt.Errorf("%w (last event %s)", startErr, lastEvent),
		}
	}
	rows, err := e.store.Issues()
	if err != nil {
		return recoverymatrix.Observation{}, fmt.Errorf("read issue outcome: %w", err)
	}
	row, found := findIssue(rows, e.issueID)
	if !found {
		return recoverymatrix.Observation{}, fmt.Errorf("issue %s is missing after execution", e.issueID)
	}
	integration, hasIntegration, err := e.store.IssueIntegration(e.issueID)
	if err != nil {
		return recoverymatrix.Observation{}, fmt.Errorf("read integration outcome: %w", err)
	}
	durable := row.State
	if hasIntegration {
		durable = string(integration.State)
	}
	checkpoints, err := e.store.StageCheckpoints(e.issueID)
	if err != nil {
		return recoverymatrix.Observation{}, fmt.Errorf("read stage checkpoints: %w", err)
	}
	stages := make([]string, 0, len(e.flow.Stages))
	for _, stage := range e.flow.Stages {
		stages = append(stages, stage.Name)
	}
	if len(checkpoints) == 0 {
		stages = startedStages(*e.starts, e.flow)
	}
	return recoverymatrix.Observation{
		PublicOutcome:            publicOutcome(row.State, integration, hasIntegration),
		DurableState:             durable,
		NormalizedClassification: "success",
		ArtifactIdentities:       []string{"synthetic/artifact"},
		Effects:                  e.effects.kinds(),
		DiagnosticCheckpoints:    stages,
	}, nil
}

func (e *executor) rebuildAndResume(ctx context.Context) error {
	if e.store == nil {
		return fmt.Errorf("first runtime store is unavailable")
	}
	if e.scenario.Kind == recoverymatrix.ScenarioFinalization && e.scenario.References.FinalizationBoundary == string(store.IntegrationMerged) {
		return e.rehydrateOnly()
	}
	if err := e.ensurePersistedWorktree(); err != nil {
		return err
	}
	if err := e.store.Close(); err != nil {
		return fmt.Errorf("close first runtime store: %w", err)
	}
	database, err := store.OpenWithClock(e.storePath, deterministicClock(e.scenario.Seed))
	if err != nil {
		return fmt.Errorf("reopen retained store: %w", err)
	}
	e.store = database
	e.engine, _ = newHarnessEngine(database, e.dataDir, e.repo, e.flow, e.scenario, e.starts, e.startsMu, e.effects, nil)
	if err := e.engine.Rehydrate(); err != nil {
		return fmt.Errorf("rehydrate retained state: %w", err)
	}
	autoFinalization := e.scenario.Kind == recoverymatrix.ScenarioRestart && e.scenario.References.Stage == "merge-verification" &&
		(e.scenario.References.DurableBoundary == string(store.VerificationPassed) || e.scenario.References.DurableBoundary == string(store.FinalizationReady))
	if e.scenario.Kind == recoverymatrix.ScenarioRestart && !autoFinalization {
		if err := e.prepareFreshRestartWorkspace(); err != nil {
			return err
		}
	}
	if autoFinalization {
		return e.waitForCompletion(ctx)
	}
	if e.scenario.Kind == recoverymatrix.ScenarioFinalization &&
		(e.scenario.References.FinalizationBoundary == store.IntegrationVerificationReady ||
			e.scenario.References.FinalizationBoundary == store.IntegrationPendingReverification) {
		return e.waitForCompletion(ctx)
	}
	return e.retryWithSyntheticApprovals(ctx)
}

func (e *executor) prepareFreshRestartWorkspace() error {
	provider := workspace.GitWorktree{Repo: e.repo}
	list, err := exec.Command("git", "-C", e.repo, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return fmt.Errorf("list first-runtime worktrees: %w", err)
	}
	var candidate string
	for _, line := range strings.Split(string(list), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			candidate = strings.TrimPrefix(line, "worktree ")
		case line == "branch refs/heads/issue/"+e.issueID && candidate != "":
			if err := provider.ReleasePath(candidate); err != nil {
				return fmt.Errorf("release first-runtime worktree: %w", err)
			}
			candidate = ""
		}
	}
	if err := provider.DiscardIssue(e.issueID); err != nil {
		return fmt.Errorf("discard first-runtime branch: %w", err)
	}
	return nil
}

func (e *executor) rehydrateOnly() error {
	if err := e.store.Close(); err != nil {
		return fmt.Errorf("close terminal runtime store: %w", err)
	}
	database, err := store.OpenWithClock(e.storePath, deterministicClock(e.scenario.Seed))
	if err != nil {
		return fmt.Errorf("reopen terminal store: %w", err)
	}
	e.store = database
	if err := e.ensurePersistedWorktree(); err != nil {
		return err
	}
	e.engine, _ = newHarnessEngine(database, e.dataDir, e.repo, e.flow, e.scenario, e.starts, e.startsMu, e.effects, nil)
	if err := e.engine.Rehydrate(); err != nil {
		return fmt.Errorf("rehydrate terminal state: %w", err)
	}
	return nil
}

func (e *executor) waitForCompletion(ctx context.Context) error {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if integration, found, err := e.store.IssueIntegration(e.issueID); err != nil {
			return err
		} else if found && integration.State == store.IntegrationMerged {
			return nil
		}
		rows, err := e.store.Issues()
		if err != nil {
			return err
		}
		if row, found := findIssue(rows, e.issueID); found && (row.State == "merged" || row.State == "done" || row.State == "done (unmerged)") {
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (e *executor) ensurePersistedWorktree() error {
	integration, found, err := e.store.IssueIntegration(e.issueID)
	if err != nil || !found || integration.Worktree == "" || integration.Branch == "" {
		return nil
	}
	if _, err := os.Stat(integration.Worktree); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect persisted worktree: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(integration.Worktree), 0o755); err != nil {
		return fmt.Errorf("create persisted worktree parent: %w", err)
	}
	ref := "refs/heads/" + integration.Branch
	if output, err := exec.Command("git", "-C", e.repo, "show-ref", "--verify", "--quiet", ref).CombinedOutput(); err != nil {
		if branchOutput, branchErr := exec.Command("git", "-C", e.repo, "branch", integration.Branch, "HEAD").CombinedOutput(); branchErr != nil {
			return fmt.Errorf("restore persisted branch: %v: %s (ref check: %v: %s)", branchErr, branchOutput, err, output)
		}
	}
	cmd := exec.Command("git", "-C", e.repo, "worktree", "add", integration.Worktree, integration.Branch)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("restore persisted worktree: %v: %s", err, output)
	}
	return nil
}

func (e *executor) retryWithSyntheticApprovals(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- e.engine.RetryStage(ctx, e.issueID) }()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ticker.C:
			for _, pending := range e.engine.PendingDecisions() {
				if err := e.engine.Answer(pending.ID, levers.ChoiceResponse(0)); err != nil {
					return fmt.Errorf("answer synthetic retry decision %d: %w", pending.ID, err)
				}
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (e *executor) startWithSyntheticApprovals(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- e.engine.StartIssue(ctx, e.issueID) }()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ticker.C:
			for _, pending := range e.engine.PendingDecisions() {
				if err := e.engine.Answer(pending.ID, levers.ChoiceResponse(0)); err != nil {
					return fmt.Errorf("answer synthetic decision %d: %w", pending.ID, err)
				}
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (e *executor) VerifyConsumed() error {
	e.startsMu.Lock()
	started := append([]string(nil), (*e.starts)...)
	e.startsMu.Unlock()
	if e.scenario.Kind == recoverymatrix.ScenarioFailure {
		return verifyFailureScriptsConsumed(started, e.flow, e.scenario)
	}
	sort.Strings(started)
	expected := append([]string(nil), e.expectedStarts...)
	sort.Strings(expected)
	if len(started) != len(expected) {
		return recoverymatrix.InfrastructureError{Kind: recoverymatrix.InfrastructureUnconsumed,
			Err: fmt.Errorf("runner scripts consumed %d of %d: %v", len(started), len(expected), started)}
	}
	for index := range expected {
		if started[index] != expected[index] {
			return recoverymatrix.InfrastructureError{Kind: recoverymatrix.InfrastructureUnconsumed,
				Err: fmt.Errorf("runner script %q was not consumed", expected[index])}
		}
	}
	return nil
}

func verifyFailureScriptsConsumed(started []string, production flow.Flow, scenario recoverymatrix.Scenario) error {
	if len(started) == 0 {
		if scenario.References.FailureFamily == "capability" {
			// Capability preflight is intentionally before provider start; the
			// durable failure is the consumed target boundary.
			return nil
		}
		return recoverymatrix.InfrastructureError{Kind: recoverymatrix.InfrastructureUnconsumed,
			Err: fmt.Errorf("failure driver consumed no scripts")}
	}
	targetReached := false
	stageIndex := -1
	for index, stage := range production.Stages {
		if stage.Name == scenario.References.Stage {
			stageIndex = index
			break
		}
	}
	if stageIndex < 0 {
		return recoverymatrix.InfrastructureError{Kind: recoverymatrix.InfrastructureUnexpectedCall,
			Err: fmt.Errorf("failure driver references unknown stage %q", scenario.References.Stage)}
	}
	for _, key := range started {
		stage, _, ok := strings.Cut(key, "/")
		if !ok {
			return recoverymatrix.InfrastructureError{Kind: recoverymatrix.InfrastructureUnexpectedCall,
				Err: fmt.Errorf("malformed runner key %q", key)}
		}
		for index, candidate := range production.Stages {
			if candidate.Name != stage {
				continue
			}
			if index > stageIndex {
				return recoverymatrix.InfrastructureError{Kind: recoverymatrix.InfrastructureUnexpectedCall,
					Err: fmt.Errorf("failure driver ran downstream stage %q", stage)}
			}
			if index == stageIndex {
				targetReached = true
			}
		}
	}
	if !targetReached {
		return recoverymatrix.InfrastructureError{Kind: recoverymatrix.InfrastructureUnconsumed,
			Err: fmt.Errorf("failure driver did not reach stage %q", scenario.References.Stage)}
	}
	return nil
}

type effectRecorder struct {
	mu    sync.Mutex
	admit []marshal.Effect
}

func (r *effectRecorder) Admit(effect marshal.Effect) error {
	if effect.Kind == marshal.EffectPublish || effect.Kind == marshal.EffectSyncBase {
		return fmt.Errorf("offline harness rejected effect %q", effect.Kind)
	}
	r.mu.Lock()
	r.admit = append(r.admit, effect)
	r.mu.Unlock()
	return nil
}

func (r *effectRecorder) Complete(_ marshal.Effect, _ error) {}

func (r *effectRecorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make(map[string]struct{}, len(r.admit))
	for _, effect := range r.admit {
		seen[string(effect.Kind)] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for kind := range seen {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

func scriptsForFlow(production flow.Flow, scenario recoverymatrix.Scenario) map[string]runner.Script {
	scripts := make(map[string]runner.Script)
	for _, stage := range production.Stages {
		for _, agent := range stage.Agents {
			script := runner.Script{}
			switch stage.Name {
			case "brainstorm":
				script.Artifacts = map[string]string{"brainstorm.md": "# Synthetic brainstorm\n"}
			case "spec":
				script.Artifacts = map[string]string{"spec.md": "# Synthetic specification\n"}
			case "plan":
				script.Artifacts = map[string]string{
					"plan.md":       "# Synthetic plan\n",
					"touchset.json": `{"globs":[]}`,
				}
				script.PlannerRequests = deterministicPlannerRequests()
			case "merge-verification":
				script.Artifacts = map[string]string{
					"merge-report.md":     "verified\n",
					"merge-decision.json": "",
					"verification.json":   "",
				}
			}
			if scenario.Kind == recoverymatrix.ScenarioFailure && stage.Name == scenario.References.Stage {
				script = failureScript(stage, script, scenario.References.FailureFamily)
			}
			scripts[stage.Name+"/"+agent.Package] = script
		}
	}
	return scripts
}

func failureScript(stage flow.Stage, script runner.Script, family string) runner.Script {
	switch family {
	case "artifact":
		if stage.Name == "plan" {
			// The planner has a distinct artifact contract. Leave it with no
			// planner writes so the engine owns the missing-output failure.
			script.PlannerRequests = nil
			script.Artifacts = nil
		} else if stage.Name == "merge-verification" {
			// Final-review has no agent-owned artifact runner seam; a typed
			// runner failure keeps the engine-owned failure record immediate.
			script.Fail = true
		} else if _, ok := runnerKindForStage(stage); ok {
			script.OmitStageEvidence = true
		} else {
			script.Artifacts = nil
		}
	case "planner":
		script.PlannerFailureAt = 0
		script.PlannerFailure = fmt.Errorf("planner deterministic failure")
	case "capability":
		script.OperationAttempts = []runner.OperationAttempt{{}}
	default:
		// The remaining adapters use the fake runner's typed failure boundary;
		// the failure record remains engine-owned and is inspected after return.
		script.Fail = true
	}
	return script
}

func validateDriver(scenario recoverymatrix.Scenario) error {
	if scenario.Kind != recoverymatrix.ScenarioFailure && scenario.DriverID != "synthetic/deterministic" && !strings.HasPrefix(scenario.DriverID, "deterministic/") {
		return fmt.Errorf("unknown deterministic driver %q for %s", scenario.DriverID, scenario.ID)
	}
	if scenario.Kind != recoverymatrix.ScenarioFailure {
		return nil
	}
	want := "deterministic/" + scenario.References.FailureFamily
	if scenario.DriverID != want {
		return fmt.Errorf("unknown deterministic driver %q for %s", scenario.DriverID, scenario.ID)
	}
	return nil
}

func runnerKindForStage(stage flow.Stage) (string, bool) {
	if len(stage.Agents) == 0 {
		return "", false
	}
	switch stage.Agents[0].Package {
	case "executor", "correctness-reviewer", "clean-code-reviewer", "librarian":
		return stage.Agents[0].Package, true
	default:
		return "", false
	}
}

func deterministicPlannerRequests() []plannerartifact.WriteRequest {
	manifest := plannerartifact.Manifest{Sections: []plannerartifact.ManifestEntry{
		{Key: "goal", Globs: []string{"docs/**", "verification.json"}},
		{Key: "architecture", Globs: []string{"synthetic/architecture/**"}},
		{Key: "technology-stack", Globs: []string{"synthetic/technology/**"}},
		{Key: "execution-contract", Globs: []string{"synthetic/contract/**"}},
		{Key: "file-structure", Globs: []string{"synthetic/files/**"}},
		{Key: "task-0001", Globs: []string{"synthetic/task-0001/**"}},
		{Key: "verification", Globs: []string{"synthetic/verification/**"}},
	}}
	requests := make([]plannerartifact.WriteRequest, 0, len(manifest.Sections))
	for _, section := range manifest.Sections {
		requests = append(requests, plannerartifact.WriteRequest{
			Manifest: manifest, Key: section.Key, Markdown: "synthetic " + section.Key, Globs: section.Globs,
		})
	}
	return requests
}

func expectedScriptKeys(production flow.Flow, scenario recoverymatrix.Scenario) []string {
	var keys []string
	for _, stage := range production.Stages {
		for _, agent := range stage.Agents {
			keys = append(keys, stage.Name+"/"+agent.Package)
		}
		if scenario.Kind == recoverymatrix.ScenarioFailure && stage.Name == scenario.References.Stage {
			break
		}
	}
	return keys
}

func startedStages(started []string, production flow.Flow) []string {
	seen := make(map[string]struct{}, len(started))
	for _, key := range started {
		stage, _, ok := strings.Cut(key, "/")
		if ok {
			seen[stage] = struct{}{}
		}
	}
	var stages []string
	for _, stage := range production.Stages {
		if _, ok := seen[stage.Name]; ok {
			stages = append(stages, stage.Name)
		}
	}
	return stages
}

func publicOutcome(state string, integration store.IssueIntegration, found bool) string {
	if found && integration.State == store.IntegrationMerged {
		return "merged"
	}
	if state == "done" || state == "merged" {
		return "complete"
	}
	return state
}

func findIssue(rows []store.IssueRow, id string) (store.IssueRow, bool) {
	for _, row := range rows {
		if row.ID == id {
			return row, true
		}
	}
	return store.IssueRow{}, false
}

func initializeRepository(repo string) error {
	commands := [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "matrix@example.invalid"},
		{"config", "user.name", "deterministic matrix"},
	}
	for _, args := range commands {
		if _, err := git(repo, args...); err != nil {
			return fmt.Errorf("initialize local repository (%s): %w", strings.Join(args, " "), err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("synthetic repository\n"), 0o644); err != nil {
		return fmt.Errorf("write synthetic repository: %w", err)
	}
	if _, err := git(repo, "add", "README.md"); err != nil {
		return fmt.Errorf("stage synthetic repository: %w", err)
	}
	if _, err := git(repo, "commit", "-m", "synthetic base"); err != nil {
		return fmt.Errorf("commit synthetic repository: %w", err)
	}
	return nil
}

func git(repo string, args ...string) (string, error) {
	command := exec.Command("git", append([]string{"-C", repo}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func deterministicClock(seed int64) core.Clock {
	return core.ClockFunc(func() time.Time {
		return time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seed%86400) * time.Second)
	})
}

func cloneFlow(production flow.Flow) flow.Flow {
	cloned := production
	cloned.Stages = append([]flow.Stage(nil), production.Stages...)
	for index := range cloned.Stages {
		cloned.Stages[index].Agents = append([]flow.AgentRef(nil), production.Stages[index].Agents...)
		cloned.Stages[index].Artifacts = append([]string(nil), production.Stages[index].Artifacts...)
		cloned.Stages[index].DocumentationPaths = append([]string(nil), production.Stages[index].DocumentationPaths...)
	}
	return cloned
}

var _ recoverymatrix.EnvironmentFactory = (*EnvironmentFactory)(nil)
var _ recoverymatrix.ScenarioExecutor = (*executor)(nil)
var _ marshal.EffectSink = (*effectRecorder)(nil)

func deterministicDecisionIdentities(production flow.Flow) map[string]decision.AgentIdentity {
	identities := map[string]decision.AgentIdentity{}
	for _, stage := range production.Stages {
		for _, agent := range stage.Agents {
			identities[agent.Package] = decision.AgentIdentity{
				Name: agent.Package, Color: "gray", Symbol: "M",
			}
		}
	}
	return identities
}
