package claude

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/weston6142/watchtower/internal/agentprotocol"
	"github.com/weston6142/watchtower/internal/deps"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/pkgs"
	"github.com/weston6142/watchtower/internal/runner"
)

// maxLineBytes bounds a single stream-json line; the CLI can emit large
// assistant messages that exceed bufio.Scanner's default 64KiB limit.
const maxLineBytes = 1 << 20

// ThinkingTokens is the thinking-token budget an effort level maps to, as a
// bare number. Exported for the setup inspector, which reports the budget
// alongside the effort name; EffortEnv wraps it for the CLI so the two can
// never disagree. Empty or unknown levels return "" (CLI default).
func ThinkingTokens(effort string) string {
	switch effort {
	case "low":
		return "1024"
	case "medium":
		return "8192"
	case "high":
		return "32768"
	}
	return ""
}

// EffortEnv maps a package effort level to the CLI's thinking-budget env var.
// Empty or unknown levels return "" (CLI default).
func EffortEnv(effort string) string {
	if tokens := ThinkingTokens(effort); tokens != "" {
		return "MAX_THINKING_TOKENS=" + tokens
	}
	return ""
}

// CodeRunner drives a claude CLI subprocess in stream-json mode, translating
// its output into runner.Result and decision markers into runner.Ask.
type CodeRunner struct {
	Bin             string
	Packages        map[string]pkgs.Package
	ExtraEnv        []string
	OnProposal      func(string, runner.Proposal)
	OnProposalBatch func(string, []runner.Proposal)
	OnLine          func(issueID, stage, line string)
}

func (c *CodeRunner) Run(ctx context.Context, issueID, stage, agentPkg, workdir string,
	asks chan<- runner.Ask) <-chan runner.Result {
	done := make(chan runner.Result, 1)
	go func() {
		done <- c.runWithGate(ctx, issueID, stage, agentPkg, workdir, asks, nil)
	}()
	return done
}

func (c *CodeRunner) RunPlanner(ctx context.Context, issueID, stage, agentPkg, workdir string,
	asks chan<- runner.Ask, gate runner.ExplorationGate) <-chan runner.Result {
	done := make(chan runner.Result, 1)
	go func() {
		done <- c.runWithGate(ctx, issueID, stage, agentPkg, workdir, asks, gate)
	}()
	return done
}

