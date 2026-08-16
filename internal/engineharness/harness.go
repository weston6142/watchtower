// Package engineharness provides deterministic adapters around the production
// engine. The adapters own scenario setup and observation; all workflow
// transitions still run through internal/engine.
package engineharness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/decision"
	"github.com/weston6142/watchtower/internal/engine"
	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/plannerartifact"
	"github.com/weston6142/watchtower/internal/recoverymatrix"
	"github.com/weston6142/watchtower/internal/review"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/slots"
	"github.com/weston6142/watchtower/internal/stagelifecycle"
	"github.com/weston6142/watchtower/internal/stageresult"
	"github.com/weston6142/watchtower/internal/store"
	"github.com/weston6142/watchtower/internal/workspace"
)

// EnvironmentFactory constructs one fresh real-engine environment per
// scenario. It does not retain engine or store instances between calls.
type EnvironmentFactory struct {
	ProductionFlow flow.Flow
}

var (
	errHarnessRestart         = errors.New("deterministic harness restart")
	errHarnessDecisionPending = errors.New("deterministic decision pending")
)

type boundaryInterruptObserver struct {
	mu       sync.Mutex
	kind     engine.BoundaryKind
	id       string
	stage    string
	scenario recoverymatrix.Scenario
	repo     string
	fired    bool
	mutated  bool
}

type scenarioDriver struct {
	mu    sync.Mutex
	state string
}

type failureDriver struct {
	mu     sync.Mutex
	family failure.Site
	stage  string
	fired  bool
}

func newFailureDriver(scenario recoverymatrix.Scenario) *failureDriver {
	if scenario.Kind != recoverymatrix.ScenarioFailure || !isInjectedFailureFamily(scenario.References.FailureFamily) {
		return nil
	}
	return &failureDriver{family: failure.Site(scenario.References.FailureFamily), stage: scenario.References.Stage}
}

func (d *failureDriver) inject(site failure.Site, stage string) error {
	if d == nil || d.family != site || d.stage != stage {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fired = true
	return errors.New("deterministic subsystem boundary failure")
}

func (d *failureDriver) BeforeWorkspace(_ context.Context, _, stage string) error {
	return d.inject(failure.SiteWorkspace, stage)
}
func (d *failureDriver) BeforeArtifact(_ context.Context, _, stage string) error {
	return d.inject(failure.SiteArtifact, stage)
}
func (d *failureDriver) BeforePlanner(_ context.Context, _, stage string) error {
	return d.inject(failure.SitePlanner, stage)
}
func (d *failureDriver) BeforeGit(_ context.Context, _, stage string) error {
	return d.inject(failure.SiteGit, stage)
}
func (d *failureDriver) BeforeVerification(_ context.Context, _, stage string) error {
	return d.inject(failure.SiteVerification, stage)
}
func (d *failureDriver) BeforeCache(_ context.Context, _, stage string) error {
	return d.inject(failure.SiteCache, stage)
}
func (d *failureDriver) BeforeStore(_ context.Context, _, stage string) error {
	return d.inject(failure.SiteStore, stage)
}
func (d *failureDriver) BeforeFinalization(_ context.Context, _, stage string) error {
	return d.inject(failure.SiteFinalization, stage)
}

func (d *failureDriver) wasFired() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fired
}

func (d *scenarioDriver) get() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state
}

func (d *scenarioDriver) set(state string) {
	d.mu.Lock()
	d.state = state
	d.mu.Unlock()
}

func (o *boundaryInterruptObserver) AfterCommit(_ context.Context, boundary engine.DurableBoundary) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.needsStaleVerification() && !o.mutated && boundary.Kind == engine.BoundaryFinalization && boundary.ID == store.IntegrationVerificationReady {
		if err := mutateVerifiedWorktree(o.repo, boundary.IssueID); err != nil {
			return err
		}
		o.mutated = true
		return engine.InterruptAfterCommit(errHarnessRestart)
	}
	if boundary.Kind == o.kind && strings.TrimSpace(boundary.ID) == strings.TrimSpace(o.id) &&
		(strings.TrimSpace(o.stage) == "" || strings.TrimSpace(boundary.Stage) == strings.TrimSpace(o.stage)) {
		o.fired = true
		if o.needsStaleVerification() {
			return nil
		}
		if o.scenario.Kind == recoverymatrix.ScenarioFailure {
			return failureBoundaryError(o.scenario.References.FailureFamily)
		}
		return engine.InterruptAfterCommit(errHarnessRestart)
	}
	return nil
}

func (o *boundaryInterruptObserver) needsStaleVerification() bool {
	return o.scenario.References.FinalizationBoundary == store.IntegrationPendingReverification ||
		o.scenario.References.FinalizationBoundary == store.IntegrationReverificationFailed
}

func (o *boundaryInterruptObserver) wasFired() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.fired
}

func (o *boundaryInterruptObserver) preparedStaleVerification() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.mutated
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

