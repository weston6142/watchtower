package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/weston6142/watchtower/internal/levers"
)

type Script struct {
	Asks            []levers.Decision
	Proposals       []Proposal
	ProposalBatches [][]Proposal
	DependsOn       []string
	Lines           []string
	Artifacts       map[string]string
	SessionID       string
	Tokens          int
	Fail            bool
}

type FakeRunner struct {
	Scripts         map[string]Script
	OnProposal      func(string, Proposal)
	OnProposalBatch func(string, []Proposal)
	OnResponse      func(issueID, stage string, response levers.Response)
	OnStart         func(issueID, stage, agentPkg, workdir string) error
	OnLine          func(issueID, stage, line string)
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
		if f.OnStart != nil {
			if err := f.OnStart(issueID, stage, agentPkg, workdir); err != nil {
				done <- Result{SessionID: sc.SessionID, Err: err}
				return
			}
		}
		for _, d := range sc.Asks {
			reply := make(chan levers.Response, 1)
			select {
			case asks <- Ask{Decision: d, Reply: reply}:
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
		done <- Result{
			Artifacts: out, DependsOn: append([]string(nil), sc.DependsOn...),
			SessionID: sc.SessionID, Tokens: sc.Tokens,
		}
	}()
	return done
}

func (f *FakeRunner) SetOnLine(fn func(issueID, stage, line string)) { f.OnLine = fn }

func generatedFakeArtifact(name, workdir string) (string, error) {
	switch name {
	case "merge-decision.json":
		return `{"decision":"merge"}`, nil
	case "verification.json":
		stageBrief, err := os.ReadFile(filepath.Join(workdir, "STAGE.md"))
		if err != nil {
			return "", err
		}
		base := ""
		for _, line := range strings.Split(string(stageBrief), "\n") {
			if strings.HasPrefix(line, "- Base commit: ") {
				base = strings.TrimSpace(strings.TrimPrefix(line, "- Base commit: "))
				break
			}
		}
		revision := func(ref string) (string, error) {
			output, err := exec.Command("git", "-C", workdir, "rev-parse", ref).CombinedOutput()
			if err != nil {
				return "", fmt.Errorf("fake verification %s: %v: %s",
					ref, err, strings.TrimSpace(string(output)))
			}
			return strings.TrimSpace(string(output)), nil
		}
		branch, err := revision("HEAD")
		if err != nil {
			return "", err
		}
		tree, err := revision("HEAD^{tree}")
		if err != nil {
			return "", err
		}
		document, err := json.Marshal(map[string]any{
			"base_sha": base, "branch_sha": branch, "tree_sha": tree,
			"passed": true, "commands": [][]string{{"true"}},
		})
		return string(document), err
	default:
		return "", nil
	}
}