func (c *CodeRunner) runWithGate(ctx context.Context, issueID, stage, agentPkg, workdir string,
	asks chan<- runner.Ask, gate runner.ExplorationGate) runner.Result {
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
	extraEnv := append([]string(nil), c.ExtraEnv...)
	if env := EffortEnv(pkg.Effort); env != "" {
		extraEnv = append(extraEnv, env)
	}
	cmd.Env = runner.MergeEnvironment(os.Environ(), extraEnv, runner.ManagedEnvironment(ctx))
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

	task := agentprotocol.TaskMessage(stage, issueID)
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
	sessionDone := false
	coachCount := 0
	decisionAccepted := false
	var stageResults agentprotocol.StageResultCollector
	var pendingTools []runner.ToolDecision
	// Replies to the agent (decision answers, coaching) are deferred until the
	// current turn's result event. A message written mid-turn is absorbed into
	// the running turn as steering — it never starts a new turn — so an agent
	// that emits a decision and keeps working would leave the runner waiting
	// forever for a turn that never comes.
	var pendingReplies []string
	// Prose first, then the tool calls it introduced: that is the order the
	// agent produced them, and a tool-only message must not write a blank.
	emit := func(ev StreamEvent) {
		if c.OnLine == nil {
			return
		}
		if ev.Text != "" {
			for _, line := range strings.Split(ev.Text, "\n") {
				c.OnLine(issueID, stage, line)
			}
		}
		for _, tool := range ev.Tools {
			c.OnLine(issueID, stage, tool)
		}
	}
	for sc.Scan() {
		ev := ParseLine(sc.Bytes())
		switch ev.Kind {
		case KindInit:
			res.SessionID = ev.SessionID
		case KindAssistantText:
			if res.Err != nil {
				continue
			}
			decisions, err := admitTools(ctx, gate, ev.ToolCalls)
			if err != nil {
				return abort(err)
			}
			pendingTools = append(pendingTools, decisions...)
			emit(ev)
			if err := stageResults.Collect(ev.Text); err != nil {
				res.FailureClass = runner.FailureProtocol
				return abort(err)
			}
			if d, found := agentprotocol.ExtractDecision(ev.Text); found {
				incomplete := agentprotocol.DecisionNeedsCoaching(d)
				if incomplete {
					if coachCount >= 2 {
						res.Err = fmt.Errorf("claude decision remained incomplete after 2 coaching attempts")
						continue
					}
					coachCount++
					pendingReplies = append(pendingReplies, agentprotocol.CoachMessage)
					continue
				}
				reply := make(chan levers.Response, 1)
				failure := make(chan error, 1)
				select {
				case asks <- runner.Ask{Decision: d, Reply: reply, Error: failure}:
				case <-ctx.Done():
					return abort(ctx.Err())
				}
				var response levers.Response
				select {
				case response = <-reply:
				case err := <-failure:
					return abort(err)
				case <-ctx.Done():
					return abort(ctx.Err())
				}
				if !d.Accepts(response) {
					return abort(fmt.Errorf("invalid response for decision %q", d.Question))
				}
				decisionAccepted = true
				answer := response.Text
				if response.Kind == levers.DecisionChoice {
					answer = d.Options[*response.Option]
				}
				pendingReplies = append(pendingReplies, "Human decision: "+answer)
			}
			if p, found := agentprotocol.ExtractProposal(ev.Text); found && c.OnProposal != nil {
				c.OnProposal(issueID, p)
			}
			if batch, found := agentprotocol.ExtractProposalBatch(ev.Text); found && c.OnProposalBatch != nil {
				c.OnProposalBatch(issueID, batch)
			}
			if dependsOn, found := agentprotocol.ExtractDependency(ev.Text); found {
				if !decisionAccepted {
					return abort(fmt.Errorf("dependency marker emitted without an accepted decision"))
				}
				res.DependsOn = deps.Normalize(append(res.DependsOn, dependsOn...))
			}
		case KindToolUse:
			if res.Err != nil {
				continue
			}
			decisions, err := admitTools(ctx, gate, ev.ToolCalls)
			if err != nil {
				return abort(err)
			}
			pendingTools = append(pendingTools, decisions...)
			emit(ev)
		case KindResult:
			res.Tokens += ev.Tokens
			res.TokensKnown = res.TokensKnown || ev.TokensKnown
			if ev.IsError && res.Err == nil {
				res.Err = fmt.Errorf("claude session %s ended with error", res.SessionID)
			}
			if len(pendingTools) > 0 {
				var actual *int64
				if len(pendingTools) == 1 && ev.TokensKnown {
					value := int64(ev.Tokens)
					actual = &value
				}
				for _, decision := range pendingTools {
					if err := gate.Complete(ctx, decision, actual, res.Err); err != nil {
						return abort(err)
					}
				}
				pendingTools = nil
			}
			if c.OnLine != nil {
				c.OnLine(issueID, stage, fmt.Sprintf("— turn complete (%d tokens) —", ev.Tokens))
			}
			gotResult = true
			if len(pendingReplies) > 0 && stageResults.Evidence() != nil {
				res.FailureClass = runner.FailureProtocol
				return abort(fmt.Errorf("watchtower_stage_result marker is only valid on the final assistant turn"))
			}
			// In stream-json input mode the CLI emits one result per turn and
			// then waits for more input. Send any deferred replies now — the
			// CLI is idle, so each starts a fresh turn. A turn with no reply
			// owed is the agent's final turn — close stdin so the process
			// exits.
			if len(pendingReplies) == 0 || res.Err != nil {
				sessionDone = true
			} else {
				for _, msg := range pendingReplies {
					if _, err := stdin.Write(UserMessage(msg)); err != nil {
						return abort(err)
					}
				}
				pendingReplies = nil
			}
		}
		if sessionDone {
			break
		}
	}
	if len(pendingTools) > 0 && gate != nil {
		for _, decision := range pendingTools {
			if err := gate.Complete(ctx, decision, nil, res.Err); err != nil && res.Err == nil {
				res.Err = err
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
	if res.Err == nil {
		res.StageEvidence = stageResults.Evidence()
	}
	return res
}

func admitTools(ctx context.Context, gate runner.ExplorationGate, calls []runner.ToolCall) ([]runner.ToolDecision, error) {
	if gate == nil {
		return nil, nil
	}
	var decisions []runner.ToolDecision
	for _, call := range calls {
		decision, err := gate.Admit(ctx, call)
		if err != nil {
			return nil, err
		}
		if !decision.Allowed {
			continue
		}
		decisions = append(decisions, decision)
	}
	return decisions, nil
}

func (c *CodeRunner) SetOnLine(fn func(issueID, stage, line string)) { c.OnLine = fn }
