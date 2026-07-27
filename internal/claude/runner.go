package claude

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/wbushyeager/guildhall/internal/pkgs"
	"github.com/wbushyeager/guildhall/internal/runner"
)

// maxLineBytes bounds a single stream-json line; the CLI can emit large
// assistant messages that exceed bufio.Scanner's default 64KiB limit.
const maxLineBytes = 1 << 20

// CodeRunner drives a claude CLI subprocess in stream-json mode, translating
// its output into runner.Result and decision markers into runner.Ask.
type CodeRunner struct {
	Bin        string
	Packages   map[string]pkgs.Package
	ExtraEnv   []string
	OnProposal func(string, runner.Proposal)
	OnLine     func(string, string, string)
}

const coachMsg = `Your guildhall_decision is missing required fields. Re-emit the SAME decision
as one JSON line including: "why" (one line: why you recommend option N) and
"consequences" (one line per option, same order as options). Nothing else.`

func (c *CodeRunner) Run(ctx context.Context, issueID, stage, agentPkg, workdir string,
	asks chan<- runner.Ask) <-chan runner.Result {
	done := make(chan runner.Result, 1)
	go func() {
		done <- c.run(ctx, issueID, stage, agentPkg, workdir, asks)
	}()
	return done
}

func (c *CodeRunner) run(ctx context.Context, issueID, stage, agentPkg, workdir string,
	asks chan<- runner.Ask) runner.Result {
	pkg, ok := c.Packages[agentPkg]
	if !ok {
		return runner.Result{Err: fmt.Errorf("unknown agent package %q", agentPkg)}
	}
	args := []string{"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--append-system-prompt", pkg.Prompt,
	}
	if len(pkg.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(pkg.AllowedTools, ","))
	}
	if pkg.Model != "" {
		args = append(args, "--model", pkg.Model)
	}

	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), c.ExtraEnv...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return runner.Result{Err: err}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return runner.Result{Err: err}
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return runner.Result{Err: err}
	}

	task := fmt.Sprintf("Task: run the %s stage for issue %s. Read ISSUE.md in the current directory for the issue description; artifacts from earlier stages are alongside it. Work in the current directory.", stage, issueID)
	if _, err := stdin.Write(UserMessage(task)); err != nil {
		cmd.Process.Kill()
		return runner.Result{Err: err}
	}

	var res runner.Result
	// abort kills the subprocess and returns the partial result with err set.
	abort := func(err error) runner.Result {
		cmd.Process.Kill()
		res.Err = err
		return res
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, maxLineBytes), maxLineBytes)
	gotResult := false
	repliedThisTurn := false
	sessionDone := false
	coachCount := 0
	for sc.Scan() {
		ev := ParseLine(sc.Bytes())
		switch ev.Kind {
		case KindInit:
			res.SessionID = ev.SessionID
		case KindAssistantText:
			if c.OnLine != nil {
				for _, line := range strings.Split(ev.Text, "\n") {
					c.OnLine(issueID, stage, line)
				}
			}
			if d, found := ExtractDecision(ev.Text); found {
				if (d.Why == "" || len(d.Consequences) != len(d.Options)) && coachCount < 2 {
					coachCount++
					if _, err := stdin.Write(UserMessage(coachMsg)); err != nil {
						return abort(err)
					}
					repliedThisTurn = true
					continue
				}
				repliedThisTurn = true
				reply := make(chan int, 1)
				select {
				case asks <- runner.Ask{Decision: d, Reply: reply}:
				case <-ctx.Done():
					return abort(ctx.Err())
				}
				var choice int
				select {
				case choice = <-reply:
				case <-ctx.Done():
					return abort(ctx.Err())
				}
				opt := ""
				if choice >= 0 && choice < len(d.Options) {
					opt = d.Options[choice]
				}
				if _, err := stdin.Write(UserMessage("Human decision: " + opt)); err != nil {
					return abort(err)
				}
			}
			if p, found := ExtractProposal(ev.Text); found && c.OnProposal != nil {
				c.OnProposal(issueID, p)
			}
		case KindResult:
			res.Tokens += ev.Tokens
			if c.OnLine != nil {
				c.OnLine(issueID, stage, fmt.Sprintf("— turn complete (%d tokens) —", ev.Tokens))
			}
			gotResult = true
			if ev.IsError {
				res.Err = fmt.Errorf("claude session %s ended with error", res.SessionID)
			}
			// In stream-json input mode the CLI emits one result per turn and
			// then waits for more input. A turn that asked a decision continues
			// (we already sent the reply); any other completed turn is the
			// agent's final turn — close stdin so the process exits.
			if !repliedThisTurn || res.Err != nil {
				sessionDone = true
			}
			repliedThisTurn = false
		}
		if sessionDone {
			break
		}
	}
	stdin.Close()
	waitErr := cmd.Wait()
	if res.Err == nil && waitErr != nil {
		res.Err = fmt.Errorf("claude exited: %w", waitErr)
	}
	if res.Err == nil && !gotResult {
		res.Err = fmt.Errorf("claude session %s ended without result event", res.SessionID)
	}
	return res
}

func (c *CodeRunner) SetOnLine(fn func(issueID, stage, line string)) { c.OnLine = fn }
