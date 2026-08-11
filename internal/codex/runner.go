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
	FallbackProfile *repocfg.CodexProfile
	AttemptSink     runner.AttemptSink
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

type turnResult struct {
	threadID     string
	tokens       int
	tokensKnown  bool
	events       []Event
	leases       []runner.ToolDecision
	failed       error
	failureClass runner.FailureClass
	invocation   invocation
}

func (c *CodeRunner) runWithGate(ctx context.Context, issueID, stage, agentPkg, workdir string,
	asks chan<- runner.Ask, gate runner.ExplorationGate) runner.Result {
	pkg, ok := c.Packages[agentPkg]
	if !ok {
		return runner.Result{Err: fmt.Errorf("unknown agent package %q", agentPkg), FailureClass: runner.FailureConfiguration}
	}
	primary, fallback, err := c.effectiveProfiles(pkg)
	if err != nil {
		return runner.Result{Err: err, FailureClass: runner.FailureConfiguration, NextAction: "correct the Codex profile before launching a stage"}
	}
	operationID := runner.OperationID(ctx)
	if c.AttemptSink != nil && operationID != "" {
		prior, loadErr := c.AttemptSink.LoadOperation(ctx, operationID)
		if loadErr != nil {
			return runner.Result{Err: fmt.Errorf("load Codex attempt %s: %w", operationID, loadErr), FailureClass: runner.FailureUnknown}
		}
		for _, attempt := range prior {
			if attempt.Kind == runner.AttemptFallback && attempt.State != runner.AttemptFailed {
				return runner.Result{
					Err:          fmt.Errorf("Codex operation %s already consumed fallback; press the existing retry command for a new operation", operationID),
					FailureClass: attempt.FailureClass, FallbackConsumed: true, Attempt: attempt, Attempts: prior,
					NextAction: "press the existing retry command for a new operation",
				}
			}
		}
	}

	primaryAttempt := runner.Attempt{
		OperationID: operationID, IssueID: issueID, Stage: stage, AgentPackage: agentPkg,
		Kind: runner.AttemptPrimary, State: runner.AttemptRunning,
	}
	if err := c.recordAttempt(ctx, primaryAttempt); err != nil {
		return runner.Result{Err: fmt.Errorf("record primary Codex attempt: %w", err), FailureClass: runner.FailureUnknown, Attempt: primaryAttempt}
	}
	primaryResult, continuation := c.runProfile(ctx, issueID, stage, pkg, workdir, primary, nil, asks, gate)
	primaryAttempt = updateAttempt(primaryAttempt, primaryResult, runner.AttemptRunning)
	if primaryResult.Err == nil {
		primaryAttempt.State = runner.AttemptSucceeded
		if err := c.recordAttempt(ctx, primaryAttempt); err != nil {
			primaryResult.Err = fmt.Errorf("record successful Codex attempt: %w", err)
			primaryResult.FailureClass = runner.FailureUnknown
			primaryAttempt.State = runner.AttemptTerminal
		}
		primaryResult.Attempt = primaryAttempt
		primaryResult.Attempts = append(primaryResult.Attempts, primaryAttempt)
		return primaryResult
	}

	primaryClass := primaryResult.FailureClass
	if primaryClass == "" {
		primaryClass = runner.FailureUnknown
	}
	primaryAttempt.FailureClass = primaryClass
	primaryAttempt.State = runner.AttemptFailed
	if err := c.recordAttempt(ctx, primaryAttempt); err != nil {
		return terminalResult(primaryResult, primaryAttempt, nil, false, fmt.Errorf("record failed primary Codex attempt: %w", err))
	}
	if fallback == nil || !eligibleFailure(primaryClass) {
		primaryAttempt.State = runner.AttemptTerminal
		_ = c.recordAttempt(ctx, primaryAttempt)
		primaryResult.Attempt = primaryAttempt
		primaryResult.Attempts = append(primaryResult.Attempts, primaryAttempt)
		primaryResult.FailureClass = primaryClass
		primaryResult.NextAction = "restore the Codex configuration or press the existing retry command"
		primaryResult.Err = fmt.Errorf("Codex primary attempt failed (%s): %w; next action: %s", primaryClass, primaryResult.Err, primaryResult.NextAction)
		return primaryResult
	}

	fallbackAttempt := runner.Attempt{
		OperationID: operationID, IssueID: issueID, Stage: stage, AgentPackage: agentPkg,
		Kind: runner.AttemptFallback, State: runner.AttemptReserved,
		FailureClass: primaryClass,
	}
	if err := c.recordAttempt(ctx, fallbackAttempt); err != nil {
		return terminalResult(primaryResult, primaryAttempt, &fallbackAttempt, true, fmt.Errorf("reserve Codex fallback: %w", err))
	}
	fallbackAttempt.State = runner.AttemptRunning
	if err := c.recordAttempt(ctx, fallbackAttempt); err != nil {
		return terminalResult(primaryResult, primaryAttempt, &fallbackAttempt, true, fmt.Errorf("start Codex fallback: %w", err))
	}
	fallbackResult, _ := c.runProfile(ctx, issueID, stage, pkg, workdir, *fallback, &continuation, asks, gate)
	fallbackAttempt = updateAttempt(fallbackAttempt, fallbackResult, runner.AttemptRunning)
	fallbackAttempt.FailureClass = fallbackResult.FailureClass
	fallbackResult.FallbackConsumed = true
	fallbackResult.Tokens += primaryResult.Tokens
	fallbackResult.TokensKnown = fallbackResult.TokensKnown || primaryResult.TokensKnown
	if fallbackResult.SessionID == "" {
		fallbackResult.SessionID = primaryResult.SessionID
	}
	fallbackResult.DependsOn = deps.Normalize(append(primaryResult.DependsOn, fallbackResult.DependsOn...))
	fallbackResult.Attempts = append(primaryResult.Attempts, primaryAttempt)
	fallbackResult.Attempts = append(fallbackResult.Attempts, fallbackAttempt)
	if fallbackResult.Err == nil {
		fallbackAttempt.State = runner.AttemptSucceeded
		if err := c.recordAttempt(ctx, fallbackAttempt); err != nil {
			fallbackResult.Err = fmt.Errorf("record successful Codex fallback: %w", err)
			fallbackAttempt.State = runner.AttemptTerminal
		}
		fallbackResult.Attempt = fallbackAttempt
		return fallbackResult
	}
	fallbackAttempt.State = runner.AttemptTerminal
	if err := c.recordAttempt(ctx, fallbackAttempt); err != nil {
		fallbackResult.Err = fmt.Errorf("%w; record fallback outcome: %v", fallbackResult.Err, err)
	}
	fallbackResult.Attempt = fallbackAttempt
	fallbackResult.FailureClass = fallbackAttempt.FailureClass
	fallbackResult.NextAction = "inspect both Codex attempts, correct the profile, and press the existing retry command"
	fallbackResult.Err = fmt.Errorf("Codex primary attempt failed (%s); fallback attempt failed (%s): %w; next action: %s",
		primaryClass, fallbackAttempt.FailureClass, fallbackResult.Err, fallbackResult.NextAction)
	return fallbackResult
}

