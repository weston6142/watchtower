package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/plannerartifact"
	"github.com/weston6142/watchtower/internal/stageresult"
	"github.com/weston6142/watchtower/internal/touchset"
)

type OperationAttempt struct {
	Operation capability.OperationClass
	Mutation  capability.MutationClass
	Path      string
	Content   string
}

type Script struct {
	Asks              []levers.Decision
	Proposals         []Proposal
	ProposalBatches   [][]Proposal
	DependsOn         []string
	Lines             []string
	Artifacts         map[string]string
	SessionID         string
	Tokens            int
	TokensKnown       bool
	Tools             []ToolCall
	Fail              bool
	PlannerRequests   []plannerartifact.WriteRequest
	PlannerFailureAt  int
	PlannerFailure    error
	StageEvidence     *stageresult.Evidence
	OmitStageEvidence bool
	OperationAttempts []OperationAttempt
}

type FakeRunner struct {
	Scripts         map[string]Script
	OnProposal      func(string, Proposal)
	OnProposalBatch func(string, []Proposal)
	OnResponse      func(issueID, stage string, response levers.Response)
	OnStart         func(issueID, stage, agentPkg, workdir string) error
	OnEnvironment   func(issueID, stage, agentPkg, workdir string, env []string)
	OnLine          func(issueID, stage, line string)
	PreflightError  error
	PreflightPlan   *capability.EnforcementPlan
}

func (f *FakeRunner) Preflight(_ context.Context, request PreflightRequest) (capability.EnforcementPlan, error) {
	if f.PreflightError != nil {
		return capability.EnforcementPlan{}, f.PreflightError
	}
	if f.PreflightPlan != nil {
		plan := *f.PreflightPlan
		plan.Controls = append([]capability.ControlProof(nil), f.PreflightPlan.Controls...)
		return plan, ValidateEnforcementPlan(request.Contract, plan)
	}
	controls := RequiredControls(request.Contract)
	proofs := make([]capability.ControlProof, 0, len(controls))
	for _, control := range controls {
		proofs = append(proofs, capability.ControlProof{Control: control, Proven: true})
	}
	return NewEnforcementPlan(request.Contract, "fake", "deterministic-fake", "1", proofs)
}

