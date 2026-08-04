package runner

import (
	"context"

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

func WithOperationID(ctx context.Context, operationID string) context.Context {
	return context.WithValue(ctx, operationIDKey{}, operationID)
}

func OperationID(ctx context.Context) string {
	if value, ok := ctx.Value(operationIDKey{}).(string); ok {
		return value
	}
	return ""
}