type runContinuation struct {
	threadID         string
	prompt           string
	coachCount       int
	decisionAccepted bool
}

func (c *CodeRunner) runProfile(ctx context.Context, issueID, stage string, pkg pkgs.Package,
	workdir string, profile repocfg.CodexProfile, start *runContinuation,
	asks chan<- runner.Ask, gate runner.ExplorationGate) (runner.Result, runContinuation) {
	var res runner.Result
	threadID := ""
	prompt := agentprotocol.TaskMessage(stage, issueID)
	coachCount := 0
	decisionAccepted := false
	if start != nil {
		threadID = start.threadID
		prompt = start.prompt
		coachCount = start.coachCount
		decisionAccepted = start.decisionAccepted
	}
	continuation := func() runContinuation {
		return runContinuation{threadID: threadID, prompt: prompt, coachCount: coachCount, decisionAccepted: decisionAccepted}
	}

	for {
		var stageResults agentprotocol.StageResultCollector
		turn := c.runTurn(ctx, workdir, pkg, profile, threadID, prompt, gate)
		res.Attempt.RedactedArgv = turn.invocation.RedactedArgv
		if turn.threadID != "" {
			if threadID != "" && turn.threadID != threadID {
				res.Err = fmt.Errorf("codex resume returned thread %q, want %q", turn.threadID, threadID)
				res.FailureClass = runner.FailureResumeIdentity
				return res, continuation()
			}
			if threadID == "" {
				threadID = turn.threadID
				res.SessionID = threadID
			}
		}
		res.Tokens += turn.tokens
		res.TokensKnown = res.TokensKnown || turn.tokensKnown
		if turn.failed != nil {
			res.Err = turn.failed
			res.FailureClass = turn.failureClass
			return res, continuation()
		}

		var decision *levers.Decision
		unstructuredDecision := false
		for _, event := range turn.events {
			switch event.Kind {
			case KindText:
				c.emitText(issueID, stage, event.Text)
				if err := stageResults.Collect(event.Text); err != nil {
					res.Err = err
					res.FailureClass = runner.FailureProtocol
					return res, continuation()
				}
				if decision == nil {
					if parsed, found := agentprotocol.ExtractDecision(event.Text); found {
						decision = &parsed
					} else if agentprotocol.UnstructuredDecisionRequestNeedsCoaching(event.Text) {
						unstructuredDecision = true
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
						res.FailureClass = runner.FailureProtocol
						return res, continuation()
					}
					res.DependsOn = deps.Normalize(append(res.DependsOn, dependsOn...))
				}
			case KindTool:
				if c.OnLine != nil && strings.TrimSpace(event.Tool) != "" {
					c.OnLine(issueID, stage, event.Tool)
				}
			}
		}
		if stageResults.Evidence() != nil && (decision != nil || unstructuredDecision) {
			res.Err = fmt.Errorf("watchtower_stage_result marker is only valid on the final assistant turn")
			res.FailureClass = runner.FailureProtocol
			return res, continuation()
		}

		if decision == nil && unstructuredDecision {
			if coachCount >= 2 {
				res.Err = fmt.Errorf("codex decision remained unstructured after 2 coaching attempts")
				res.FailureClass = runner.FailureProtocol
				return res, continuation()
			}
			coachCount++
			prompt = agentprotocol.UnstructuredDecisionCoachMessage
			continue
		}
		if decision == nil {
			res.StageEvidence = stageResults.Evidence()
			return res, continuation()
		}
		d := *decision
		incomplete := agentprotocol.DecisionNeedsCoaching(d)
		if incomplete {
			if coachCount >= 2 {
				res.Err = fmt.Errorf("codex decision remained incomplete after 2 coaching attempts")
				res.FailureClass = runner.FailureProtocol
				return res, continuation()
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
			res.FailureClass = runner.FailureCancellation
			return res, continuation()
		}
		var response levers.Response
		select {
		case response = <-reply:
		case err := <-failure:
			res.Err = err
			res.FailureClass = runner.FailureUnknown
			return res, continuation()
		case <-ctx.Done():
			res.Err = ctx.Err()
			res.FailureClass = runner.FailureCancellation
			return res, continuation()
		}
		if !d.Accepts(response) {
			res.Err = fmt.Errorf("invalid response for decision %q", d.Question)
			res.FailureClass = runner.FailureProtocol
			return res, continuation()
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
	profile repocfg.CodexProfile, threadID, prompt string, gate runner.ExplorationGate) turnResult {
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
	extraEnv := append([]string(nil), c.ExtraEnv...)
	cmd.Env = runner.MergeEnvironment(os.Environ(), extraEnv, runner.ManagedEnvironment(ctx))
	// If a shell wrapper leaves a child holding the JSONL pipe open after
	// cancellation, do not let that child defeat CommandContext cancellation.
	cmd.WaitDelay = 250 * time.Millisecond
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return turnResult{failed: fmt.Errorf("codex stdout: %w", err), failureClass: runner.FailureTransport, invocation: invocation}
	}
	var stderrTail tailBuffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrTail)
	if err := cmd.Start(); err != nil {
		return turnResult{failed: fmt.Errorf("codex start: %w", err), failureClass: runner.FailureLaunch, invocation: invocation}
	}

	result := turnResult{invocation: invocation}
	reconcile := func() {
		if gate == nil {
			return
		}
		var actual *int64
		if len(result.leases) == 1 && result.tokensKnown {
			value := int64(result.tokens)
			actual = &value
		}
		for _, decision := range result.leases {
			if err := gate.Complete(ctx, decision, actual, result.failed); err != nil && result.failed == nil {
				result.failed = err
			}
		}
	}
	gotComplete := false
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), maxLineBytes)
	stopReading := false
	for scanner.Scan() {
		event := ParseLine(scanner.Bytes())
		switch event.Kind {
		case KindThread:
			result.threadID = event.ThreadID
		case KindToolRequest:
			if gate == nil {
				continue
			}
			decision, err := gate.Admit(ctx, event.ToolCall)
			if err != nil {
				result.failed = err
				_ = cmd.Process.Kill()
				stopReading = true
				continue
			}
			if !decision.Allowed {
				continue
			}
			result.leases = append(result.leases, decision)
		case KindText, KindTool:
			result.events = append(result.events, event)
		case KindComplete:
			result.tokens += event.Tokens
			result.tokensKnown = result.tokensKnown || event.TokensKnown
			gotComplete = true
		case KindFailed:
			result.failed = fmt.Errorf("codex turn failed: %s", event.Error)
			result.failureClass = event.FailureClass
			if result.failureClass == "" {
				result.failureClass = classifyFailureMessage(event.Error)
			}
		}
		if stopReading {
			break
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if ctx.Err() != nil {
			result.failed = c.withStderr(fmt.Errorf("codex: %w", ctx.Err()), stderrTail.String(), pkg.Prompt, prompt, threadID)
			result.failureClass = runner.FailureCancellation
		} else {
			result.failed = c.withStderr(fmt.Errorf("codex JSONL: %w", scanErr), stderrTail.String(), pkg.Prompt, prompt, threadID)
			result.failureClass = runner.FailureTransport
		}
		reconcile()
		return result
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		result.failed = fmt.Errorf("codex: %w", ctx.Err())
		result.failureClass = runner.FailureCancellation
	} else if result.failed == nil && waitErr != nil {
		result.failed = fmt.Errorf("codex exited: %w", waitErr)
		result.failureClass = runner.FailureExecution
	} else if result.failed == nil && threadID == "" && result.threadID == "" {
		result.failed = fmt.Errorf("codex ended without thread ID")
		result.failureClass = runner.FailureProtocol
	} else if result.failed == nil && !gotComplete {
		activeThread := threadID
		if activeThread == "" {
			activeThread = result.threadID
		}
		result.failed = fmt.Errorf("codex thread %s ended without completion event", activeThread)
		result.failureClass = runner.FailureProtocol
	}
	reconcile()
	if result.failed != nil {
		result.failed = c.withStderr(result.failed, stderrTail.String(), pkg.Prompt, prompt, threadID)
	}
	return result
}

