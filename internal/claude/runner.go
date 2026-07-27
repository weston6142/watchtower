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

type CodeRunner struct {
	Bin      string
	Packages map[string]pkgs.Package
	ExtraEnv []string
}

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
		"--dangerously-skip-permissions=false",
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

	task := fmt.Sprintf("Task: %s for issue %s. Work in the current directory.", stage, issueID)
	if _, err := stdin.Write(UserMessage(task)); err != nil {
		cmd.Process.Kill()
		return runner.Result{Err: err}
	}

	var res runner.Result
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	gotResult := false
	for sc.Scan() {
		ev := ParseLine(sc.Bytes())
		switch ev.Kind {
		case "init":
			res.SessionID = ev.SessionID
		case "assistant_text":
			if d, found := ExtractDecision(ev.Text); found {
				reply := make(chan int, 1)
				select {
				case asks <- runner.Ask{Decision: d, Reply: reply}:
				case <-ctx.Done():
					cmd.Process.Kill()
					res.Err = ctx.Err()
					return res
				}
				var choice int
				select {
				case choice = <-reply:
				case <-ctx.Done():
					cmd.Process.Kill()
					res.Err = ctx.Err()
					return res
				}
				opt := ""
				if choice >= 0 && choice < len(d.Options) {
					opt = d.Options[choice]
				}
				if _, err := stdin.Write(UserMessage("Human decision: " + opt)); err != nil {
					cmd.Process.Kill()
					res.Err = err
					return res
				}
			}
		case "result":
			res.Tokens = ev.Tokens
			gotResult = true
			if ev.IsError {
				res.Err = fmt.Errorf("claude session %s ended with error", res.SessionID)
			}
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
