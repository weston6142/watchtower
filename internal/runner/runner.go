package runner

import (
	"context"

	"github.com/wbushyeager/guildhall/internal/levers"
)

type Ask struct {
	Decision levers.Decision
	Reply    chan int
}

type Result struct {
	Artifacts map[string]string
	Tokens    int
	Err       error
}

type Runner interface {
	Run(ctx context.Context, issueID, stage, agentPkg, workdir string,
		asks chan<- Ask) <-chan Result
}