func (c *CodeRunner) effectiveProfiles(pkg pkgs.Package) (repocfg.CodexProfile, *repocfg.CodexProfile, error) {
	primary := c.effectiveProfile(pkg)
	if err := repocfg.ValidateCodexFeatures("codex.primary", primary.FeatureOverrides); err != nil {
		return repocfg.CodexProfile{}, nil, err
	}
	if c.FallbackProfile == nil {
		return primary, nil, nil
	}
	fallback := *c.FallbackProfile
	configuredPrimary := c.PrimaryProfile
	if configuredPrimary.Bin == "" {
		configuredPrimary.Bin = c.Bin
	}
	if configuredPrimary.Model == "" {
		configuredPrimary.Model = c.DefaultModel
	}
	if configuredPrimary.Effort == "" {
		configuredPrimary.Effort = c.DefaultEffort
	}
	if fallback.Bin != "" && fallback.Bin != configuredPrimary.Bin {
		return repocfg.CodexProfile{}, nil, fmt.Errorf("codex.fallback.bin must match the primary Codex binary")
	}
	if fallback.Model != "" && fallback.Model != configuredPrimary.Model {
		return repocfg.CodexProfile{}, nil, fmt.Errorf("codex.fallback.model must match the primary Codex model")
	}
	if fallback.Effort != "" && fallback.Effort != configuredPrimary.Effort {
		return repocfg.CodexProfile{}, nil, fmt.Errorf("codex.fallback.effort must match the primary Codex effort")
	}
	fallback.Bin, fallback.Model, fallback.Effort = primary.Bin, primary.Model, primary.Effort
	if err := repocfg.ValidateCodexFeatures("codex.fallback", fallback.FeatureOverrides); err != nil {
		return repocfg.CodexProfile{}, nil, err
	}
	if equalFeatures(primary.FeatureOverrides, fallback.FeatureOverrides) {
		return repocfg.CodexProfile{}, nil, fmt.Errorf("codex.fallback is ineffective; it must differ from the primary feature profile")
	}
	return primary, &fallback, nil
}

