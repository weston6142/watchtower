package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wbushyeager/guildhall/internal/levers"
)

type Script struct {
	Asks      []levers.Decision
	Proposals []Proposal
	Artifacts map[string]string
	Tokens    int
	Fail      bool
}

type FakeRunner struct {
	Scripts    map[string]Script
	OnProposal func(string, Proposal)
}

func (f *FakeRunner) Run(ctx context.Context, issueID, stage, agentPkg, workdir string,
	asks chan<- Ask) <-chan Result {
	done := make(chan Result, 1)
	go func() {
		sc, ok := f.Scripts[stage+"/"+agentPkg]
		if !ok {
			done <- Result{Err: fmt.Errorf("no script for %s/%s", stage, agentPkg)}
			return
		}
		for _, d := range sc.Asks {
			reply := make(chan int, 1)
			select {
			case asks <- Ask{Decision: d, Reply: reply}:
			case <-ctx.Done():
				done <- Result{Err: ctx.Err()}
				return
			}
			select {
			case <-reply:
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
		if sc.Fail {
			done <- Result{Err: fmt.Errorf("scripted failure %s/%s", stage, agentPkg)}
			return
		}
		out := map[string]string{}
		for name, content := range sc.Artifacts {
			p := filepath.Join(workdir, name)
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				done <- Result{Err: err}
				return
			}
			out[name] = p
		}
		done <- Result{Artifacts: out, Tokens: sc.Tokens}
	}()
	return done
}
