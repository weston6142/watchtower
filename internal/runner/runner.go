package runner

import (
	"context"
	"os"
	"strings"

	"github.com/weston6142/watchtower/internal/levers"
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
	SessionID        string
	Tokens           int
	TokensKnown      bool
	FailureClass     FailureClass
	Attempt          Attempt
	Attempts         []Attempt
	FallbackConsumed bool
	NextAction       string
	Err              error
}

type Runner interface {
	Run(ctx context.Context, issueID, stage, agentPkg, workdir string,
		asks chan<- Ask) <-chan Result
}

// PlannerArtifactAuthority is the only planner-artifact capability a runner
// may use. It deliberately exposes no environment, path, or raw capability
// accessors.
type PlannerArtifactAuthority interface {
	ApplyPlannerArtifact(any) error
	AttachPlannerArtifactDescriptor() (*os.File, error)
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
	RunPlanner(context.Context, string, string, string, string, chan<- Ask, ExplorationGate) <-chan Result
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

const plannerArtifactSessionEnv = "WATCHTOWER_PLANNER_SESSION"

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
			if ok && key == plannerArtifactSessionEnv {
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
		if key == plannerArtifactSessionEnv {
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
