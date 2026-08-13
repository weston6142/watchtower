package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/store"
	"github.com/weston6142/watchtower/internal/workspace"
)

func TestPolicyViolationQuarantinesWorkspaceAndBlocksProgress(t *testing.T) {
	for _, mode := range []string{"runtime", "post-stage"} {
		t.Run(mode, func(t *testing.T) {
			fixture := runRecoveredPolicyStage(t, mode, 1)
			if fixture.starts.Load() != 2 {
				t.Fatalf("provider starts = %d, want rejected attempt plus recovered retry", fixture.starts.Load())
			}
			if _, err := os.Stat(filepath.Join(fixture.workdir, "outside.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected bytes survived recovery: %v", err)
			}
			if got := strings.TrimSpace(gitOutput(t, fixture.workdir, "status", "--porcelain")); got != "?? ISSUE.md\n?? STAGE.md\n?? decisions.md" {
				t.Fatalf("recovered stage workspace = %q", got)
			}
			attempts, err := fixture.store.StageLifecycleAttempts(fixture.issueID, fixture.stage.Name)
			if err != nil || len(attempts) != 2 {
				t.Fatalf("attempts = %+v, err=%v", attempts, err)
			}
			first, found, err := fixture.store.CapabilityAttempt(fixture.issueID, fixture.stage.Name, attempts[0].AttemptID)
			if err != nil || !found || first.Validation.Passed {
				t.Fatalf("rejected capability record = %+v, found=%t err=%v", first, found, err)
			}
			if _, found, err := fixture.store.StageLifecycleResult(attempts[0]); err != nil || found {
				t.Fatalf("rejected result crossed acceptance boundary: found=%t err=%v", found, err)
			}
			audit, err := fixture.store.CapabilityAudit(fixture.issueID, fixture.stage.Name, attempts[0].AttemptID)
			if err != nil || !auditHasPhases(audit, "quarantine", "recovery") {
				t.Fatalf("recovery audit = %+v, err=%v", audit, err)
			}
		})
	}
}

func TestPolicyViolationQuarantineSurvivesRefTampering(t *testing.T) {
	fixture := newPolicyRecoveryFixture(t, "none", 0)
	baseline, err := (capability.Observer{}).Capture(fixture.workdir, nil)
	if err != nil {
		t.Fatal(err)
	}
	engineGit(t, fixture.workdir, "checkout", "--detach")
	if err := os.WriteFile(filepath.Join(fixture.workdir, "tampered.txt"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	engineGit(t, fixture.workdir, "add", "tampered.txt")
	engineGit(t, fixture.workdir, "commit", "-m", "tampered detached head")
	identity := capability.AttemptIdentity{IssueID: fixture.issueID, Stage: fixture.stage.Name, AttemptID: "checkpoint-1"}
	cause := &capability.PolicyError{Phase: "post-stage", Reason: capability.ReasonPostStageViolation}
	if err := fixture.engine.markCapabilityWorkspaceRejected(
		fixture.engine.issues[fixture.issueID], fixture.stage.Name, identity,
		capability.CompiledContract{ContractID: "contract", AuthorityDigest: "authority"}, baseline, cause,
	); err != nil {
		t.Fatalf("quarantine failed after ref tampering: %v", err)
	}
	integration, found, err := fixture.store.IssueIntegration(fixture.issueID)
	if err != nil || !found || integration.State != capabilityRecoveryNeeded {
		t.Fatalf("durable quarantine=%+v found=%t err=%v", integration, found, err)
	}
}

func TestPolicyRetryUsesNewContractWithSameAuthority(t *testing.T) {
	fixture := runRecoveredPolicyStage(t, "post-stage", 1)
	attempts, err := fixture.store.StageLifecycleAttempts(fixture.issueID, fixture.stage.Name)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("attempts = %+v, err=%v", attempts, err)
	}
	first, _, _ := fixture.store.CapabilityAttempt(fixture.issueID, fixture.stage.Name, attempts[0].AttemptID)
	second, _, _ := fixture.store.CapabilityAttempt(fixture.issueID, fixture.stage.Name, attempts[1].AttemptID)
	if first.Contract.ContractID == second.Contract.ContractID {
		t.Fatal("retry reused the rejected attempt contract")
	}
	if first.Contract.AuthorityDigest == "" || first.Contract.AuthorityDigest != second.Contract.AuthorityDigest {
		t.Fatalf("authority widened across recovery: first=%s second=%s", first.Contract.AuthorityDigest, second.Contract.AuthorityDigest)
	}
}

func TestRestartRequiresBoundValidationOrRecovery(t *testing.T) {
	fixture := newPolicyRecoveryFixture(t, "runtime", 0)
	err := fixture.engine.runStageOnce(context.Background(), fixture.engine.issues[fixture.issueID], fixture.stage, 1, 1, nil)
	if _, ok := capabilityRecoveryReason(err); !ok {
		t.Fatalf("stage error = %v", err)
	}
	integration, found, loadErr := fixture.store.IssueIntegration(fixture.issueID)
	if loadErr != nil || !found || integration.State != capabilityRecoveryNeeded {
		t.Fatalf("durable quarantine = %+v found=%t err=%v", integration, found, loadErr)
	}

	restarted := New(fixture.engine.cfg)
	restartedIssue := &issueState{
		id: fixture.issueID, wsPath: integration.Worktree, branch: integration.Branch,
		baseRef: fixture.engine.issues[fixture.issueID].baseRef,
	}
	if recovered, err := restarted.recoverPendingCapabilityWorkspace(context.Background(), restartedIssue); err != nil || !recovered {
		t.Fatal(err)
	}
	integration, found, loadErr = fixture.store.IssueIntegration(fixture.issueID)
	if loadErr != nil || !found || integration.State != store.IntegrationClaimed {
		t.Fatalf("restarted recovery = %+v found=%t err=%v", integration, found, loadErr)
	}
	if got := strings.TrimSpace(gitOutput(t, restartedIssue.wsPath, "status", "--porcelain")); got != "" {
		t.Fatalf("restart recovered dirty workspace: %q", got)
	}
}

func TestPolicyFailureProjectionIsStableAndRedacted(t *testing.T) {
	fixture := newPolicyRecoveryFixture(t, "runtime", 0)
	fixture.runner.OnStart = func(_, _, _, workdir string) error {
		if err := os.WriteFile(filepath.Join(workdir, "raw-path-sentinel"), []byte("secret-content-sentinel"), 0o644); err != nil {
			return err
		}
		return &capability.PolicyError{
			Phase: "runtime", Reason: capability.ReasonRuntimeDenied,
			Paths: []string{"raw-path-sentinel"}, Diagnostic: "prompt credential command secret-content-sentinel",
		}
	}
	err := fixture.engine.runStage(context.Background(), fixture.engine.issues[fixture.issueID], fixture.stage, nil)
	if err == nil {
		t.Fatal("policy violation succeeded")
	}
	history, historyErr := fixture.store.FailureHistory(context.Background(), fixture.issueID)
	if historyErr != nil || len(history) == 0 {
		t.Fatalf("policy failure history = %+v err=%v", history, historyErr)
	}
	latest := history[len(history)-1]
	if latest.FailureSite != failure.SiteCapability || latest.FailureClass != failure.ClassPolicy ||
		latest.RetryDisposition != failure.RetryAfterStateChange || latest.RequiredStateChange != failure.StateTrustedWorkspace {
		t.Fatalf("policy failure classification = %+v", latest)
	}
	events, err := fixture.store.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != core.EvStageFailed && event.Type != core.EvCapabilityDenied && event.Type != core.EvCapabilityRejected {
			continue
		}
		encoded := string(event.Payload)
		for _, forbidden := range []string{"raw-path-sentinel", "secret-content-sentinel", "credential", "command", "prompt"} {
			if strings.Contains(encoded, forbidden) {
				t.Fatalf("event leaked %q: %s", forbidden, encoded)
			}
		}
		var payload map[string]any
		if json.Unmarshal(event.Payload, &payload) == nil && event.Type == core.EvStageFailed {
			if payload["retry_disposition"] != string(failure.RetryAfterStateChange) || payload["required_state"] != string(failure.StateTrustedWorkspace) {
				t.Fatalf("policy projection = %v", payload)
			}
		}
	}
}