func (f *EnvironmentFactory) newInProcess(ctx context.Context, scenario recoverymatrix.Scenario, root string) (*executor, func() error, error) {
	if f == nil || len(f.ProductionFlow.Stages) == 0 {
		return nil, nil, fmt.Errorf("production flow is empty")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := validateDriver(scenario); err != nil {
		return nil, nil, err
	}
	if err := validateCrossCutInputs(scenario); err != nil {
		return nil, nil, err
	}
	ownsRoot := root == ""
	var err error
	if root == "" {
		root, err = os.MkdirTemp("", "watchtower-matrix-")
		if err != nil {
			return nil, nil, fmt.Errorf("create scenario root: %w", err)
		}
	} else if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create scenario root: %w", err)
	}
	cleanup := func() error {
		if !ownsRoot {
			return nil
		}
		return os.RemoveAll(root)
	}
	fail := func(err error) (*executor, func() error, error) {
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
	if scenario.References.FinalizationBoundary == store.IntegrationPublishPending {
		if err := initializeLocalRemote(root, repo); err != nil {
			return fail(err)
		}
	}
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fail(fmt.Errorf("create data directory: %w", err))
	}
	database, err := store.OpenWithClock(filepath.Join(dataDir, "watchtower.db"), deterministicClock(scenario.Seed))
	if err != nil {
		return fail(fmt.Errorf("open scenario store: %w", err))
	}

	scenarioFlow := flowForScenario(f.ProductionFlow, scenario)
	effects := newEffectRecorder(scenario)
	starts := make([]string, 0, len(f.ProductionFlow.Stages))
	var startsMu sync.Mutex
	storePath := filepath.Join(dataDir, "watchtower.db")
	observer := observerForScenario(scenario, repo)
	failureInjector := newFailureDriver(scenario)
	driver := &scenarioDriver{state: scenario.InitialInputs["state"]}
	configured, fakeRunner := newHarnessEngine(database, dataDir, repo, scenarioFlow, scenario, &starts, &startsMu, effects, observer, failureInjector, driver)
	matrix := levers.Preset(scenarioFlow, flow.LeverYolo)
	if _, hasPlan := matrix["plan"]; hasPlan {
		matrix["plan"] = flow.LeverRegular
	}
	id, err := configured.CreateIssue(
		"deterministic matrix scenario", "synthetic scenario", scenarioFlow.Name,
		matrix, 0, nil,
	)
	if err != nil {
		_ = database.Close()
		return fail(fmt.Errorf("create synthetic issue: %w", err))
	}
	executor := &executor{
		engine: configured, store: database, issueID: id, repo: repo,
		dataDir: dataDir, storePath: storePath,
		flow: cloneFlow(scenarioFlow), scenario: scenario, starts: &starts, startsMu: &startsMu,
		expectedStarts: expectedScriptKeys(scenarioFlow, scenario),
		effects:        effects, observer: observer, failureInjector: failureInjector,
		driver: driver, runner: fakeRunner,
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

func observerForScenario(scenario recoverymatrix.Scenario, repo string) *boundaryInterruptObserver {
	switch scenario.Kind {
	case recoverymatrix.ScenarioRestart:
		return &boundaryInterruptObserver{
			kind:     engine.BoundaryStageLifecycle,
			id:       scenario.References.DurableBoundary,
			stage:    scenario.References.Stage,
			scenario: scenario, repo: repo,
		}
	case recoverymatrix.ScenarioFinalization, recoverymatrix.ScenarioFinalizationRecovery:
		return &boundaryInterruptObserver{
			kind:     engine.BoundaryFinalization,
			id:       scenario.References.FinalizationBoundary,
			scenario: scenario, repo: repo,
		}
	case recoverymatrix.ScenarioFailure:
		if scenario.References.FailureFamily != "lifecycle" {
			return nil
		}
		return &boundaryInterruptObserver{
			kind: engine.BoundaryStageLifecycle, id: string(store.RunnerSucceeded),
			stage: scenario.References.Stage, scenario: scenario, repo: repo,
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
	observer *boundaryInterruptObserver,
	failureInjector engine.FailureInjector,
	driver *scenarioDriver,
) (*engine.Engine, *runner.FakeRunner) {
	runtimeFlow := flowForDriverState(production, scenario, driver.get())
	var configuredObserver engine.BoundaryObserver
	if observer != nil {
		configuredObserver = observer
	}
	r := &runner.FakeRunner{
		Scripts: scriptsForFlow(runtimeFlow, scenario, driver.get()),
		OnStart: func(_, stage, agent, _ string) error {
			startsMu.Lock()
			*starts = append(*starts, stage+"/"+agent)
			count := 0
			for _, key := range *starts {
				if key == stage+"/"+agent {
					count++
				}
			}
			startsMu.Unlock()
			if scenario.Kind == recoverymatrix.ScenarioChangedStateRecovery && stage == scenario.References.Stage && driver.get() == "failing-store-state" {
				database.FailNextStageResultPutForTest()
			}
			if scenario.References.FinalizationBoundary == store.IntegrationReverificationFailed && stage == "merge-verification" {
				if count == 2 && driver.get() == scenario.InitialInputs["state"] {
					return fmt.Errorf("verification recovery failed")
				}
			}
			return nil
		},
	}
	var preflightErr error
	if scenario.Kind == recoverymatrix.ScenarioFailure && scenario.References.FailureFamily == "capability" {
		preflightErr = &capability.PolicyError{Phase: "preflight", Reason: capability.ReasonProviderUnsupported}
	}
	if scenario.Kind == recoverymatrix.ScenarioConfigurationDrift && driver.get() == "unsupported-provider-config" {
		preflightErr = &capability.PolicyError{Phase: "preflight", Reason: capability.ReasonProviderUnsupported}
	}
	adapter := &stageBoundRunner{FakeRunner: r, stage: scenario.References.Stage, preflightErr: preflightErr}
	workspaceProvider := workspace.Provider(workspace.GitWorktree{Repo: repo})
	if scenario.Kind == recoverymatrix.ScenarioArtifactIdentity || driver.get() == scenario.RecoveryInputs["state"] || scenario.References.FinalizationBoundary == store.IntegrationPendingReverification ||
		scenario.References.FinalizationBoundary == store.IntegrationReverificationFailed {
		workspaceProvider = reusableWorkspace{GitWorktree: workspace.GitWorktree{Repo: repo}}
	} else if scenario.References.FinalizationBoundary == store.IntegrationCleanupNeeded {
		workspaceProvider = &recoverableWorkspace{GitWorktree: workspace.GitWorktree{Repo: repo}, failRelease: true}
	} else if scenario.Kind == recoverymatrix.ScenarioUnchangedRetryRefusal {
		workspaceProvider = retainedWorkspace{GitWorktree: workspace.GitWorktree{Repo: repo}}
	}
	configured := engine.New(engine.Config{
		Store:            database,
		Clock:            deterministicClock(scenario.Seed),
		Runner:           adapter,
		BoundaryObserver: configuredObserver,
		FailureInjector:  failureInjector,
		Pool:             slots.NewPool(1),
		Flows:            map[string]flow.Flow{runtimeFlow.Name: runtimeFlow},
		DataDir:          dataDir,
		Workspace:        workspaceProvider,
		Train: &marshal.Train{
			Repo: repo, TestCmd: driverTestCommand(scenario, driver.get()), Pull: false,
			Push:    scenario.References.FinalizationBoundary == store.IntegrationPublishPending,
			Effects: effects,
		},
		PlanReview: review.PolicySettings{
			ID: "deterministic-harness", Version: "1", Valid: true, AutoApproveRegular: true,
		},
		DecisionIdentities: deterministicDecisionIdentities(runtimeFlow),
	})
	return configured, r
}

type executor struct {
	engine          *engine.Engine
	store           *store.Store
	issueID         string
	repo            string
	dataDir         string
	storePath       string
	flow            flow.Flow
	scenario        recoverymatrix.Scenario
	starts          *[]string
	startsMu        *sync.Mutex
	expectedStarts  []string
	effects         *effectRecorder
	observer        *boundaryInterruptObserver
	failureInjector *failureDriver
	driver          *scenarioDriver
	runner          *runner.FakeRunner
	decisionID      int64
}

func (e *executor) Execute(ctx context.Context, _ recoverymatrix.Scenario) (recoverymatrix.Observation, error) {
	startErr := e.startWithSyntheticApprovals(ctx)
	if isCrossCutScenario(e.scenario.Kind) && e.scenario.Kind != recoverymatrix.ScenarioFinalizationRecovery {
		if err := e.driveCrossCutRecovery(ctx, startErr); err != nil {
			return recoverymatrix.Observation{}, err
		}
		startErr = nil
	}
	boundaryPrepared := e.observer != nil && e.observer.wasFired()
	if e.observer != nil && e.observer.needsStaleVerification() {
		boundaryPrepared = e.observer.preparedStaleVerification()
	}
	if requiresBoundaryInterruption(e.scenario) && (!boundaryPrepared || !errors.Is(startErr, errHarnessRestart)) {
		return recoverymatrix.Observation{}, recoverymatrix.InfrastructureError{
			Kind: recoverymatrix.InfrastructureFixture,
			Err:  fmt.Errorf("selected boundary %q did not interrupt execution", selectedBoundary(e.scenario)),
		}
	}
	if startErr != nil && errors.Is(startErr, errHarnessRestart) && e.observer != nil && e.observer.needsStaleVerification() {
		if err := e.driveStaleFinalizationRecovery(ctx); err != nil {
			return recoverymatrix.Observation{}, recoverymatrix.InfrastructureError{
				Kind: recoverymatrix.InfrastructureFixture,
				Err:  fmt.Errorf("recover selected finalization boundary: %w", err),
			}
		}
		startErr = nil
	}
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
		if startErr == nil {
			return recoverymatrix.Observation{}, fmt.Errorf("expected failure unexpectedly completed")
		}
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
		latest := records[len(records)-1]
		if string(latest.FailureSite) != e.scenario.References.FailureFamily {
			return recoverymatrix.Observation{}, fmt.Errorf("failure family %q recorded at site %q", e.scenario.References.FailureFamily, latest.FailureSite)
		}
		rows, err := e.store.Issues()
		if err != nil {
			return recoverymatrix.Observation{}, fmt.Errorf("read failed issue: %w", err)
		}
		row, found := findIssue(rows, e.issueID)
		if !found {
			return recoverymatrix.Observation{}, fmt.Errorf("failed issue %s is missing", e.issueID)
		}
		durable := row.State
		if integration, found, err := e.store.IssueIntegration(e.issueID); err != nil {
			return recoverymatrix.Observation{}, fmt.Errorf("read failed integration: %w", err)
		} else if found {
			durable = string(integration.State)
		}
		return recoverymatrix.Observation{
			PublicOutcome:            publicOutcome(row.State, store.IssueIntegration{}, false),
			DurableState:             durable,
			NormalizedClassification: string(latest.FailureSite),
			ArtifactIdentities:       []string{"failure-site:" + string(latest.FailureSite)},
			Effects:                  e.effects.kinds(),
			DiagnosticCheckpoints:    []string{latest.Stage},
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
	artifactIdentities := []string{"synthetic/artifact"}
	if e.scenario.Kind == recoverymatrix.ScenarioArtifactIdentity {
		identity, err := e.latestArtifactIdentity()
		if err != nil {
			return recoverymatrix.Observation{}, fmt.Errorf("read recovered artifact identity: %w", err)
		}
		artifactIdentities = []string{identity}
	}
	return recoverymatrix.Observation{
		PublicOutcome:            publicOutcome(row.State, integration, hasIntegration),
		DurableState:             durable,
		NormalizedClassification: "success",
		ArtifactIdentities:       artifactIdentities,
		Effects:                  e.effects.kinds(),
		DiagnosticCheckpoints:    stages,
	}, nil
}

func (e *executor) driveStaleFinalizationRecovery(ctx context.Context) error {
	if err := e.engine.SetLever(e.issueID, "merge-verification", flow.LeverStrict); err != nil {
		return err
	}
	err := e.retryWithSyntheticApprovals(ctx)
	if !e.observer.wasFired() {
		return fmt.Errorf("selected finalization boundary %q was not observed", e.scenario.References.FinalizationBoundary)
	}
	if e.scenario.References.FinalizationBoundary == store.IntegrationReverificationFailed {
		if err == nil {
			return fmt.Errorf("reverification failure unexpectedly succeeded")
		}
		return e.recoverFailedReverification(ctx)
	}
	if err != nil {
		return err
	}
	return e.rehydrateOnly()
}

type stageBoundRunner struct {
	*runner.FakeRunner
	stage        string
	preflightErr error
}

type recoverableWorkspace struct {
	workspace.GitWorktree
	mu          sync.Mutex
	failRelease bool
}

type retainedWorkspace struct{ workspace.GitWorktree }

func (w retainedWorkspace) Acquire(issueID string) (string, func() error, error) {
	path, _, err := w.GitWorktree.Acquire(issueID)
	return path, func() error { return nil }, err
}

type reusableWorkspace struct{ workspace.GitWorktree }

func (w reusableWorkspace) Acquire(issueID string) (string, func() error, error) {
	path := filepath.Join(w.Repo, ".worktrees", issueID)
	branch := "issue/" + issueID
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return path, func() error { return w.ReleasePath(path) }, nil
	}
	if _, err := git(w.Repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		if output, addErr := exec.Command("git", "-C", w.Repo, "worktree", "add", path, branch).CombinedOutput(); addErr != nil {
			return "", nil, fmt.Errorf("reuse worktree: %v: %s", addErr, output)
		}
		return path, func() error { return w.ReleasePath(path) }, nil
	}
	return w.GitWorktree.Acquire(issueID)
}

func (w *recoverableWorkspace) Acquire(issueID string) (string, func() error, error) {
	path, _, err := w.GitWorktree.Acquire(issueID)
	if err != nil {
		return "", nil, err
	}
	return path, func() error { return w.ReleasePath(path) }, nil
}

func (w *recoverableWorkspace) ReleasePath(path string) error {
	w.mu.Lock()
	if w.failRelease {
		w.failRelease = false
		w.mu.Unlock()
		return fmt.Errorf("deterministic worktree release failure")
	}
	w.mu.Unlock()
	return w.GitWorktree.ReleasePath(path)
}

func (r *stageBoundRunner) Preflight(ctx context.Context, request runner.PreflightRequest) (capability.EnforcementPlan, error) {
	if request.Stage == r.stage && r.preflightErr != nil {
		return capability.EnforcementPlan{}, r.preflightErr
	}
	return r.FakeRunner.Preflight(ctx, request)
}

func flowForScenario(production flow.Flow, scenario recoverymatrix.Scenario) flow.Flow {
	configured := cloneFlow(production)
	if scenario.Kind == recoverymatrix.ScenarioRestart {
		for index := range configured.Stages {
			if configured.Stages[index].Name == scenario.References.Stage {
				configured.Stages[index].Retries = 0
				if scenario.References.Stage == "execute" {
					configured.Stages[index].Artifacts = append(configured.Stages[index].Artifacts, "execute.out")
				}
			}
		}
	}
	if scenario.Kind != recoverymatrix.ScenarioFailure &&
		(!isCrossCutScenario(scenario.Kind) || scenario.Kind == recoverymatrix.ScenarioDecisionEscalation ||
			scenario.Kind == recoverymatrix.ScenarioFinalizationRecovery) {
		return configured
	}
	for index := range configured.Stages {
		if configured.Stages[index].Name == scenario.References.Stage {
			configured.Stages[index].Retries = 0
			if scenario.Kind == recoverymatrix.ScenarioArtifactIdentity {
				configured.Stages[index].Artifacts = append(configured.Stages[index].Artifacts, "artifact-identity.txt")
				configured.Stages[index].Gate = flow.GateApproveArtifact
			}
		}
	}
	return configured
}

func flowForDriverState(production flow.Flow, scenario recoverymatrix.Scenario, state string) flow.Flow {
	configured := cloneFlow(production)
	for index := range configured.Stages {
		if configured.Stages[index].Name != scenario.References.Stage {
			continue
		}
		switch scenario.Kind {
		case recoverymatrix.ScenarioConfigurationDrift, recoverymatrix.ScenarioArtifactIdentity:
			for agent := range configured.Stages[index].Agents {
				configured.Stages[index].Agents[agent].Model = state
			}
		case recoverymatrix.ScenarioCapabilityViolation:
			if state == "corrected-capability-contract" {
				configured.Stages[index].Artifacts = append(configured.Stages[index].Artifacts, "capability-recovery.txt")
			}
		}
	}
	return configured
}

func driverTestCommand(scenario recoverymatrix.Scenario, state string) []string {
	if scenario.Kind == recoverymatrix.ScenarioUnchangedRetryRefusal && state == "unchanged-verification-state" {
		return []string{"false"}
	}
	return []string{"true"}
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
	e.driver.set(e.scenario.RecoveryInputs["state"])
	e.engine, _ = newHarnessEngine(database, e.dataDir, e.repo, e.flow, e.scenario, e.starts, e.startsMu, e.effects, e.observer, e.failureInjector, e.driver)
	if err := e.engine.Rehydrate(); err != nil {
		return fmt.Errorf("rehydrate retained state: %w", err)
	}
	autoFinalization := e.scenario.Kind == recoverymatrix.ScenarioRestart && e.scenario.References.Stage == "merge-verification" &&
		(e.scenario.References.DurableBoundary == string(store.VerificationPassed) || e.scenario.References.DurableBoundary == string(store.FinalizationReady))
	completedStageRestart := e.scenario.Kind == recoverymatrix.ScenarioRestart &&
		e.scenario.References.DurableBoundary == string(store.FinalizationReady) &&
		e.scenario.References.Stage != "merge-verification"
	if completedStageRestart {
		if err := e.engine.Resume(e.issueID); err != nil && !strings.Contains(err.Error(), "unknown issue") {
			return err
		}
		return e.waitForCompletion(ctx)
	}
	if e.scenario.Kind == recoverymatrix.ScenarioRestart &&
		(e.scenario.References.DurableBoundary == string(store.GateResolved) ||
			e.scenario.References.DurableBoundary == string(store.VerificationPassed)) {
		for _, stage := range e.flow.Stages {
			if stage.Name == e.scenario.References.Stage &&
				(stage.Gate == flow.GatePlanReview || stage.Gate == flow.GateApproveArtifact) {
				return e.waitForCompletion(ctx)
			}
		}
	}
	if e.scenario.Kind == recoverymatrix.ScenarioRestart && !autoFinalization && e.scenario.References.Stage != "plan" {
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
	if err := e.retryWithSyntheticApprovals(ctx); err != nil {
		if errors.Is(err, errHarnessRestart) && e.observer != nil && e.observer.wasFired() {
			return e.resumeAfterSelectedBoundary(ctx)
		}
		if e.observer != nil && e.observer.wasFired() &&
			e.scenario.References.FinalizationBoundary == store.IntegrationReverificationFailed {
			return e.recoverFailedReverification(ctx)
		}
		if strings.Contains(err.Error(), "is already running") {
			return e.waitForCompletion(ctx)
		}
		return err
	}
	if e.observer != nil && e.observer.needsStaleVerification() {
		if !e.observer.wasFired() {
			return fmt.Errorf("selected finalization boundary %q was not observed", e.scenario.References.FinalizationBoundary)
		}
		return e.rehydrateOnly()
	}
	if integration, found, err := e.store.IssueIntegration(e.issueID); err == nil && found && integration.State == store.IntegrationCleanupNeeded {
		return e.retryWithSyntheticApprovals(ctx)
	}
	return nil
}

func (e *executor) recoverFailedReverification(ctx context.Context) error {
	integration, found, err := e.store.IssueIntegration(e.issueID)
	if err != nil {
		return err
	}
	if !found || integration.State != store.IntegrationReverificationFailed {
		return fmt.Errorf("reverification failure state was not persisted")
	}
	e.driver.set(e.scenario.RecoveryInputs["state"])
	if err := e.ensurePersistedWorktree(); err != nil {
		return err
	}
	if err := e.engine.SetLever(e.issueID, "merge-verification", flow.LeverRegular); err != nil {
		return err
	}
	if err := e.retryWithSyntheticApprovals(ctx); err != nil {
		return err
	}
	return e.rehydrateOnly()
}

func (e *executor) resumeAfterSelectedBoundary(ctx context.Context) error {
	if err := e.store.Close(); err != nil {
		return fmt.Errorf("close interrupted recovery store: %w", err)
	}
	database, err := store.OpenWithClock(e.storePath, deterministicClock(e.scenario.Seed))
	if err != nil {
		return fmt.Errorf("reopen interrupted recovery store: %w", err)
	}
	e.store = database
	e.driver.set(e.scenario.RecoveryInputs["state"])
	e.engine, _ = newHarnessEngine(database, e.dataDir, e.repo, e.flow, e.scenario, e.starts, e.startsMu, e.effects, nil, e.failureInjector, e.driver)
	if err := e.engine.Rehydrate(); err != nil {
		return fmt.Errorf("rehydrate interrupted recovery state: %w", err)
	}
	if err := e.retryWithSyntheticApprovals(ctx); err != nil {
		if strings.Contains(err.Error(), "is already running") {
			return e.waitForCompletion(ctx)
		}
		return err
	}
	return nil
}

func (e *executor) driveCrossCutRecovery(ctx context.Context, startErr error) error {
	initial, initialOK := e.scenario.InitialInputs["state"]
	recovery, recoveryOK := e.scenario.RecoveryInputs["state"]
	if !initialOK || !recoveryOK || initial == "" || recovery == "" || initial == recovery {
		return recoverymatrix.InfrastructureError{Kind: recoverymatrix.InfrastructureFixture,
			Err: fmt.Errorf("cross-cutting driver requires distinct initial and recovery state")}
	}
	if e.scenario.Kind == recoverymatrix.ScenarioDecisionEscalation {
		if !errors.Is(startErr, errHarnessDecisionPending) {
			return fmt.Errorf("decision escalation did not interrupt with a pending decision: %w", startErr)
		}
		rows, err := e.store.AllDecisionRows()
		if err != nil {
			return fmt.Errorf("read escalated decisions: %w", err)
		}
		for _, row := range rows {
			if row.ID == e.decisionID && row.Stage == e.scenario.References.Stage && row.Status == "pending" && row.Response.Option == nil {
				if err := e.rebuildForCrossCutRecovery(ctx); err != nil {
					return err
				}
				resolved, err := e.store.AllDecisionRows()
				if err != nil {
					return fmt.Errorf("read recovered decision: %w", err)
				}
				for _, recovered := range resolved {
					if recovered.ID == e.decisionID && recovered.Stage == e.scenario.References.Stage && recovered.Status == "answered" && recovered.Response.Option != nil && *recovered.Response.Option == 0 {
						return nil
					}
				}
				return fmt.Errorf("fresh runtime did not resolve decision %d", e.decisionID)
			}
		}
		return fmt.Errorf("decision escalation produced no pending durable decision for %q", e.scenario.References.Stage)
	}
	if startErr == nil {
		return fmt.Errorf("cross-cutting initial state %q unexpectedly completed", initial)
	}
	if e.scenario.Kind == recoverymatrix.ScenarioArtifactIdentity {
		if !strings.Contains(startErr.Error(), "artifacts require revision") {
			return fmt.Errorf("artifact identity was not rejected through review: %w", startErr)
		}
		if err := e.verifyRejectedArtifactIdentity(initial); err != nil {
			return err
		}
	}
	if e.scenario.Kind == recoverymatrix.ScenarioUnchangedRetryRefusal {
		e.startsMu.Lock()
		before := len(*e.starts)
		e.startsMu.Unlock()
		err := e.retryWithSyntheticApprovals(ctx)
		e.startsMu.Lock()
		after := len(*e.starts)
		e.startsMu.Unlock()
		if err == nil || after != before {
			return fmt.Errorf("unchanged retry was not refused: %v", err)
		}
	}
	if e.scenario.Kind == recoverymatrix.ScenarioArtifactIdentity {
		e.driver.set(recovery)
		e.runner.Scripts = scriptsForFlow(flowForDriverState(e.flow, e.scenario, recovery), e.scenario, recovery)
		return e.retryWithSyntheticApprovals(ctx)
	}
	if e.scenario.Kind != recoverymatrix.ScenarioDecisionEscalation {
		e.driver.set(recovery)
		lever := flow.LeverStrict
		if e.scenario.References.Stage == "plan" {
			lever = flow.LeverYolo
		}
		if err := e.engine.SetLever(e.issueID, e.scenario.References.Stage, lever); err != nil {
			return fmt.Errorf("apply recovery configuration %q: %w", recovery, err)
		}
	}
	return e.rebuildForCrossCutRecovery(ctx)
}

type persistedArtifactIdentity struct {
	attempt string
	content string
	digest  string
}

func (e *executor) persistedArtifactIdentities() ([]persistedArtifactIdentity, error) {
	records, err := e.store.StageLifecycleRecords(e.issueID, e.scenario.References.Stage, "")
	if err != nil {
		return nil, err
	}
	var identities []persistedArtifactIdentity
	for _, record := range records {
		if !record.Committed || record.Substate != stagelifecycle.ArtifactsArchived {
			continue
		}
		for _, artifact := range record.Artifacts {
			if artifact.Name != "artifact-identity.txt" {
				continue
			}
			body, err := os.ReadFile(filepath.Join(e.dataDir, e.issueID, filepath.FromSlash(artifact.Path)))
			if err != nil {
				return nil, err
			}
			digest := sha256.Sum256(body)
			encoded := hex.EncodeToString(digest[:])
			if encoded != artifact.SHA256 {
				return nil, fmt.Errorf("persisted artifact %s has digest %s, want %s", record.AttemptID, encoded, artifact.SHA256)
			}
			identities = append(identities, persistedArtifactIdentity{
				attempt: record.AttemptID, content: string(body), digest: "sha256:" + encoded,
			})
		}
	}
	return identities, nil
}

func (e *executor) latestArtifactIdentity() (string, error) {
	identities, err := e.persistedArtifactIdentities()
	if err != nil {
		return "", err
	}
	if len(identities) == 0 {
		return "", fmt.Errorf("no durable artifact identity")
	}
	latest := identities[0]
	for _, candidate := range identities[1:] {
		if harnessAttemptAfter(candidate.attempt, latest.attempt) {
			latest = candidate
		}
	}
	return latest.digest, nil
}

func (e *executor) verifyRejectedArtifactIdentity(initial string) error {
	identities, err := e.persistedArtifactIdentities()
	if err != nil {
		return err
	}
	if len(identities) != 1 || identities[0].content != initial {
		return fmt.Errorf("rejected artifact identity was not durably archived: %+v", identities)
	}
	reviews, err := e.store.ArtifactReviewRows(e.issueID)
	if err != nil {
		return err
	}
	for _, row := range reviews {
		if row.Stage == e.scenario.References.Stage && row.Status == "answered" && row.Response.Option != nil && *row.Response.Option == 1 {
			return nil
		}
	}
	return fmt.Errorf("durable artifact identity was not rejected through artifact review")
}

func validateArtifactReviewIdentity(pending engine.PendingDecision, content string) error {
	if pending.Review == nil {
		return fmt.Errorf("artifact identity review target is missing")
	}
	digest := sha256.Sum256([]byte(content))
	want := hex.EncodeToString(digest[:])
	for _, artifact := range pending.Review.Artifacts {
		if artifact.Name == "artifact-identity.txt" && artifact.SHA256 == want {
			return nil
		}
	}
	return fmt.Errorf("artifact review target does not bind the persisted identity %s", want)
}

func harnessAttemptAfter(left, right string) bool {
	leftNumber, leftErr := strconv.ParseInt(strings.TrimPrefix(left, "checkpoint-"), 10, 64)
	rightNumber, rightErr := strconv.ParseInt(strings.TrimPrefix(right, "checkpoint-"), 10, 64)
	if leftErr == nil && rightErr == nil {
		return leftNumber > rightNumber
	}
	return left > right
}

func (e *executor) rebuildForCrossCutRecovery(ctx context.Context) error {
	if err := e.ensurePersistedWorktree(); err != nil {
		return err
	}
	if err := e.store.Close(); err != nil {
		return fmt.Errorf("close initial cross-cutting store: %w", err)
	}
	database, err := store.OpenWithClock(e.storePath, deterministicClock(e.scenario.Seed))
	if err != nil {
		return fmt.Errorf("reopen cross-cutting store: %w", err)
	}
	e.store = database
	if e.scenario.Kind == recoverymatrix.ScenarioDecisionEscalation {
		recovery := e.scenario.RecoveryInputs["state"]
		if recovery != "approved-artifact-decision" {
			return fmt.Errorf("unsupported decision recovery input %q", recovery)
		}
		e.driver.set(recovery)
	}
	e.engine, _ = newHarnessEngine(database, e.dataDir, e.repo, e.flow, e.scenario, e.starts, e.startsMu, e.effects, nil, e.failureInjector, e.driver)
	if err := e.engine.Rehydrate(); err != nil {
		return fmt.Errorf("rehydrate cross-cutting state: %w", err)
	}
	if e.scenario.Kind == recoverymatrix.ScenarioDecisionEscalation {
		for _, pending := range e.engine.PendingDecisions() {
			if pending.ID != e.decisionID || pending.Stage != e.scenario.References.Stage {
				continue
			}
			if err := e.engine.Answer(pending.ID, levers.ChoiceResponse(0)); err != nil {
				return fmt.Errorf("resolve recovered decision %d: %w", pending.ID, err)
			}
			return e.waitForCompletion(ctx)
		}
		return fmt.Errorf("fresh runtime did not rehydrate pending decision %d", e.decisionID)
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
	e.engine, _ = newHarnessEngine(database, e.dataDir, e.repo, e.flow, e.scenario, e.starts, e.startsMu, e.effects, nil, e.failureInjector, e.driver)
	if err := e.engine.Rehydrate(); err != nil {
		return fmt.Errorf("rehydrate terminal state: %w", err)
	}
	return nil
}

func (e *executor) waitForCompletion(ctx context.Context) error {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		for _, pending := range e.engine.PendingDecisions() {
			if err := e.engine.Answer(pending.ID, levers.ChoiceResponse(0)); err != nil {
				return fmt.Errorf("answer synthetic recovery decision %d: %w", pending.ID, err)
			}
		}
		if integration, found, err := e.store.IssueIntegration(e.issueID); err != nil {
			return err
		} else if found && integration.State == store.IntegrationMerged {
			return nil
		} else if found && integration.State == store.IntegrationReverificationFailed {
			return fmt.Errorf("reverification failed: %s", integration.LastError)
		}
		rows, err := e.store.Issues()
		if err != nil {
			return err
		}
		if row, found := findIssue(rows, e.issueID); found {
			if row.State == "merged" || row.State == "done" || row.State == "done (unmerged)" {
				return nil
			}
			if row.State == "failed" {
				return fmt.Errorf("recovery issue failed")
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			integration, _, _ := e.store.IssueIntegration(e.issueID)
			e.startsMu.Lock()
			started := append([]string(nil), (*e.starts)...)
			e.startsMu.Unlock()
			events, _ := e.store.EventsSince(0)
			lastFailure := ""
			for _, event := range events {
				if event.IssueID == e.issueID && event.Type == core.EvStageFailed {
					lastFailure = string(event.Payload)
				}
			}
			return fmt.Errorf("%w while integration is %s with %d pending decisions after %v; last failure %s", ctx.Err(), integration.State, len(e.engine.PendingDecisions()), started, lastFailure)
		}
	}
}

func (e *executor) ensurePersistedWorktree() error {
	integration, found, err := e.store.IssueIntegration(e.issueID)
	if err != nil {
		return err
	}
	worktree, branch := integration.Worktree, integration.Branch
	if !found || worktree == "" || branch == "" {
		run, runFound, runErr := e.store.LoadRunState(e.issueID)
		if runErr != nil {
			return runErr
		}
		if runFound && run.Worktree != "" && run.Branch != "" {
			worktree, branch = run.Worktree, run.Branch
		} else {
			runs, runsErr := e.store.StageRuns(e.issueID)
			if runsErr != nil {
				return runsErr
			}
			for index := len(runs) - 1; index >= 0; index-- {
				if runs[index].Worktree != "" {
					worktree, branch = runs[index].Worktree, "issue/"+e.issueID
					break
				}
			}
			if worktree == "" {
				return nil
			}
		}
	}
	if _, err := os.Stat(worktree); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect persisted worktree: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		return fmt.Errorf("create persisted worktree parent: %w", err)
	}
	ref := "refs/heads/" + branch
	if output, err := exec.Command("git", "-C", e.repo, "show-ref", "--verify", "--quiet", ref).CombinedOutput(); err != nil {
		if branchOutput, branchErr := exec.Command("git", "-C", e.repo, "branch", branch, "HEAD").CombinedOutput(); branchErr != nil {
			return fmt.Errorf("restore persisted branch: %v: %s (ref check: %v: %s)", branchErr, branchOutput, err, output)
		}
	}
	cmd := exec.Command("git", "-C", e.repo, "worktree", "add", worktree, branch)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("restore persisted worktree: %v: %s", err, output)
	}
	return nil
}

func (e *executor) retryWithSyntheticApprovals(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
	if err := ctx.Err(); err != nil {
		return err
	}
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
				response := levers.ChoiceResponse(0)
				if e.scenario.Kind == recoverymatrix.ScenarioDecisionEscalation && e.driver.get() == "pending-artifact-decision" && pending.Stage == e.scenario.References.Stage {
					e.decisionID = pending.ID
					if err := e.engine.InterruptPendingDecision(e.issueID); err != nil {
						return fmt.Errorf("interrupt pending decision %d: %w", pending.ID, err)
					}
					select {
					case runErr := <-done:
						if !errors.Is(runErr, context.Canceled) {
							return fmt.Errorf("join interrupted initial execution: %w", runErr)
						}
					case <-ctx.Done():
						return fmt.Errorf("join interrupted initial execution: %w", ctx.Err())
					}
					return errHarnessDecisionPending
				}
				if e.scenario.Kind == recoverymatrix.ScenarioArtifactIdentity && e.driver.get() == e.scenario.InitialInputs["state"] && pending.Stage == e.scenario.References.Stage {
					if err := validateArtifactReviewIdentity(pending, e.scenario.InitialInputs["state"]); err != nil {
						return err
					}
					response = levers.ChoiceResponse(1)
				}
				if err := e.engine.Answer(pending.ID, response); err != nil {
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
	if e.scenario.References.FinalizationBoundary == store.IntegrationPublishPending {
		successful := e.effects.successful(marshal.EffectPublish)
		rejected := e.effects.wasRejected()
		if !rejected || successful != 1 {
			return recoverymatrix.InfrastructureError{Kind: recoverymatrix.InfrastructureUnconsumed,
				Err: fmt.Errorf("publish recovery rejected=%t successful=%d", rejected, successful)}
		}
	}
	if e.scenario.Kind == recoverymatrix.ScenarioFailure {
		if isInjectedFailureFamily(e.scenario.References.FailureFamily) && !e.failureInjector.wasFired() {
			return recoverymatrix.InfrastructureError{Kind: recoverymatrix.InfrastructureUnconsumed,
				Err: fmt.Errorf("failure driver did not reach %s boundary for stage %q", e.scenario.References.FailureFamily, e.scenario.References.Stage)}
		}
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
		if scenario.References.FailureFamily == "capability" || isInjectedFailureFamily(scenario.References.FailureFamily) {
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
		if scenario.References.FailureFamily == "capability" || isInjectedFailureFamily(scenario.References.FailureFamily) {
			return nil
		}
		return recoverymatrix.InfrastructureError{Kind: recoverymatrix.InfrastructureUnconsumed,
			Err: fmt.Errorf("failure driver did not reach stage %q", scenario.References.Stage)}
	}
	return nil
}

func isInjectedFailureFamily(family string) bool {
	switch failure.Site(family) {
	case failure.SiteWorkspace, failure.SiteArtifact, failure.SitePlanner, failure.SiteGit,
		failure.SiteVerification, failure.SiteCache, failure.SiteStore, failure.SiteFinalization:
		return true
	default:
		return false
	}
}

type effectRecorder struct {
	mu         sync.Mutex
	admit      []marshal.Effect
	completed  []marshal.Effect
	rejectKind marshal.EffectKind
	rejected   bool
}

func newEffectRecorder(scenario recoverymatrix.Scenario) *effectRecorder {
	recorder := &effectRecorder{}
	switch scenario.References.FinalizationBoundary {
	case store.IntegrationPublishPending:
		recorder.rejectKind = marshal.EffectPublish
	}
	return recorder
}

func (r *effectRecorder) Admit(effect marshal.Effect) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if effect.Kind == marshal.EffectSyncBase {
		return fmt.Errorf("offline harness rejected effect %q", effect.Kind)
	}
	if effect.Kind == r.rejectKind && !r.rejected {
		r.rejected = true
		return fmt.Errorf("offline harness injected %q recovery boundary", effect.Kind)
	}
	r.admit = append(r.admit, effect)
	return nil
}

func (r *effectRecorder) Complete(effect marshal.Effect, err error) {
	if err != nil {
		return
	}
	r.mu.Lock()
	r.completed = append(r.completed, effect)
	r.mu.Unlock()
}

func (r *effectRecorder) successful(kind marshal.EffectKind) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, effect := range r.completed {
		if effect.Kind == kind {
			count++
		}
	}
	return count
}

func (r *effectRecorder) wasRejected() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rejected
}

func (r *effectRecorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.admit))
	for _, effect := range r.admit {
		out = append(out, string(effect.Kind))
	}
	return out
}

func scriptsForFlow(production flow.Flow, scenario recoverymatrix.Scenario, state string) map[string]runner.Script {
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
			if stage.Name == "execute" {
				script.StageEvidence = &stageresult.Evidence{
					SchemaVersion: stageresult.SchemaVersion, StageKind: stageresult.KindExecute,
					Outcome: stageresult.OutcomeCompleted,
					Execute: &stageresult.ExecutePayload{
						PlanTasks: []stageresult.PlanTask{{ID: "task-0001", Outcome: stageresult.TaskCompleted, Summary: "completed deterministic task"}},
						Skips: []stageresult.Skip{
							{Activity: "commits", Explanation: "deterministic harness makes no source commit"},
							{Activity: "checks", Explanation: "deterministic harness uses the repository gate"},
						},
					},
				}
			}
			if scenario.Kind == recoverymatrix.ScenarioFailure && stage.Name == scenario.References.Stage {
				script = failureScript(stage, script, scenario.References.FailureFamily)
			}
			if stage.Name == scenario.References.Stage {
				switch scenario.Kind {
				case recoverymatrix.ScenarioUnchangedRetryRefusal:
					if state == "unchanged-verification-state" {
						script.Fail = true
						script.FailureClass = runner.FailureConfiguration
					}
				case recoverymatrix.ScenarioPlannerCapabilityLoss:
					if state == "planner-authority-lost" {
						script.PlannerFailureAt = 1
						script.PlannerFailure = fmt.Errorf("planner authority unavailable")
					}
				case recoverymatrix.ScenarioCapabilityViolation:
					operation := runner.OperationAttempt{Operation: capability.OpWorkspaceMutate, Mutation: capability.MutationCreate}
					if state == "denied-capability-contract" {
						operation.Path = "outside-capability-contract.txt"
					} else if state == "corrected-capability-contract" {
						operation.Path = "capability-recovery.txt"
						operation.Content = "recovered\n"
					}
					if operation.Path != "" {
						script.OperationAttempts = []runner.OperationAttempt{operation}
					}
				}
			}
			if scenario.Kind == recoverymatrix.ScenarioArtifactIdentity && stage.Name == scenario.References.Stage {
				if script.Artifacts == nil {
					script.Artifacts = map[string]string{}
				}
				script.Artifacts["artifact-identity.txt"] = state
			}
			for _, name := range stage.Artifacts {
				if script.Artifacts == nil {
					script.Artifacts = map[string]string{}
				}
				if _, exists := script.Artifacts[name]; !exists {
					script.Artifacts[name] = "deterministic artifact\n"
				}
			}
			scripts[stage.Name+"/"+agent.Package] = script
		}
	}
	return scripts
}