func (f *FakeRunner) Run(ctx context.Context, request StageRequest, asks chan<- Ask) <-chan Result {
	done := make(chan Result, 1)
	go func() {
		if err := ValidateStageRequest(request); err != nil {
			done <- Result{Err: err}
			return
		}
		issueID, stage, agentPkg, workdir := request.IssueID, request.Stage, request.Agent, request.Workdir
		sc, ok := f.Scripts[stage+"/"+agentPkg]
		if !ok {
			done <- Result{Err: fmt.Errorf("no script for %s/%s", stage, agentPkg)}
			return
		}
		if f.OnEnvironment != nil {
			f.OnEnvironment(issueID, stage, agentPkg, workdir, ManagedEnvironment(ctx))
		}
		if f.OnStart != nil {
			if err := f.OnStart(issueID, stage, agentPkg, workdir); err != nil {
				done <- Result{SessionID: sc.SessionID, Err: err}
				return
			}
		}
		for _, d := range sc.Asks {
			reply := make(chan levers.Response, 1)
			failure := make(chan error, 1)
			select {
			case asks <- Ask{Decision: d, Reply: reply, Error: failure}:
			case <-ctx.Done():
				done <- Result{Err: ctx.Err()}
				return
			}
			select {
			case response, ok := <-reply:
				if !ok || !d.Accepts(response) {
					done <- Result{Err: context.Canceled}
					return
				}
				if f.OnResponse != nil {
					f.OnResponse(issueID, stage, response)
				}
			case err := <-failure:
				done <- Result{SessionID: sc.SessionID, Err: err}
				return
			case <-ctx.Done():
				done <- Result{Err: ctx.Err()}
				return
			}
		}
		for _, p := range sc.Proposals {
			if f.OnProposal != nil {
				f.OnProposal(issueID, p)
			}
		}
		for _, batch := range sc.ProposalBatches {
			if f.OnProposalBatch != nil {
				f.OnProposalBatch(issueID, batch)
			}
		}
		for _, line := range sc.Lines {
			if f.OnLine != nil {
				f.OnLine(issueID, stage, line)
			}
		}
		var runtimeAudit []capability.AuditRecord
		for _, operation := range sc.OperationAttempts {
			audit := capability.AuditRecord{
				ContractID: request.Contract.ContractID, Phase: "runtime", Outcome: "passed",
				Operation: operation.Operation,
			}
			if operation.Path != "" {
				audit.Paths = []string{operation.Path}
			}
			if !fakeOperationAllowed(request.Contract, operation) {
				audit.Outcome = "denied"
				audit.Reason = capability.ReasonRuntimeDenied
				runtimeAudit = append(runtimeAudit, audit)
				done <- Result{RuntimeAudit: runtimeAudit, Err: &capability.PolicyError{
					Phase: "runtime", Reason: capability.ReasonRuntimeDenied, Operation: operation.Operation, Paths: audit.Paths,
				}}
				return
			}
			if operation.Operation == capability.OpWorkspaceMutate {
				path := filepath.Join(workdir, filepath.FromSlash(operation.Path))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					done <- Result{RuntimeAudit: runtimeAudit, Err: err}
					return
				}
				if err := os.WriteFile(path, []byte(operation.Content), 0o644); err != nil {
					done <- Result{RuntimeAudit: runtimeAudit, Err: err}
					return
				}
			}
			runtimeAudit = append(runtimeAudit, audit)
		}
		if sc.Fail {
			done <- Result{Err: fmt.Errorf("scripted failure %s/%s", stage, agentPkg)}
			return
		}
		out := map[string]string{}
		for name, content := range sc.Artifacts {
			if content == "" {
				generated, err := generatedFakeArtifact(name, workdir)
				if err != nil {
					done <- Result{Err: err}
					return
				}
				content = generated
			}
			p := filepath.Join(workdir, name)
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				done <- Result{Err: err}
				return
			}
			out[name] = p
		}
		stageEvidence, err := fakeStageEvidence(sc, agentPkg)
		if err != nil {
			done <- Result{Err: err}
			return
		}
		done <- Result{
			Artifacts: out, DependsOn: append([]string(nil), sc.DependsOn...),
			SessionID: sc.SessionID, Tokens: sc.Tokens, TokensKnown: sc.TokensKnown,
			StageEvidence: stageEvidence, RuntimeAudit: runtimeAudit,
		}
	}()
	return done
}

func fakeOperationAllowed(contract capability.CompiledContract, attempted OperationAttempt) bool {
	allowedOperation := false
	for _, operation := range contract.Contract.Operations {
		allowedOperation = allowedOperation || operation == attempted.Operation
	}
	if !allowedOperation {
		return false
	}
	if attempted.Operation != capability.OpWorkspaceMutate {
		return true
	}
	canonical, err := touchset.CanonicalPath(attempted.Path)
	if err != nil {
		return false
	}
	mutation := attempted.Mutation
	if mutation == "" {
		mutation = capability.MutationModify
	}
	for _, grant := range contract.Contract.Writes {
		matched, matchErr := touchset.Match(grant.Path, canonical)
		if matchErr != nil || !matched {
			continue
		}
		for _, allowed := range grant.Mutations {
			if allowed == mutation {
				return true
			}
		}
	}
	return false
}

func (f *FakeRunner) SetOnLine(fn func(issueID, stage, line string)) { f.OnLine = fn }