type policyRecoveryFixture struct {
	engine  *Engine
	store   *store.Store
	runner  *runner.FakeRunner
	stage   flow.Stage
	issueID string
	workdir string
	starts  *atomic.Int32
}

func runRecoveredPolicyStage(t *testing.T, mode string, retries int) policyRecoveryFixture {
	t.Helper()
	fixture := newPolicyRecoveryFixture(t, mode, retries)
	if err := fixture.engine.runStage(context.Background(), fixture.engine.issues[fixture.issueID], fixture.stage, nil); err != nil {
		t.Fatal(err)
	}
	fixture.workdir = fixture.engine.issues[fixture.issueID].wsPath
	return fixture
}

func newPolicyRecoveryFixture(t *testing.T, mode string, retries int) policyRecoveryFixture {
	t.Helper()
	repo := t.TempDir()
	initGitRepo(t, repo)
	provider := workspace.GitWorktree{Repo: repo}
	stage := flow.Stage{
		Name: "inspect", Agents: []flow.AgentRef{{Package: "agent"}}, Workspace: "worktree",
		Completion: flow.CompletionAll, Gate: flow.GateAuto, Retries: retries,
		CapabilityProfile: flow.ProfileInspect,
	}
	configured := flow.Flow{Name: "recovery", Stages: []flow.Stage{stage}}
	starts := &atomic.Int32{}
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{"inspect/agent": {}}}
	r.OnStart = func(_, _, _, workdir string) error {
		if starts.Add(1) != 1 {
			return nil
		}
		if err := os.WriteFile(filepath.Join(workdir, "outside.txt"), []byte("rejected\n"), 0o644); err != nil {
			return err
		}
		if mode == "runtime" {
			return &capability.PolicyError{Phase: "runtime", Reason: capability.ReasonRuntimeDenied, Diagnostic: "unmediated operation"}
		}
		return nil
	}
	e, s := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{configured.Name: configured}
		cfg.Workspace = provider
	})
	id, err := e.CreateIssue("policy recovery", "", configured.Name, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	path, release, err := provider.Acquire(id)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(gitOutput(t, path, "rev-parse", "HEAD"))
	is := e.issues[id]
	is.wsPath, is.wsRelease = path, release
	is.branch, is.baseRef = "issue/"+id, head
	if err := s.SetIssueIntegration(store.IssueIntegration{
		IssueID: id, State: store.IntegrationClaimed, PreSHA: head, Worktree: path, Branch: is.branch,
	}); err != nil {
		t.Fatal(err)
	}
	return policyRecoveryFixture{engine: e, store: s, runner: r, stage: stage, issueID: id, workdir: path, starts: starts}
}

func auditHasPhases(records []capability.AuditRecord, phases ...string) bool {
	seen := map[string]bool{}
	for _, record := range records {
		seen[record.Phase] = true
	}
	for _, phase := range phases {
		if !seen[phase] {
			return false
		}
	}
	return true
}
