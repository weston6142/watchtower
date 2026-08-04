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
	"github.com/weston6142/watchtower/internal/deps"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/pkgs"
	"github.com/weston6142/watchtower/internal/repocfg"
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
	PrimaryProfile  repocfg.CodexProfile
	ExtraEnv        []string
	OnProposal      func(string, runner.Proposal)
	OnProposalBatch func(string, []runner.Proposal)
	OnLine          func(issueID, stage, line string)
}

func (c *CodeRunner) Run(ctx context.Context, issueID, stage, agentPkg, workdir string,
	asks chan<- runner.Ask) <-chan runner.Result {
	done := make(chan runner.Result, 1)
	go func() {
		done <- c.run(ctx, issueID, stage, agentPkg, workdir, asks)
	}()
	return done
}

type turnResult struct {
	threadID   string
	tokens     int
	events     []Event
	failed     error
	invocation invocation
}

func (c *CodeRunner) run(ctx context.Context, issueID, stage, agentPkg, workdir string,
	asks chan<- runner.Ask) runner.Result {
	pkg, ok := c.Packages[agentPkg]
	if !ok {
		return runner.Result{Err: fmt.Errorf("unknown agent package %q", agentPkg)}
	}
	profile := c.effectiveProfile(pkg)
	var res runner.Result
	threadID := ""
	prompt := agentprotocol.TaskMessage(stage, issueID)
	coachCount := 0
	decisionAccepted := false

	for {
		turn := c.runTurn(ctx, workdir, pkg, profile, threadID, prompt)
		if turn.threadID != "" {
			if threadID != "" && turn.threadID != threadID {
				res.Err = fmt.Errorf("codex resume returned thread %q, want %q", turn.threadID, threadID)
				return res
			}
			if threadID == "" {
				threadID = turn.threadID
				res.SessionID = threadID
			}
		}
		res.Tokens += turn.tokens
		if turn.failed != nil {
			res.Err = turn.failed
			return res
		}

		var decision *levers.Decision
		for _, event := range turn.events {
			switch event.Kind {
			case KindText:
				c.emitText(issueID, stage, event.Text)
				if decision == nil {
					if parsed, found := agentprotocol.ExtractDecision(event.Text); found {
						decision = &parsed
					}
				}
				if proposal, found := agentprotocol.ExtractProposal(event.Text); found && c.OnProposal != nil {
					c.OnProposal(issueID, proposal)
				}
				if batch, found := agentprotocol.ExtractProposalBatch(event.Text); found && c.OnProposalBatch != nil {
					c.OnProposalBatch(issueID, batch)
				}
				if dependsOn, found := agentprotocol.ExtractDependency(event.Text); found {
					if !decisionAccepted {
						res.Err = fmt.Errorf("dependency marker emitted without an accepted decision")
						return res
					}
					res.DependsOn = deps.Normalize(append(res.DependsOn, dependsOn...))
				}
			case KindTool:
				if c.OnLine != nil && strings.TrimSpace(event.Tool) != "" {
					c.OnLine(issueID, stage, event.Tool)
				}
			}
		}

		if decision == nil {
			return res
		}
		d := *decision
		incomplete := d.Why == "" ||
			(d.Kind == levers.DecisionChoice && len(d.Consequences) != len(d.Options)) ||
			(d.Kind == levers.DecisionFreeform && len(d.Consequences) == 0)
		if incomplete {
			if coachCount >= 2 {
				res.Err = fmt.Errorf("codex decision remained incomplete after 2 coaching attempts")
				return res
			}
			coachCount++
			prompt = agentprotocol.CoachMessage
			continue
		}

		reply := make(chan levers.Response, 1)
		failure := make(chan error, 1)
		select {
		case asks <- runner.Ask{Decision: d, Reply: reply, Error: failure}:
		case <-ctx.Done():
			res.Err = ctx.Err()
			return res
		}
		var response levers.Response
		select {
		case response = <-reply:
		case err := <-failure:
			res.Err = err
			return res
		case <-ctx.Done():
			res.Err = ctx.Err()
			return res
		}
		if !d.Accepts(response) {
			res.Err = fmt.Errorf("invalid response for decision %q", d.Question)
			return res
		}
		decisionAccepted = true
		answer := response.Text
		if response.Kind == levers.DecisionChoice {
			answer = d.Options[*response.Option]
		}
		prompt = "Human decision: " + answer
	}
}

func (c *CodeRunner) runTurn(ctx context.Context, workdir string, pkg pkgs.Package,
	profile repocfg.CodexProfile, threadID, prompt string) turnResult {
	kind := turnInitial
	if threadID != "" {
		kind = turnResumed
	}
	invocation := buildInvocation(profile, turnDescriptor{
		Workdir:       workdir,
		Kind:          kind,
		ResumeID:      threadID,
		PackagePrompt: pkg.Prompt,
		Prompt:        prompt,
	})

	cmd := exec.CommandContext(ctx, profile.Bin, invocation.Argv...)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), c.ExtraEnv...)
	// If a shell wrapper leaves a child holding the JSONL pipe open after
	// cancellation, do not let that child defeat CommandContext cancellation.
	cmd.WaitDelay = 250 * time.Millisecond
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return turnResult{failed: fmt.Errorf("codex stdout: %w", err), invocation: invocation}
	}
	var stderrTail tailBuffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrTail)
	if err := cmd.Start(); err != nil {
		return turnResult{failed: fmt.Errorf("codex start: %w", err), invocation: invocation}
	}

	var result turnResult
	gotComplete := false
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), maxLineBytes)
	for scanner.Scan() {
		event := ParseLine(scanner.Bytes())
		switch event.Kind {
		case KindThread:
			result.threadID = event.ThreadID
		case KindText, KindTool:
			result.events = append(result.events, event)
		case KindComplete:
			result.tokens += event.Tokens
			gotComplete = true
		case KindFailed:
			result.failed = fmt.Errorf("codex turn failed: %s", event.Error)
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		result.failed = c.withStderr(fmt.Errorf("codex JSONL: %w", scanErr), stderrTail.String(), pkg.Prompt, prompt)
		return result
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		result.failed = fmt.Errorf("codex: %w", ctx.Err())
	} else if result.failed == nil && waitErr != nil {
		result.failed = fmt.Errorf("codex exited: %w", waitErr)
	} else if result.failed == nil && threadID == "" && result.threadID == "" {
		result.failed = fmt.Errorf("codex ended without thread ID")
	} else if result.failed == nil && !gotComplete {
		activeThread := threadID
		if activeThread == "" {
			activeThread = result.threadID
		}
		result.failed = fmt.Errorf("codex thread %s ended without completion event", activeThread)
	}
	if result.failed != nil {
		result.failed = c.withStderr(result.failed, stderrTail.String(), pkg.Prompt, prompt)
	}
	return result
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

func (c *CodeRunner) effectiveProfile(pkg pkgs.Package) repocfg.CodexProfile {
	profile := c.PrimaryProfile
	if profile.Bin == "" {
		profile.Bin = c.Bin
	}
	if pkg.Model != "" {
		profile.Model = pkg.Model
	}
	if profile.Model == "" {
		profile.Model = c.DefaultModel
	}
	if pkg.Effort != "" {
		profile.Effort = pkg.Effort
	}
	if profile.Effort == "" {
		profile.Effort = c.DefaultEffort
	}
	return profile
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
	secrets := []string{prompt, task}
	for _, entry := range c.ExtraEnv {
		if _, value, ok := strings.Cut(entry, "="); ok && value != "" {
			secrets = append(secrets, value)
		}
	}
	stderr = redactText(stderr, secrets...)
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