func equalFeatures(left, right map[string]bool) bool {
	if len(left) != len(right) {
		return false
	}
	for name, value := range left {
		if other, ok := right[name]; !ok || other != value {
			return false
		}
	}
	return true
}

func eligibleFailure(class runner.FailureClass) bool {
	return class == runner.FailureLaunch || class == runner.FailureExecution || class == runner.FailureTransport
}

func (c *CodeRunner) recordAttempt(ctx context.Context, attempt runner.Attempt) error {
	if c.AttemptSink == nil {
		return nil
	}
	return c.AttemptSink.RecordAttempt(ctx, attempt)
}

func updateAttempt(attempt runner.Attempt, result runner.Result, state runner.AttemptState) runner.Attempt {
	attempt.State = state
	attempt.FailureClass = result.FailureClass
	attempt.SessionID = result.SessionID
	attempt.Tokens = result.Tokens
	attempt.RedactedArgv = append([]string(nil), result.Attempt.RedactedArgv...)
	return attempt
}

func terminalResult(result runner.Result, primary runner.Attempt, fallback *runner.Attempt,
	consumed bool, err error) runner.Result {
	result.Err = err
	result.FailureClass = runner.FailureUnknown
	result.FallbackConsumed = consumed
	primary.State = runner.AttemptTerminal
	result.Attempts = append(result.Attempts, primary)
	if fallback != nil {
		fallback.State = runner.AttemptTerminal
		result.Attempts = append(result.Attempts, *fallback)
		result.Attempt = *fallback
	} else {
		result.Attempt = primary
	}
	return result
}