func fakeStageEvidence(script Script, agentPkg string) (*stageresult.Evidence, error) {
	if script.OmitStageEvidence {
		return nil, nil
	}
	if script.StageEvidence != nil {
		data, err := json.Marshal(script.StageEvidence)
		if err != nil {
			return nil, fmt.Errorf("copy fake stage evidence: %w", err)
		}
		var copied stageresult.Evidence
		if err := json.Unmarshal(data, &copied); err != nil {
			return nil, fmt.Errorf("copy fake stage evidence: %w", err)
		}
		return &copied, nil
	}
	kind, ok := stageresult.KindForAgentPackage(agentPkg)
	if !ok {
		return nil, nil
	}
	evidence := stageresult.Evidence{
		SchemaVersion: stageresult.SchemaVersion,
		StageKind:     kind,
		Outcome:       stageresult.OutcomeCompleted,
	}
	switch kind {
	case stageresult.KindExecute:
		evidence.Execute = &stageresult.ExecutePayload{
			PlanTasks: []stageresult.PlanTask{{ID: "synthetic-task", Outcome: stageresult.TaskCompleted, Summary: "fake runner completed the stage"}},
			Skips: []stageresult.Skip{
				{Activity: "commits", Explanation: "the fake runner makes no repository commit"},
				{Activity: "checks", Explanation: "the fake runner does not execute provider checks"},
			},
		}
	case stageresult.KindCorrectnessReview:
		evidence.CorrectnessReview = &stageresult.CorrectnessReviewPayload{
			ReviewedPaths: []string{"internal/runner/fake.go"},
			Skips:         []stageresult.Skip{{Activity: "checks", Explanation: "the fake runner does not execute provider checks"}},
			NoChange:      &stageresult.NoChangeConclusion{Explanation: "the fake runner found no correctness change to make"},
		}
	case stageresult.KindCleanCodeReview:
		evidence.CleanCodeReview = &stageresult.CleanCodeReviewPayload{
			ReviewedPaths: []string{"internal/runner/fake.go"},
			Skips:         []stageresult.Skip{{Activity: "checks", Explanation: "the fake runner does not execute provider checks"}},
			NoChange:      &stageresult.NoChangeConclusion{Explanation: "the fake runner found no clean-code change to make"},
		}
	case stageresult.KindLibrarian:
		evidence.Librarian = &stageresult.LibrarianPayload{
			ReviewedPaths: []string{"internal/runner/fake.go"},
			NoChange:      &stageresult.NoChangeConclusion{Explanation: "the fake runner found no documentation change to make"},
		}
	}
	return &evidence, nil
}

func (f *FakeRunner) RunPlanner(ctx context.Context, request StageRequest, _asks chan<- Ask, gate ExplorationGate) <-chan Result {
	done := make(chan Result, 1)
	go func() {
		if err := ValidateStageRequest(request); err != nil {
			done <- Result{Err: err}
			return
		}
		issueID, stage, agentPkg, workdir := request.IssueID, request.Stage, request.Agent, request.Workdir
		sc, ok := f.Scripts[stage+"/"+agentPkg]
		if !ok {
			done <- Result{Err: fmt.Errorf("no script for %s/%s", stage, agentPkg)}
			return
		}
		if f.OnEnvironment != nil {
			f.OnEnvironment(issueID, stage, agentPkg, workdir, ManagedEnvironment(ctx))
		}
		if f.OnStart != nil {
			if err := f.OnStart(issueID, stage, agentPkg, workdir); err != nil {
				done <- Result{SessionID: sc.SessionID, Err: err}
				return
			}
		}
		for _, tool := range sc.Tools {
			decision, err := gate.Admit(ctx, tool)
			if err != nil {
				done <- Result{SessionID: sc.SessionID, Err: err}
				return
			}
			if !decision.Allowed {
				continue
			}
			var actual *int64
			if sc.TokensKnown {
				value := tool.Reservation
				actual = &value
			}
			if err := gate.Complete(ctx, decision, actual, nil); err != nil {
				done <- Result{SessionID: sc.SessionID, Err: err}
				return
			}
		}
		if sc.Fail {
			done <- Result{SessionID: sc.SessionID, Tokens: sc.Tokens, TokensKnown: sc.TokensKnown,
				Err: fmt.Errorf("scripted failure %s/%s", stage, agentPkg)}
			return
		}
		if len(sc.PlannerRequests) > 0 {
			authority := PlannerArtifactAuthorityFromContext(ctx)
			if authority == nil {
				done <- Result{SessionID: sc.SessionID, Err: fmt.Errorf("planner authority unavailable")}
				return
			}
			for index, request := range sc.PlannerRequests {
				if sc.PlannerFailure != nil && index == sc.PlannerFailureAt {
					done <- Result{SessionID: sc.SessionID, Err: sc.PlannerFailure}
					return
				}
				if err := authority.ApplyPlannerArtifact(request); err != nil {
					done <- Result{SessionID: sc.SessionID, Err: err}
					return
				}
			}
			artifacts := map[string]string{
				"plan.md":       filepath.Join(workdir, "plan.md"),
				"touchset.json": filepath.Join(workdir, "touchset.json"),
			}
			done <- Result{Artifacts: artifacts, DependsOn: append([]string(nil), sc.DependsOn...),
				SessionID: sc.SessionID, Tokens: sc.Tokens, TokensKnown: sc.TokensKnown}
			return
		}
		artifacts, err := writeFakeArtifacts(sc.Artifacts, workdir)
		if err != nil {
			done <- Result{SessionID: sc.SessionID, Err: err}
			return
		}
		done <- Result{Artifacts: artifacts, DependsOn: append([]string(nil), sc.DependsOn...),
			SessionID: sc.SessionID, Tokens: sc.Tokens, TokensKnown: sc.TokensKnown}
	}()
	return done
}

