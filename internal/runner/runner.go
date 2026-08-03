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

type Result struct {
	Artifacts map[string]string
	DependsOn []string
	SessionID string
	Tokens    int
	Err       error
}

type Runner interface {
	Run(ctx context.Context, issueID, stage, agentPkg, workdir string,
		asks chan<- Ask) <-chan Result
}

// LineSink is implemented by runners that can stream human-readable output.
type LineSink interface {
	SetOnLine(func(issueID, stage, line string))
}