func classifyFailureMessage(message string) runner.FailureClass {
	message = strings.ToLower(message)
	switch {
	case strings.Contains(message, "authentication") || strings.Contains(message, "auth failed"):
		return runner.FailureAuthentication
	case strings.Contains(message, "authorization") || strings.Contains(message, "forbidden") || strings.Contains(message, "permission denied"):
		return runner.FailureAuthorization
	case strings.Contains(message, "canceled") || strings.Contains(message, "cancelled"):
		return runner.FailureCancellation
	case strings.Contains(message, "resume") && strings.Contains(message, "thread"):
		return runner.FailureResumeIdentity
	default:
		return runner.FailureExecution
	}
}

func (c *CodeRunner) SetAttemptSink(sink runner.AttemptSink) { c.AttemptSink = sink }

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

func (c *CodeRunner) withStderr(base error, stderr string, secrets ...string) error {
	for _, entry := range c.ExtraEnv {
		if _, value, ok := strings.Cut(entry, "="); ok && value != "" {
			secrets = append(secrets, value)
		}
	}
	redactedBase := redactText(base.Error(), secrets...)
	if redactedBase != base.Error() {
		base = &redactedError{message: redactedBase, cause: base}
	}
	stderr = redactText(stderr, secrets...)
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return base
	}
	return fmt.Errorf("%w: %s", base, stderr)
}

type redactedError struct {
	message string
	cause   error
}

func (e *redactedError) Error() string { return e.message }

func (e *redactedError) Unwrap() error { return e.cause }

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