func isCrossCutScenario(kind recoverymatrix.ScenarioKind) bool {
	switch kind {
	case recoverymatrix.ScenarioUnchangedRetryRefusal, recoverymatrix.ScenarioChangedStateRecovery,
		recoverymatrix.ScenarioPlannerCapabilityLoss, recoverymatrix.ScenarioConfigurationDrift,
		recoverymatrix.ScenarioCapabilityViolation, recoverymatrix.ScenarioDecisionEscalation,
		recoverymatrix.ScenarioArtifactIdentity, recoverymatrix.ScenarioFinalizationRecovery:
		return true
	default:
		return false
	}
}

func failureScript(stage flow.Stage, script runner.Script, family string) runner.Script {
	switch family {
	case "runner":
		script.Fail = true
	}
	return script
}

func failureBoundaryError(family string) error {
	switch family {
	case "lifecycle":
		return &stagelifecycle.DiagnosticError{Code: stagelifecycle.CodeIntegrity, Message: "deterministic lifecycle failure"}
	case "capability":
		return &capability.PolicyError{Phase: "post-stage", Reason: capability.ReasonPostStageViolation}
	default:
		return fmt.Errorf("deterministic %s failure", family)
	}
}

func requiresBoundaryInterruption(scenario recoverymatrix.Scenario) bool {
	return scenario.Kind == recoverymatrix.ScenarioRestart || scenario.Kind == recoverymatrix.ScenarioFinalization ||
		scenario.Kind == recoverymatrix.ScenarioFinalizationRecovery
}