func writeFakeArtifacts(declared map[string]string, workdir string) (map[string]string, error) {
	out := map[string]string{}
	for name, content := range declared {
		if content == "" {
			generated, err := generatedFakeArtifact(name, workdir)
			if err != nil {
				return nil, err
			}
			content = generated
		}
		path := filepath.Join(workdir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return nil, err
		}
		out[name] = path
	}
	return out, nil
}

func generatedFakeArtifact(name, workdir string) (string, error) {
	switch name {
	case "merge-decision.json":
		base, branch, _, err := fakeVerificationIdentity(workdir)
		if err != nil {
			return "", err
		}
		document, err := json.Marshal(marshal.MergeDecision{
			Decision: "merge", BranchCommit: branch, BaseCommit: base,
		})
		return string(document), err
	case "verification.json":
		base, branch, tree, err := fakeVerificationIdentity(workdir)
		if err != nil {
			return "", err
		}
		document, err := json.Marshal(marshal.Verification{
			BaseSHA: base, BranchSHA: branch, TreeSHA: tree,
			Passed: true, Commands: [][]string{{"true"}},
		})
		return string(document), err
	default:
		return "", nil
	}
}

func fakeVerificationIdentity(workdir string) (base, branch, tree string, err error) {
	stageBrief, err := os.ReadFile(filepath.Join(workdir, "STAGE.md"))
	if err != nil {
		return "", "", "", err
	}
	for _, line := range strings.Split(string(stageBrief), "\n") {
		if strings.HasPrefix(line, "- Base commit: ") {
			base = strings.TrimSpace(strings.TrimPrefix(line, "- Base commit: "))
			break
		}
	}
	if base == "" || base == "unknown" {
		return "", "", "", fmt.Errorf("fake verification: STAGE.md has no base commit")
	}
	revision := func(ref string) (string, error) {
		output, revisionErr := exec.Command("git", "-C", workdir, "rev-parse", ref).CombinedOutput()
		if revisionErr != nil {
			return "", fmt.Errorf("fake verification %s: %v: %s",
				ref, revisionErr, strings.TrimSpace(string(output)))
		}
		return strings.TrimSpace(string(output)), nil
	}
	branch, err = revision("HEAD")
	if err != nil {
		return "", "", "", err
	}
	tree, err = revision("HEAD^{tree}")
	if err != nil {
		return "", "", "", err
	}
	return base, branch, tree, nil
}
