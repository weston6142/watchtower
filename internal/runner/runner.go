package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/stageresult"
)

type Ask struct {
	Decision levers.Decision
	Reply    chan levers.Response
	Error    chan error
}

// Proposal is a suggested new issue discovered by an agent mid-flow.
type Proposal struct {
	Key       string
	Title     string
	Body      string
	DependsOn []string
}

type FailureClass string

const (
	FailureLaunch         FailureClass = "launch"
	FailureExecution      FailureClass = "execution"
	FailureTransport      FailureClass = "transport"
	FailureProtocol       FailureClass = "protocol"
	FailureCancellation   FailureClass = "cancellation"
	FailureAuthentication FailureClass = "authentication"
	FailureAuthorization  FailureClass = "authorization"
	FailureResumeIdentity FailureClass = "resume_identity"
	FailureConfiguration  FailureClass = "configuration"
	FailureUnknown        FailureClass = "unknown"
)

type AttemptKind string

const (
	AttemptPrimary  AttemptKind = "primary"
	AttemptFallback AttemptKind = "fallback"
)

type AttemptState string

const (
	AttemptRunning   AttemptState = "running"
	AttemptFailed    AttemptState = "failed"
	AttemptReserved  AttemptState = "reserved"
	AttemptSucceeded AttemptState = "succeeded"
	AttemptTerminal  AttemptState = "terminal"
)

type Attempt struct {
	OperationID  string       `json:"operation_id"`
	IssueID      string       `json:"issue_id"`
	Stage        string       `json:"stage"`
	AgentPackage string       `json:"agent_package"`
	Kind         AttemptKind  `json:"kind"`
	State        AttemptState `json:"state"`
	FailureClass FailureClass `json:"failure_class,omitempty"`
	SessionID    string       `json:"session_id,omitempty"`
	Tokens       int          `json:"tokens,omitempty"`
	RedactedArgv []string     `json:"redacted_argv,omitempty"`
}

type Result struct {
	Artifacts        map[string]string
	DependsOn        []string
	StageEvidence    *stageresult.Evidence
	SessionID        string
	Tokens           int
	TokensKnown      bool
	FailureClass     FailureClass
	Attempt          Attempt
	Attempts         []Attempt
	FallbackConsumed bool
	NextAction       string
	RuntimeAudit     []capability.AuditRecord
	Err              error
}

type PreflightRequest struct {
	IssueID  string
	Stage    string
	Agent    string
	Workdir  string
	Contract capability.CompiledContract
}

type StageRequest struct {
	IssueID  string
	Stage    string
	Agent    string
	Workdir  string
	Contract capability.CompiledContract
	Plan     capability.EnforcementPlan
}

type Runner interface {
	Preflight(context.Context, PreflightRequest) (capability.EnforcementPlan, error)
	Run(context.Context, StageRequest, chan<- Ask) <-chan Result
}

func NewEnforcementPlan(contract capability.CompiledContract, provider, implementation, version string,
	proofs []capability.ControlProof) (capability.EnforcementPlan, error) {
	plan := capability.EnforcementPlan{
		ContractID: contract.ContractID, Provider: provider, Implementation: implementation, Version: version,
		Controls: append([]capability.ControlProof(nil), proofs...),
	}
	if err := canonicalizeAndValidatePlan(contract, &plan); err != nil {
		return capability.EnforcementPlan{}, err
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return capability.EnforcementPlan{}, err
	}
	sum := sha256.Sum256(encoded)
	plan.PlanID = hex.EncodeToString(sum[:])
	return plan, nil
}

