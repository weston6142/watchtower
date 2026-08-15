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
	r := &runner.FakeRunner{Scripts: scriptsForFlow(f.ProductionFlow), OnStart: func(_, stage, agent, _ string) error {
		startsMu.Lock()
		starts = append(starts, stage+"/"+agent)
		startsMu.Unlock()
		return nil
	}}
	configured := engine.New(engine.Config{
		Store:     database,
		Clock:     deterministicClock(scenario.Seed),
		Runner:    r,
		Pool:      slots.NewPool(1),
		Flows:     map[string]flow.Flow{f.ProductionFlow.Name: cloneFlow(f.ProductionFlow)},
		DataDir:   dataDir,
		Workspace: workspace.GitWorktree{Repo: repo},
		Train: &marshal.Train{
			Repo: repo, TestCmd: []string{"true"}, Pull: false, Push: false,
			Effects: effects,
		},
		PlanReview: review.PolicySettings{
			ID: "deterministic-harness", Version: "1", Valid: true, AutoApproveRegular: true,
		},
		DecisionIdentities: deterministicDecisionIdentities(f.ProductionFlow),
	})
	id, err := configured.CreateIssue(
		"deterministic matrix scenario", "synthetic scenario", f.ProductionFlow.Name,
		levers.Preset(f.ProductionFlow, flow.LeverYolo), 0, nil,
	)
	if err != nil {
		_ = database.Close()
		return fail(fmt.Errorf("create synthetic issue: %w", err))
	}
	return &executor{
			engine: configured, store: database, issueID: id, repo: repo,
			flow: cloneFlow(f.ProductionFlow), starts: &starts, startsMu: &startsMu,
			expectedStarts: expectedScriptKeys(f.ProductionFlow),
			effects:        effects,
		}, func() error {
			if err := database.Close(); err != nil {
				_ = cleanup()
				return err
			}
			return cleanup()
		}, nil
}

type executor struct {
	engine         *engine.Engine
	store          *store.Store
	issueID        string
	repo           string
	flow           flow.Flow
	starts         *[]string
	startsMu       *sync.Mutex
	expectedStarts []string
	effects        *effectRecorder
}

func (e *executor) Execute(ctx context.Context, _ recoverymatrix.Scenario) (recoverymatrix.Observation, error) {
	if err := e.engine.StartIssue(ctx, e.issueID); err != nil {
		return recoverymatrix.Observation{}, recoverymatrix.InfrastructureError{
			Kind: recoverymatrix.InfrastructureFixture, Err: err,
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

func (e *executor) VerifyConsumed() error {
	e.startsMu.Lock()
	started := append([]string(nil), (*e.starts)...)
	e.startsMu.Unlock()
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

func scriptsForFlow(production flow.Flow) map[string]runner.Script {
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
			case "merge-verification":
				script.Artifacts = map[string]string{
					"merge-report.md":     "verified\n",
					"merge-decision.json": "",
					"verification.json":   "",
				}
			}
			scripts[stage.Name+"/"+agent.Package] = script
		}
	}
	return scripts
}

func expectedScriptKeys(production flow.Flow) []string {
	var keys []string
	for _, stage := range production.Stages {
		for _, agent := range stage.Agents {
			keys = append(keys, stage.Name+"/"+agent.Package)
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
var _ = errors.Is

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