func selectedBoundary(scenario recoverymatrix.Scenario) string {
	if scenario.References.FinalizationBoundary != "" {
		return scenario.References.FinalizationBoundary
	}
	return scenario.References.DurableBoundary
}

func mutateVerifiedWorktree(repo, issueID string) error {
	list, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return fmt.Errorf("list verified worktrees: %w", err)
	}
	var worktree string
	found := false
	for _, line := range strings.Split(string(list), "\n") {
		if strings.HasPrefix(line, "worktree ") {
			worktree = strings.TrimPrefix(line, "worktree ")
			continue
		}
		if line == "branch refs/heads/issue/"+issueID && worktree != "" {
			found = true
			break
		}
	}
	if !found || worktree == "" || worktree == repo {
		return fmt.Errorf("verified worktree for %s is unavailable", issueID)
	}
	path := filepath.Join(worktree, "matrix-verification-change")
	if err := os.WriteFile(path, []byte("changed\n"), 0o644); err != nil {
		return fmt.Errorf("write deterministic verification change: %w", err)
	}
	if _, err := git(worktree, "add", "matrix-verification-change"); err != nil {
		return err
	}
	if _, err := git(worktree, "commit", "-m", "deterministic verification change"); err != nil {
		return err
	}
	return nil
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

func validateCrossCutInputs(scenario recoverymatrix.Scenario) error {
	expected := map[recoverymatrix.ScenarioKind][2]string{
		recoverymatrix.ScenarioUnchangedRetryRefusal: {"unchanged-verification-state", "changed-verification-state"},
		recoverymatrix.ScenarioChangedStateRecovery:  {"failing-store-state", "repaired-configuration-state"},
		recoverymatrix.ScenarioPlannerCapabilityLoss: {"planner-authority-lost", "planner-authority-restored"},
		recoverymatrix.ScenarioConfigurationDrift:    {"unsupported-provider-config", "supported-provider-config"},
		recoverymatrix.ScenarioCapabilityViolation:   {"denied-capability-contract", "corrected-capability-contract"},
		recoverymatrix.ScenarioDecisionEscalation:    {"pending-artifact-decision", "approved-artifact-decision"},
		recoverymatrix.ScenarioArtifactIdentity:      {"artifact-v1", "artifact-v2"},
	}
	want, ok := expected[scenario.Kind]
	if !ok {
		return nil
	}
	initial, recovery := scenario.InitialInputs["state"], scenario.RecoveryInputs["state"]
	if initial != want[0] || recovery != want[1] {
		return fmt.Errorf("driver %s requires state transition %q to %q", scenario.Kind, want[0], want[1])
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
	if isCrossCutScenario(scenario.Kind) && scenario.Kind != recoverymatrix.ScenarioDecisionEscalation &&
		scenario.Kind != recoverymatrix.ScenarioFinalizationRecovery && scenario.Kind != recoverymatrix.ScenarioConfigurationDrift {
		for _, stage := range production.Stages {
			if stage.Name == scenario.References.Stage {
				for _, agent := range stage.Agents {
					keys = append(keys, stage.Name+"/"+agent.Package)
				}
			}
		}
	}
	if scenario.References.FinalizationBoundary == store.IntegrationPendingReverification ||
		scenario.References.FinalizationBoundary == store.IntegrationReverificationFailed {
		for _, stage := range production.Stages {
			if stage.Name != "merge-verification" {
				continue
			}
			for _, agent := range stage.Agents {
				keys = append(keys, stage.Name+"/"+agent.Package)
				if scenario.References.FinalizationBoundary == store.IntegrationReverificationFailed {
					keys = append(keys, stage.Name+"/"+agent.Package)
				}
			}
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

func initializeLocalRemote(root, repo string) error {
	remote := filepath.Join(root, "origin.git")
	if output, err := exec.Command("git", "init", "--bare", remote).CombinedOutput(); err != nil {
		return fmt.Errorf("initialize local remote: %v: %s", err, output)
	}
	if _, err := git(repo, "remote", "add", "origin", remote); err != nil {
		return err
	}
	if _, err := git(repo, "push", "-q", "-u", "origin", "main"); err != nil {
		return err
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