func ValidateEnforcementPlan(contract capability.CompiledContract, plan capability.EnforcementPlan) error {
	providedID := plan.PlanID
	plan.PlanID = ""
	if err := canonicalizeAndValidatePlan(contract, &plan); err != nil {
		return err
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(encoded)
	if providedID != hex.EncodeToString(sum[:]) {
		return &capability.PolicyError{Phase: "preflight", Reason: capability.ReasonProviderUnsupported, Provider: plan.Provider, Diagnostic: "enforcement plan identity mismatch"}
	}
	return nil
}

func ValidateStageRequest(request StageRequest) error {
	if request.IssueID == "" || request.Stage == "" || request.Agent == "" || request.Workdir == "" ||
		request.Contract.ContractID == "" || request.Contract.Contract.IssueID != request.IssueID ||
		request.Contract.Contract.Stage != request.Stage || request.Plan.ContractID != request.Contract.ContractID {
		return &capability.PolicyError{Phase: "launch", Reason: capability.ReasonProviderUnsupported, Diagnostic: "stage request identity mismatch"}
	}
	return ValidateEnforcementPlan(request.Contract, request.Plan)
}

func RequiredControls(contract capability.CompiledContract) []capability.EnforcementControl {
	controls := []capability.EnforcementControl{
		capability.ControlCredentials, capability.ControlDescendants, capability.ControlEngineState, capability.ControlGateway, capability.ControlGitCommonDir,
		capability.ControlNetwork, capability.ControlProcessGroup, capability.ControlScratch,
	}
	for _, operation := range contract.Contract.Operations {
		switch operation {
		case capability.OpWorkspaceRead, capability.OpVCSRead:
			controls = append(controls, capability.ControlWorkspaceRead)
		case capability.OpWorkspaceMutate, capability.OpVCSCommit, capability.OpPlannerArtifactApply:
			controls = append(controls, capability.ControlWorkspaceWrite)
		}
	}
	sort.Slice(controls, func(i, j int) bool { return controls[i] < controls[j] })
	result := controls[:0]
	for _, control := range controls {
		if len(result) == 0 || result[len(result)-1] != control {
			result = append(result, control)
		}
	}
	return result
}

func canonicalizeAndValidatePlan(contract capability.CompiledContract, plan *capability.EnforcementPlan) error {
	if contract.ContractID == "" || plan.ContractID != contract.ContractID || plan.Provider == "" || plan.Implementation == "" || plan.Version == "" {
		return &capability.PolicyError{Phase: "preflight", Reason: capability.ReasonProviderUnsupported, Provider: plan.Provider, Diagnostic: "enforcement plan identity is incomplete"}
	}
	sort.Slice(plan.Controls, func(i, j int) bool { return plan.Controls[i].Control < plan.Controls[j].Control })
	required := RequiredControls(contract)
	if len(plan.Controls) != len(required) {
		return &capability.PolicyError{Phase: "preflight", Reason: capability.ReasonProviderUnsupported, Provider: plan.Provider, Diagnostic: "complete control proof is required"}
	}
	for index, control := range required {
		if plan.Controls[index].Control != control || !plan.Controls[index].Proven {
			return &capability.PolicyError{Phase: "preflight", Reason: capability.ReasonProviderUnsupported, Provider: plan.Provider, Diagnostic: fmt.Sprintf("control %s is not proven", control)}
		}
	}
	return nil
}

// PlannerArtifactAuthority is the only planner-artifact capability a runner
// may use. It deliberately exposes no environment, path, or raw capability
// accessors.
type PlannerArtifactAuthority interface {
	ApplyPlannerArtifact(any) error
}

type ToolCall struct {
	Name        string
	SourceID    string
	Fingerprint string
	Reservation int64
	Priority    int
}

type ToolDecision struct {
	Allowed       bool
	LeaseID       string
	CachedContent string
}

type ExplorationGate interface {
	Admit(context.Context, ToolCall) (ToolDecision, error)
	Complete(context.Context, ToolDecision, *int64, error) error
}

type PlannerRunner interface {
	RunPlanner(context.Context, StageRequest, chan<- Ask, ExplorationGate) <-chan Result
}

// LineSink is implemented by runners that can stream human-readable output.
type LineSink interface {
	SetOnLine(func(issueID, stage, line string))
}

type AttemptSink interface {
	RecordAttempt(context.Context, Attempt) error
	LoadOperation(context.Context, string) ([]Attempt, error)
}

type AttemptReporter interface {
	SetAttemptSink(AttemptSink)
}

type operationIDKey struct{}

type plannerArtifactAuthorityKey struct{}

const privatePlannerEnvironmentPrefix = "WATCHTOWER_PLANNER_"

func isPrivatePlannerEnvironmentKey(key string) bool {
	return strings.HasPrefix(key, privatePlannerEnvironmentPrefix)
}

type managedEnvironmentKey struct{}

// WithPlannerArtifactAuthority attaches an immutable engine-owned authority
// handle to the runner context.
func WithPlannerArtifactAuthority(ctx context.Context, authority PlannerArtifactAuthority) context.Context {
	return context.WithValue(ctx, plannerArtifactAuthorityKey{}, authority)
}

// PlannerArtifactAuthorityFromContext returns the engine-owned authority, if
// one was attached. It never consults process environment or argv.
func PlannerArtifactAuthorityFromContext(ctx context.Context) PlannerArtifactAuthority {
	authority, _ := ctx.Value(plannerArtifactAuthorityKey{}).(PlannerArtifactAuthority)
	return authority
}

// WithManagedEnvironment carries an immutable child-process environment
// overlay selected by the verification lifecycle.
func WithManagedEnvironment(ctx context.Context, env []string) context.Context {
	return context.WithValue(ctx, managedEnvironmentKey{}, append([]string(nil), env...))
}

// ManagedEnvironment returns a copy of the lifecycle-provided overlay.
func ManagedEnvironment(ctx context.Context) []string {
	if env, ok := ctx.Value(managedEnvironmentKey{}).([]string); ok {
		return append([]string(nil), env...)
	}
	return nil
}

// MergeEnvironment preserves inherited and extra variables while replacing
// every key present in the managed overlay exactly once.
func MergeEnvironment(inherited, extra, managed []string) []string {
	replacements := make(map[string]string, len(managed))
	order := make([]string, 0, len(managed))
	for _, entry := range managed {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		if _, seen := replacements[key]; !seen {
			order = append(order, key)
		}
		replacements[key] = value
	}
	result := make([]string, 0, len(inherited)+len(extra)+len(order))
	appendUnmanaged := func(entries []string) {
		for _, entry := range entries {
			key, _, ok := strings.Cut(entry, "=")
			if ok && isPrivatePlannerEnvironmentKey(key) {
				continue
			}
			if ok {
				if _, replace := replacements[key]; replace {
					continue
				}
			}
			result = append(result, entry)
		}
	}
	appendUnmanaged(inherited)
	appendUnmanaged(extra)
	for _, key := range order {
		if isPrivatePlannerEnvironmentKey(key) {
			continue
		}
		result = append(result, key+"="+replacements[key])
	}
	return result
}

func WithOperationID(ctx context.Context, operationID string) context.Context {
	return context.WithValue(ctx, operationIDKey{}, operationID)
}

func OperationID(ctx context.Context) string {
	if value, ok := ctx.Value(operationIDKey{}).(string); ok {
		return value
	}
	return ""
}
