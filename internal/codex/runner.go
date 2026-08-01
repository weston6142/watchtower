package codex

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/weston6142/watchtower/internal/agentprotocol"
	"github.com/weston6142/watchtower/internal/pkgs"
	"github.com/weston6142/watchtower/internal/runner"
)

const (
	maxLineBytes    = 1 << 20
	stderrTailBytes = 16 << 10
)

// CodeRunner drives unattended Codex CLI turns and translates their JSONL
// output into the runner contract.
type CodeRunner struct {
	Bin             string
	Packages        map[string]pkgs.Package
	DefaultModel    string
	DefaultEffort   string
	ExtraEnv        []string
	OnProposal      func(string, runner.Proposal)
	OnProposalBatch func(string, []runner.Proposal)
	OnLine          func(issueID, stage, line string)
}

func (c *CodeRunner) Run(ctx context.Context, issueID, stage, agentPkg, workdir string,
	asks chan<- runner.Ask) <-chan runner.Result {
	done := make(chan runner.Result, 1)
	go func() {
		done <- c.run(ctx, issueID, stage, agentPkg, workdir)
	}()
	return done
}

func (c *CodeRunner) run(ctx context.Context, issueID, stage, agentPkg, workdir string) runner.Result {
	pkg, ok := c.Packages[agentPkg]
	if !ok {
		return runner.Result{Err: fmt.Errorf("unknown agent package %q", agentPkg)}
	}
	model, effort := c.effective(pkg)
	task := agentprotocol.TaskMessage(stage, issueID)
	args := []string{
		"exec", "--json", "-C", workdir, "-m", model,
		"-c", configString("model_reasoning_effort", effort),
		"-c", configString("sandbox_mode", "danger-full-access"),
		"-c", configString("approval_policy", "never"),
		"-c", configString("developer_instructions", pkg.Prompt),
		task,
	}

	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), c.ExtraEnv...)
	// If a shell wrapper leaves a child holding the JSONL pipe open after
	// cancellation, do not let that child defeat CommandContext cancellation.
	cmd.WaitDelay = 250 * time.Millisecond
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return runner.Result{Err: fmt.Errorf("codex stdout: %w", err)}
	}
	var stderrTail tailBuffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrTail)
	if err := cmd.Start(); err != nil {
		return runner.Result{Err: fmt.Errorf("codex start: %w", err)}
	}

	var res runner.Result
	gotComplete := false
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), maxLineBytes)
	for scanner.Scan() {
		event := ParseLine(scanner.Bytes())
		switch event.Kind {
		case KindThread:
			res.SessionID = event.ThreadID
		case KindText:
			c.emitText(issueID, stage, event.Text)
		case KindTool:
			if c.OnLine != nil && strings.TrimSpace(event.Tool) != "" {
				c.OnLine(issueID, stage, event.Tool)
			}
		case KindComplete:
			res.Tokens += event.Tokens
			gotComplete = true
		case KindFailed:
			res.Err = fmt.Errorf("codex turn failed: %s", event.Error)
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return runner.Result{
			SessionID: res.SessionID,
			Tokens:    res.Tokens,
			Err:       c.withStderr(fmt.Errorf("codex JSONL: %w", scanErr), stderrTail.String(), pkg.Prompt, task),
		}
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		res.Err = fmt.Errorf("codex: %w", ctx.Err())
	} else if res.Err == nil && waitErr != nil {
		res.Err = fmt.Errorf("codex exited: %w", waitErr)
	} else if res.Err == nil && res.SessionID == "" {
		res.Err = fmt.Errorf("codex ended without thread ID")
	} else if res.Err == nil && !gotComplete {
		res.Err = fmt.Errorf("codex thread %s ended without completion event", res.SessionID)
	}
	if res.Err != nil {
		res.Err = c.withStderr(res.Err, stderrTail.String(), pkg.Prompt, task)
	}
	return res
}

func (c *CodeRunner) effective(pkg pkgs.Package) (string, string) {
	model, effort := pkg.Model, pkg.Effort
	if model == "" {
		model = c.DefaultModel
	}
	if effort == "" {
		effort = c.DefaultEffort
	}
	return model, effort
}

func (c *CodeRunner) emitText(issueID, stage, value string) {
	if c.OnLine == nil {
		return
	}
	for _, line := range strings.Split(value, "\n") {
		if strings.TrimSpace(line) != "" {
			c.OnLine(issueID, stage, line)
		}
	}
}

func (c *CodeRunner) withStderr(base error, stderr, prompt, task string) error {
	if prompt != "" {
		stderr = strings.ReplaceAll(stderr, prompt, "[redacted prompt]")
	}
	if task != "" {
		stderr = strings.ReplaceAll(stderr, task, "[redacted task]")
	}
	for _, entry := range c.ExtraEnv {
		if _, value, ok := strings.Cut(entry, "="); ok && value != "" {
			stderr = strings.ReplaceAll(stderr, value, "[redacted env]")
		}
	}
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return base
	}
	return fmt.Errorf("%w: %s", base, stderr)
}

func configString(key, value string) string {
	return key + "=" + strconv.Quote(value)
}

type tailBuffer struct{ data []byte }

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.data = append(b.data, p...)
	if len(b.data) > stderrTailBytes {
		b.data = append([]byte(nil), b.data[len(b.data)-stderrTailBytes:]...)
	}
	return n, nil
}

func (b *tailBuffer) String() string { return strings.TrimSpace(string(b.data)) }

func (c *CodeRunner) SetOnLine(fn func(issueID, stage, line string)) { c.OnLine = fn }
