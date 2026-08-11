package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/agentprotocol"
	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/pkgs"
	"github.com/weston6142/watchtower/internal/plannerartifact"
	"github.com/weston6142/watchtower/internal/repocfg"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/store"
)

func TestCodeRunnerReturnsStageResultEvidence(t *testing.T) {
	marker := codexStageResultMarker("implemented")
	bin := writeStub(t, strings.Join([]string{
		codexJSONLine(map[string]any{"type": "thread.started", "thread_id": "thr-result"}),
		codexJSONLine(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": marker}}),
		codexJSONLine(map[string]any{"type": "turn.completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}),
	}, "\n"))

	r := testRunner(bin)
	res := <-r.Run(context.Background(), testStageRequest(r, "GH-67", "execute", "executor", t.TempDir()), make(chan runner.Ask))
	if res.Err != nil || res.StageEvidence == nil {
		t.Fatalf("result = %+v", res)
	}
	if got := res.StageEvidence.Execute.PlanTasks[0].Summary; got != "implemented" {
		t.Fatalf("task summary = %q", got)
	}
}

func TestCodeRunnerRejectsConflictingStageResultMarkers(t *testing.T) {
	bin := writeStub(t, strings.Join([]string{
		codexJSONLine(map[string]any{"type": "thread.started", "thread_id": "thr-conflict"}),
		codexJSONLine(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": codexStageResultMarker("first")}}),
		codexJSONLine(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": codexStageResultMarker("different")}}),
		codexJSONLine(map[string]any{"type": "turn.completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}),
	}, "\n"))

	r := testRunner(bin)
	res := <-r.Run(context.Background(), testStageRequest(r, "GH-67", "execute", "executor", t.TempDir()), make(chan runner.Ask))
	if res.Err == nil || res.FailureClass != runner.FailureProtocol || res.StageEvidence != nil {
		t.Fatalf("result = %+v", res)
	}
}

func TestCodeRunnerRejectsStageResultBeforeFinalTurn(t *testing.T) {
	decision := `{"watchtower_decision":{"kind":"choice","question":"Apply the repair?","options":["Apply","Hold"],"recommended":0,"why":"The repair closes the gap.","consequences":["The repair is applied.","The stage remains incomplete."],"reversible":"Before the repair is committed."}}`
	bin, state := statefulStub(t,
		strings.Join([]string{
			codexJSONLine(map[string]any{"type": "thread.started", "thread_id": "thr-premature-result"}),
			codexJSONLine(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": codexStageResultMarker("before decision") + "\n" + decision}}),
			codexJSONLine(map[string]any{"type": "turn.completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}),
		}, "\n"),
		strings.Join([]string{
			codexJSONLine(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": "decision applied"}}),
			codexJSONLine(map[string]any{"type": "turn.completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}),
		}, "\n"),
	)
	r := testRunner(bin)
	r.ExtraEnv = []string{"STATE=" + state}
	done, asks := stageRun(r, "executor", "execute")
	select {
	case ask := <-asks:
		ask.Reply <- levers.ChoiceResponse(0)
	case res := <-done:
		if res.Err == nil || res.FailureClass != runner.FailureProtocol {
			t.Fatalf("result = %+v", res)
		}
		return
	}
	res := <-done
	if res.Err == nil || res.FailureClass != runner.FailureProtocol || res.StageEvidence != nil {
		t.Fatalf("result = %+v", res)
	}
}

func codexStageResultMarker(summary string) string {
	evidence := map[string]any{
		"schema_version": 1, "stage_kind": "execute", "outcome": "completed",
		"remaining_work": []any{}, "remaining_concerns": []any{},
		"execute": map[string]any{
			"plan_tasks": []any{map[string]any{"id": "task-0001", "outcome": "completed", "summary": summary}},
			"commits":    []any{}, "checks": []any{},
			"skips": []any{
				map[string]any{"activity": "commits", "explanation": "the fixture makes no repository change"},
				map[string]any{"activity": "checks", "explanation": "provider transport is the behavior under test"},
			},
		},
	}
	body, _ := json.Marshal(map[string]any{"watchtower_stage_result": evidence})
	return string(body)
}

func codexJSONLine(value any) string {
	body, _ := json.Marshal(value)
	return "printf '%s\\n' '" + string(body) + "'"
}

func writeStub(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex-stub")
	script := "#!/bin/sh\nset -eu\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testRunner(bin string) *CodeRunner {
	return &CodeRunner{
		Bin: bin,
		Packages: map[string]pkgs.Package{
			"executor": {Name: "executor", Prompt: "Implement and verify."},
		},
		DefaultModel:  "gpt-5.6-luna",
		DefaultEffort: "xhigh",
	}
}

func testStageRequest(r *CodeRunner, issueID, stage, agent, workdir string) runner.StageRequest {
	contract := capability.CompiledContract{
		ContractID: "contract-" + issueID + "-" + stage, AuthorityDigest: "authority-test",
		Contract: capability.Contract{
			Version: capability.ContractVersion, EnginePolicyVersion: capability.EnginePolicyVersion,
			IssueID: issueID, Stage: stage, AttemptID: "attempt-test", Profile: "implementation",
			WorkspaceRoot: workdir,
			Operations: []capability.OperationClass{
				capability.OpWorkspaceRead, capability.OpWorkspaceMutate, capability.OpLocalProcess,
				capability.OpVCSRead, capability.OpVCSCommit, capability.OpPlannerArtifactApply,
			},
		},
	}
	plan, err := r.Preflight(context.Background(), runner.PreflightRequest{
		IssueID: issueID, Stage: stage, Agent: agent, Workdir: workdir, Contract: contract,
	})
	if err != nil {
		panic(err)
	}
	return runner.StageRequest{IssueID: issueID, Stage: stage, Agent: agent, Workdir: workdir, Contract: contract, Plan: plan}
}

type recordingGate struct {
	completed int
	actual    *int64
	failed    error
}

func (g *recordingGate) Admit(context.Context, runner.ToolCall) (runner.ToolDecision, error) {
	return runner.ToolDecision{Allowed: true, LeaseID: "lease-1"}, nil
}

func (g *recordingGate) Complete(_ context.Context, _ runner.ToolDecision, actual *int64, operationErr error) error {
	g.completed++
	if actual != nil {
		value := *actual
		g.actual = &value
	}
	g.failed = operationErr
	return nil
}

func runTurn(t *testing.T, ctx context.Context, r *CodeRunner, workdir string) runner.Result {
	t.Helper()
	asks := make(chan runner.Ask, 1)
	return <-r.Run(ctx, testStageRequest(r, "GH-1", "execute", "executor", workdir), asks)
}

func successfulStub(prefix string) string {
	return prefix + `
printf '%s\n' '{"type":"thread.started","thread_id":"thr-happy"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"working\ndone"}}'
printf '%s\n' '{"type":"item.completed","item":{"type":"command_execution","command":"go test ./...","status":"completed"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":80,"output_tokens":25}}'`
}

func TestInitialTurnUsesExplicitCodexSettingsAndReportsBehavior(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "argv")
	bin := writeStub(t, successfulStub(`
: > "$CAPTURE"
for arg in "$@"; do printf '%s\n' "$arg" >> "$CAPTURE"; done`))
	r := testRunner(bin)
	r.ExtraEnv = []string{"CAPTURE=" + capture, "SENTINEL_SECRET=do-not-leak"}
	var lines []string
	r.OnLine = func(_, _, line string) { lines = append(lines, line) }
	workdir := t.TempDir()

	res := runTurn(t, context.Background(), r, workdir)
	if res.Err != nil || res.SessionID != "thr-happy" || res.Tokens != 125 {
		t.Fatalf("result = %+v", res)
	}
	if got, want := strings.Join(lines, "\n"), "working\ndone\n↳ command go test ./..."; got != want {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
	body, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	joined := strings.Join(args, "\n")
	for _, want := range []string{
		"exec", "--json", "-C", workdir, "-m", "gpt-5.6-luna",
		`model_reasoning_effort="xhigh"`,
		`sandbox_mode="read-only"`,
		`approval_policy="never"`,
		`tools.web_search=false`,
		`features.shell_tool=false`,
		`developer_instructions="Implement and verify."`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q:\n%s", want, joined)
		}
	}
	for _, forbidden := range []string{"--ignore-user-config", "--ignore-rules", "--ephemeral"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("argv contains %q:\n%s", forbidden, joined)
		}
	}
	if got, want := args[len(args)-1], agentprotocol.TaskMessage("execute", "GH-1"); got != want {
		t.Fatalf("final argument = %q, want %q", got, want)
	}
}

func TestPlannerSessionEnvironmentReachesCodexChildWithoutArgLeak(t *testing.T) {
	envCapture := filepath.Join(t.TempDir(), "planner-env")
	argvCapture := filepath.Join(t.TempDir(), "planner-argv")
	sentinel := "private-planner-session-value"
	bin := writeStub(t, successfulStub(`
	printf '%s' "${WATCHTOWER_PLANNER_SESSION-}" > "$CAPTURE_ENV"
: > "$CAPTURE_ARGV"
for arg in "$@"; do printf '%s\n' "$arg" >> "$CAPTURE_ARGV"; done`))
	r := testRunner(bin)
	r.ExtraEnv = []string{"CAPTURE_ENV=" + envCapture, "CAPTURE_ARGV=" + argvCapture}
	t.Setenv("WATCHTOWER_PLANNER_SESSION", sentinel)
	ctx := context.Background()
	workdir := t.TempDir()
	res := <-r.RunPlanner(ctx, testStageRequest(r, "GH-1", "plan", "executor", workdir), make(chan runner.Ask), nil)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if got, err := os.ReadFile(envCapture); err != nil {
		t.Fatal(err)
	} else if string(got) != "" {
		t.Fatalf("child planner environment = %q, want ambient session removed", got)
	}
	if got, err := os.ReadFile(argvCapture); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(got), sentinel) {
		t.Fatalf("planner session leaked into child argv: %q", got)
	}
}

func TestPlannerArtifactSubprocessDoesNotInheritDescriptor(t *testing.T) {
	workdir := t.TempDir()
	capture := filepath.Join(workdir, "descriptor")
	bin := writeStub(t, `
if (printf x >&3) 2>/dev/null; then
  printf 'attached' > "$CAPTURE"
else
  printf 'absent' > "$CAPTURE"
fi`+successfulStub(""))
	r := testRunner(bin)
	r.ExtraEnv = []string{"CAPTURE=" + capture}
	coordinator, err := store.Open(filepath.Join(t.TempDir(), "coordinator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	authority, err := plannerartifact.CreateOrLoad(coordinator, plannerartifact.Binding{IssueID: "GH-72", Stage: "plan", Attempt: 1, Worktree: workdir})
	if err != nil {
		t.Fatal(err)
	}
	result := <-r.RunPlanner(runner.WithPlannerArtifactAuthority(context.Background(), authority), testStageRequest(r, "GH-72", "plan", "executor", workdir), make(chan runner.Ask), nil)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if got, err := os.ReadFile(capture); err != nil {
		t.Fatal(err)
	} else if string(got) != "absent" {
		t.Fatalf("planner descriptor reached Codex child: %q", got)
	}
}

func TestManagedEnvironmentOverlayReachesCodexChild(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "environment")
	bin := writeStub(t, successfulStub(`
printf '%s\n' "$GOCACHE" "$GOMODCACHE" "$GOPATH" "$SENTINEL" > "$CAPTURE"`))
	r := testRunner(bin)
	r.ExtraEnv = []string{"GOCACHE=extra-cache", "GOMODCACHE=extra-mod", "GOPATH=extra-path", "SENTINEL=keep", "CAPTURE=" + capture}
	ctx := runner.WithManagedEnvironment(context.Background(), []string{
		"GOCACHE=lease-cache", "GOMODCACHE=lease-mod", "GOPATH=lease-path",
	})
	workdir := t.TempDir()
	res := <-r.Run(ctx, testStageRequest(r, "GH-48", "execute", "executor", workdir), make(chan runner.Ask))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if got, err := os.ReadFile(capture); err != nil {
		t.Fatal(err)
	} else if want := "lease-cache\nlease-mod\nlease-path\nkeep\n"; string(got) != want {
		t.Fatalf("Codex child environment = %q, want %q", got, want)
	}
}

func TestInitialAndResumedTurnsUseTheConfiguredFeatureOverride(t *testing.T) {
	bin, state := statefulStub(t,
		`printf '%s\n' '{"type":"thread.started","thread_id":"thr-feature"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"watchtower_decision\":{\"kind\":\"choice\",\"question\":\"Ship it?\",\"options\":[\"Ship it\",\"Hold\"],\"recommended\":0,\"why\":\"Ready.\",\"consequences\":[\"Ships.\",\"Waits.\"],\"reversible\":\"yes\"}}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`,
		`printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`,
	)
	r := testRunner(bin)
	r.PrimaryProfile = repocfg.CodexProfile{FeatureOverrides: map[string]bool{"unified_exec": false}}
	r.ExtraEnv = []string{"STATE=" + state}
	done, asks := stageRun(r, "executor", "execute")
	(<-asks).Reply <- levers.ChoiceResponse(0)
	if res := <-done; res.Err != nil {
		t.Fatal(res.Err)
	}
	for index := 1; index <= 2; index++ {
		args := readCapturedArgs(t, state, index)
		joined := strings.Join(args, "\n")
		if !strings.Contains(joined, "-c\nfeatures.unified_exec=false") {
			t.Fatalf("turn %d missing separate feature override: %s", index, joined)
		}
	}
	args := readCapturedArgs(t, state, 2)
	if len(args) < 4 || args[0] != "exec" || args[1] != "resume" || args[2] != "--json" ||
		!containsArg(args, "thr-feature") {
		t.Fatalf("resumed turn did not preserve the explicit thread token: %q", args)
	}
	if strings.Contains(strings.Join(args, "\n"), "--last") {
		t.Fatalf("resumed turn used --last: %q", args)
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func TestRedactedInvocationAndErrorEvidenceExcludeSecrets(t *testing.T) {
	const (
		workdir      = "/private/worktree/secret-worktree"
		pkgPrompt    = "developer instruction secret"
		turnPrompt   = "turn prompt secret"
		resumeID     = "thread-secret"
		envSecret    = "environment-secret"
		stderrSecret = "stderr-secret"
	)
	invocation := buildInvocation(repocfg.CodexProfile{
		Bin: "codex", Model: "gpt-5.6-luna", Effort: "xhigh",
		FeatureOverrides: map[string]bool{"unified_exec": false},
	}, turnDescriptor{
		Workdir: workdir, ResumeID: resumeID, PackagePrompt: pkgPrompt,
		Prompt: turnPrompt,
	})
	redacted := strings.Join(invocation.RedactedArgv, "\n")
	for _, secret := range []string{workdir, pkgPrompt, turnPrompt, resumeID} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("redacted argv leaked %q: %q", secret, redacted)
		}
	}
	if !strings.Contains(redacted, "features.unified_exec=false") || !strings.Contains(redacted, "exec") {
		t.Fatalf("redacted argv lost safe structure: %q", redacted)
	}

	bin := writeStub(t, "printf '%s\\n' '"+stderrSecret+"' >&2\nexit 7")
	r := testRunner(bin)
	r.PrimaryProfile = repocfg.CodexProfile{FeatureOverrides: map[string]bool{"unified_exec": false}}
	r.ExtraEnv = []string{"SENTINEL_SECRET=" + envSecret}
	res := runTurn(t, context.Background(), r, t.TempDir())
	if res.Err == nil {
		t.Fatal("expected stub failure")
	}
	for _, secret := range []string{pkgPrompt, turnPrompt, envSecret, stderrSecret} {
		if strings.Contains(res.Err.Error(), secret) {
			t.Fatalf("error evidence leaked %q: %q", secret, res.Err)
		}
	}
}

func TestStructuredCodexFailureEvidenceExcludesSecrets(t *testing.T) {
	const eventSecret = "event-token-secret"
	bin := writeStub(t, `printf '%s\n' '{"type":"thread.started","thread_id":"thr-secret"}'
printf '%s\n' '{"type":"turn.failed","error":{"message":"request token event-token-secret"}}'`)
	res := runTurn(t, context.Background(), testRunner(bin), t.TempDir())
	if res.Err == nil {
		t.Fatal("expected structured Codex failure")
	}
	if strings.Contains(res.Err.Error(), eventSecret) {
		t.Fatalf("structured failure leaked %q: %v", eventSecret, res.Err)
	}
}

func TestTerminalLaunchFailureReturnsTypedAttemptOutcome(t *testing.T) {
	r := testRunner(filepath.Join(t.TempDir(), "missing-codex"))
	res := runTurn(t, context.Background(), r, t.TempDir())
	if res.Err == nil || res.FailureClass != runner.FailureLaunch {
		t.Fatalf("result = %+v, want launch failure", res)
	}
	if res.Attempt.Kind != runner.AttemptPrimary || res.Attempt.State != runner.AttemptTerminal || res.FallbackConsumed {
		t.Fatalf("attempt outcome = %+v, want terminal primary without fallback", res.Attempt)
	}
	if !strings.Contains(res.Err.Error(), "restore") && !strings.Contains(res.Err.Error(), "Codex") {
		t.Fatalf("launch error is not actionable: %v", res.Err)
	}
}

func TestEligibleFailureUsesExactlyOneFallbackAndCompletes(t *testing.T) {
	bin, state := statefulStub(t,
		`exit 7`,
		successfulStub(""),
	)
	r := testRunner(bin)
	r.PrimaryProfile = repocfg.CodexProfile{FeatureOverrides: map[string]bool{"unified_exec": false}}
	r.FallbackProfile = &repocfg.CodexProfile{FeatureOverrides: map[string]bool{"unified_exec": true}}
	r.ExtraEnv = []string{"STATE=" + state}
	res := runTurn(t, context.Background(), r, t.TempDir())
	if res.Err != nil || !res.FallbackConsumed || res.Attempt.Kind != runner.AttemptFallback || res.Attempt.State != runner.AttemptSucceeded {
		t.Fatalf("result = %+v, want successful fallback", res)
	}
	if got := readCount(t, state); got != 2 {
		t.Fatalf("process attempts = %d, want one primary and one fallback", got)
	}
	if got := strings.Join(readCapturedArgs(t, state, 2), "\n"); !strings.Contains(got, "features.unified_exec=true") {
		t.Fatalf("fallback argv = %q", got)
	}
}

func TestConfiguredFallbackFollowsPackageModelAndEffortOverrides(t *testing.T) {
	bin, state := statefulStub(t,
		`exit 7`,
		successfulStub(""),
	)
	r := testRunner(bin)
	r.Packages["executor"] = pkgs.Package{
		Name: "executor", Prompt: "Implement and verify.", Model: "package-model", Effort: "high",
	}
	r.PrimaryProfile = repocfg.CodexProfile{
		Bin: bin, Model: r.DefaultModel, Effort: r.DefaultEffort,
		FeatureOverrides: map[string]bool{"unified_exec": false},
	}
	r.FallbackProfile = &repocfg.CodexProfile{
		Bin: bin, Model: r.DefaultModel, Effort: r.DefaultEffort,
		FeatureOverrides: map[string]bool{"unified_exec": true},
	}
	r.ExtraEnv = []string{"STATE=" + state}

	res := runTurn(t, context.Background(), r, t.TempDir())
	if res.Err != nil || res.Attempt.Kind != runner.AttemptFallback || !res.FallbackConsumed {
		t.Fatalf("result = %+v, want package override-compatible fallback success", res)
	}
	if got := readCount(t, state); got != 2 {
		t.Fatalf("process attempts = %d, want one primary and one fallback", got)
	}
}

func TestFallbackFailureIsTerminalAndDoesNotStartAThirdProcess(t *testing.T) {
	bin, state := statefulStub(t, `exit 7`, `exit 8`)
	r := testRunner(bin)
	r.FallbackProfile = &repocfg.CodexProfile{FeatureOverrides: map[string]bool{"unified_exec": true}}
	r.ExtraEnv = []string{"STATE=" + state}
	res := runTurn(t, context.Background(), r, t.TempDir())
	if res.Err == nil || !res.FallbackConsumed || res.Attempt.Kind != runner.AttemptFallback || res.Attempt.State != runner.AttemptTerminal {
		t.Fatalf("result = %+v, want terminal fallback", res)
	}
	if !strings.Contains(res.Err.Error(), "primary") || !strings.Contains(res.Err.Error(), "fallback") {
		t.Fatalf("terminal error omitted attempt outcomes: %v", res.Err)
	}
	if got := readCount(t, state); got != 2 {
		t.Fatalf("process attempts = %d, want exactly two", got)
	}
}

func TestNonRetryableProtocolFailureDoesNotUseFallback(t *testing.T) {
	bin := writeStub(t, `printf '%s\n' '{"type":"thread.started","thread_id":"thr-protocol"}'`)
	r := testRunner(bin)
	r.FallbackProfile = &repocfg.CodexProfile{FeatureOverrides: map[string]bool{"unified_exec": true}}
	res := runTurn(t, context.Background(), r, t.TempDir())
	if res.Err == nil || res.FailureClass != runner.FailureProtocol || res.FallbackConsumed {
		t.Fatalf("result = %+v, want terminal protocol failure without fallback", res)
	}
}

func TestFallbackRemainsActiveAfterAResumedTurnFailure(t *testing.T) {
	bin, state := statefulStub(t,
		`printf '%s\n' '{"type":"thread.started","thread_id":"thr-fallback-resume"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"watchtower_decision\":{\"kind\":\"choice\",\"question\":\"Ship it?\",\"options\":[\"Ship it\",\"Hold\"],\"recommended\":0,\"why\":\"Ready.\",\"consequences\":[\"Ships.\",\"Waits.\"],\"reversible\":\"yes\"}}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`,
		`exit 7`,
		`printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`,
	)
	r := testRunner(bin)
	r.PrimaryProfile = repocfg.CodexProfile{FeatureOverrides: map[string]bool{"unified_exec": false}}
	r.FallbackProfile = &repocfg.CodexProfile{FeatureOverrides: map[string]bool{"unified_exec": true}}
	r.ExtraEnv = []string{"STATE=" + state}
	done, asks := stageRun(r, "executor", "execute")
	(<-asks).Reply <- levers.ChoiceResponse(0)
	res := <-done
	if res.Err != nil || !res.FallbackConsumed {
		t.Fatalf("result = %+v, want resumed fallback success", res)
	}
	if got := strings.Join(readCapturedArgs(t, state, 2), "\n"); !strings.Contains(got, "features.unified_exec=false") {
		t.Fatalf("primary resumed argv = %q", got)
	}
	if got := strings.Join(readCapturedArgs(t, state, 3), "\n"); !strings.Contains(got, "features.unified_exec=true") || !containsArg(readCapturedArgs(t, state, 3), "thr-fallback-resume") {
		t.Fatalf("fallback resumed argv = %q", got)
	}
}

func readCount(t *testing.T, state string) int {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(state, "count"))
	if err != nil {
		t.Fatal(err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func TestPlannerToolReconcilesAfterTurnUsage(t *testing.T) {
	bin := writeStub(t, `
printf '%s\n' '{"type":"thread.started","thread_id":"thr-planner"}'
printf '%s\n' '{"type":"item.started","item":{"type":"mcp_tool_call","server":"watchtower","tool":"local_process"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":5,"output_tokens":7}}'`)
	gate := &recordingGate{}
	r := testRunner(bin)
	workdir := t.TempDir()
	done := r.RunPlanner(context.Background(), testStageRequest(r, "GH-39", "plan", "executor", workdir), make(chan runner.Ask), gate)
	result := <-done
	if result.Err != nil || result.Tokens != 12 || !result.TokensKnown {
		t.Fatalf("result = %+v", result)
	}
	if gate.completed != 1 || gate.actual == nil || *gate.actual != 12 || gate.failed != nil {
		t.Fatalf("gate completion = completed:%d actual:%v err:%v", gate.completed, gate.actual, gate.failed)
	}
}
func TestPackageModelAndEffortOverrideDefaults(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "argv")
	bin := writeStub(t, successfulStub(`
: > "$CAPTURE"
for arg in "$@"; do printf '%s\n' "$arg" >> "$CAPTURE"; done`))
	r := testRunner(bin)
	pkg := r.Packages["executor"]
	pkg.Model = "package-model"
	pkg.Effort = "high"
	r.Packages["executor"] = pkg
	r.ExtraEnv = []string{"CAPTURE=" + capture}
	if res := runTurn(t, context.Background(), r, t.TempDir()); res.Err != nil {
		t.Fatal(res.Err)
	}
	body, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	for _, want := range []string{"package-model", `model_reasoning_effort="high"`} {
		if !strings.Contains(got, want) {
			t.Errorf("argv missing override %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"gpt-5.6-luna", `model_reasoning_effort="xhigh"`} {
		if strings.Contains(got, unwanted) {
			t.Errorf("argv retained default %q:\n%s", unwanted, got)
		}
	}
}

func TestUnknownPackageFailsWithoutStartingCodex(t *testing.T) {
	r := testRunner(filepath.Join(t.TempDir(), "missing-codex"))
	asks := make(chan runner.Ask, 1)
	workdir := t.TempDir()
	res := <-r.Run(context.Background(), testStageRequest(r, "GH-1", "execute", "missing", workdir), asks)
	if res.Err == nil || !strings.Contains(res.Err.Error(), `unknown agent package "missing"`) {
		t.Fatalf("result = %+v", res)
	}
}

func TestMissingThreadIDFails(t *testing.T) {
	bin := writeStub(t, `printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":2}}'`)
	res := runTurn(t, context.Background(), testRunner(bin), t.TempDir())
	if res.Err == nil || !strings.Contains(res.Err.Error(), "thread") {
		t.Fatalf("result = %+v", res)
	}
}

func TestMissingTurnCompletedFails(t *testing.T) {
	bin := writeStub(t, `printf '%s\n' '{"type":"thread.started","thread_id":"thr-incomplete"}'`)
	res := runTurn(t, context.Background(), testRunner(bin), t.TempDir())
	if res.Err == nil || !strings.Contains(res.Err.Error(), "without completion") {
		t.Fatalf("result = %+v", res)
	}
}

func TestFailedTurnFails(t *testing.T) {
	bin := writeStub(t, `
printf '%s\n' '{"type":"thread.started","thread_id":"thr-failed"}'
printf '%s\n' '{"type":"turn.failed","error":{"message":"model unavailable"}}'`)
	res := runTurn(t, context.Background(), testRunner(bin), t.TempDir())
	if res.Err == nil || !strings.Contains(res.Err.Error(), "model unavailable") {
		t.Fatalf("result = %+v", res)
	}
}

func TestNonzeroExitIncludesOnlyBoundedStderrTail(t *testing.T) {
	bin := writeStub(t, `
i=0
while [ "$i" -lt 17000 ]; do printf x >&2; i=$((i + 1)); done
printf 'TAIL-SUFFIX\n' >&2
exit 7`)
	r := testRunner(bin)
	r.ExtraEnv = []string{"SENTINEL_SECRET=do-not-leak"}
	res := runTurn(t, context.Background(), r, t.TempDir())
	if res.Err == nil {
		t.Fatal("nonzero exit succeeded")
	}
	got := res.Err.Error()
	if !strings.Contains(got, "TAIL-SUFFIX") || len(got) > stderrTailBytes+256 {
		t.Fatalf("error is not a bounded stderr suffix: len=%d suffix=%v", len(got), strings.Contains(got, "TAIL-SUFFIX"))
	}
	for _, secret := range []string{"do-not-leak", "Implement and verify.", agentprotocol.TaskMessage("execute", "GH-1")} {
		if strings.Contains(got, secret) {
			t.Fatalf("error leaked %q: %q", secret, got)
		}
	}
}

func TestCancellationStopsCodex(t *testing.T) {
	bin := writeStub(t, `sleep 10`)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := runTurn(t, ctx, testRunner(bin), t.TempDir())
	if res.Err == nil || !strings.Contains(res.Err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("result = %+v", res)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancellation did not stop Codex promptly")
	}
}

func TestOversizedJSONLLineFailsCleanly(t *testing.T) {
	bin := writeStub(t, `
printf '{"type":"item.completed","item":{"type":"agent_message","text":"'
head -c 1048600 /dev/zero | tr '\000' x
printf '"}}\n'`)
	res := runTurn(t, context.Background(), testRunner(bin), t.TempDir())
	if res.Err == nil || !strings.Contains(res.Err.Error(), "JSONL") {
		t.Fatalf("result = %+v", res)
	}
}

func statefulStub(t *testing.T, turns ...string) (string, string) {
	t.Helper()
	state := t.TempDir()
	var body strings.Builder
	body.WriteString(`
count=0
if [ -f "$STATE/count" ]; then count=$(cat "$STATE/count"); fi
count=$((count + 1))
printf '%s' "$count" > "$STATE/count"
: > "$STATE/args-$count"
for arg in "$@"; do printf '%s\n' "$arg" >> "$STATE/args-$count"; done
case "$count" in
`)
	for index, turn := range turns {
		body.WriteString(strconv.Itoa(index + 1))
		body.WriteString(")\n")
		body.WriteString(turn)
		body.WriteString("\n;;\n")
	}
	body.WriteString(`*) exit 91 ;; esac`)
	return writeStub(t, body.String()), state
}

func stageRun(r *CodeRunner, pkg, stage string) (<-chan runner.Result, chan runner.Ask) {
	asks := make(chan runner.Ask, 1)
	return r.Run(context.Background(), testStageRequest(r, "GH-1", stage, pkg, os.TempDir()), asks), asks
}

func readCapturedArgs(t *testing.T, state string, invocation int) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(state, "args-"+strconv.Itoa(invocation)))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
}

func TestDecisionResumesCapturedThreadAndAccumulatesTokens(t *testing.T) {
	bin, state := statefulStub(t,
		`printf '%s\n' '{"type":"thread.started","thread_id":"thr-choice"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"watchtower_decision\":{\"kind\":\"choice\",\"question\":\"Ship it?\",\"options\":[\"Ship it\",\"Hold\"],\"recommended\":0,\"importance\":0.8,\"why\":\"Ready.\",\"consequences\":[\"Ships.\",\"Waits.\"],\"reversible\":\"yes\"}}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":20,"output_tokens":10}}'`,
		`printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"continued"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":8,"output_tokens":4}}'`,
	)
	r := testRunner(bin)
	r.ExtraEnv = []string{"STATE=" + state}
	done, asks := stageRun(r, "executor", "execute")
	ask := <-asks
	if ask.Decision.Question != "Ship it?" {
		t.Fatalf("ask = %+v", ask.Decision)
	}
	ask.Reply <- levers.ChoiceResponse(0)
	res := <-done
	if res.Err != nil || res.SessionID != "thr-choice" || res.Tokens != 42 {
		t.Fatalf("result = %+v", res)
	}
	args := readCapturedArgs(t, state, 2)
	joined := strings.Join(args, "\n")
	for _, want := range []string{"exec", "resume", "thr-choice", "Human decision: Ship it"} {
		if !strings.Contains(joined, want) {
			t.Errorf("resume argv missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "--last") {
		t.Fatalf("resume used --last:\n%s", joined)
	}
}

func TestFreeformDecisionResumesWithHumanText(t *testing.T) {
	bin, state := statefulStub(t,
		`printf '%s\n' '{"type":"thread.started","thread_id":"thr-freeform"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"watchtower_decision\":{\"kind\":\"freeform\",\"question\":\"What limit?\",\"recommended_response\":\"Three.\",\"why\":\"Bounded.\",\"consequences\":[\"Retries change.\"],\"reversible\":\"yes\"}}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":2,"output_tokens":3}}'`,
		`printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":5,"output_tokens":7}}'`,
	)
	r := testRunner(bin)
	r.ExtraEnv = []string{"STATE=" + state}
	done, asks := stageRun(r, "executor", "execute")
	ask := <-asks
	ask.Reply <- levers.FreeformResponse("Use four retries.")
	res := <-done
	if res.Err != nil || res.Tokens != 17 {
		t.Fatalf("result = %+v", res)
	}
	if got := strings.Join(readCapturedArgs(t, state, 2), "\n"); !strings.Contains(got, "Human decision: Use four retries.") {
		t.Fatalf("resume argv = %q", got)
	}
}

func TestChoiceDecisionResumesWithHumanText(t *testing.T) {
	bin, state := statefulStub(t,
		`printf '%s\n' '{"type":"thread.started","thread_id":"thr-choice-note"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"watchtower_decision\":{\"kind\":\"choice\",\"question\":\"What retry limit?\",\"options\":[\"Three\",\"Four\"],\"recommended\":0,\"allow_freeform\":false,\"importance\":0.8,\"why\":\"Bounded retries.\",\"consequences\":[\"Three retries.\",\"Four retries.\"],\"reversible\":\"yes\"}}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":2,"output_tokens":3}}'`,
		`printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":5,"output_tokens":7}}'`,
	)
	r := testRunner(bin)
	r.ExtraEnv = []string{"STATE=" + state}
	done, asks := stageRun(r, "executor", "execute")
	ask := <-asks
	if ask.Decision.Kind != levers.DecisionChoice || ask.Decision.AllowFreeform {
		t.Fatalf("ask = %+v", ask.Decision)
	}
	ask.Reply <- levers.FreeformResponse("Use four retries.")
	res := <-done
	if res.Err != nil || res.Tokens != 17 {
		t.Fatalf("result = %+v", res)
	}
	if got := strings.Join(readCapturedArgs(t, state, 2), "\n"); !strings.Contains(got, "Human decision: Use four retries.") {
		t.Fatalf("resume argv = %q", got)
	}
}

func TestCoachRepairsDecisionBeforeAsking(t *testing.T) {
	bin, state := statefulStub(t,
		`printf '%s\n' '{"type":"thread.started","thread_id":"thr-coach"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"watchtower_decision\":{\"question\":\"Proceed?\",\"options\":[\"Yes\",\"No\"],\"recommended\":0}}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`,
		`printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"watchtower_decision\":{\"question\":\"Proceed?\",\"options\":[\"Yes\",\"No\"],\"recommended\":0,\"why\":\"Safe.\",\"consequences\":[\"Runs.\",\"Stops.\"],\"briefing\":{\"proof\":[{\"claim\":\"Tests pass.\"}]}}}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`,
		`printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"watchtower_decision\":{\"question\":\"Proceed?\",\"options\":[\"Yes\",\"No\"],\"recommended\":0,\"why\":\"Safe.\",\"consequences\":[\"Runs.\",\"Stops.\"],\"reversible\":\"yes\",\"briefing\":{\"proof\":[{\"claim\":\"Tests pass.\",\"cite\":\"go test ./internal/decisionpage\"}]}}}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`,
		`printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`,
	)
	r := testRunner(bin)
	r.ExtraEnv = []string{"STATE=" + state}
	done, asks := stageRun(r, "executor", "execute")
	ask := <-asks
	if ask.Decision.Why != "Safe." || ask.Decision.Briefing == nil ||
		len(ask.Decision.Briefing.Proof) != 1 || ask.Decision.Briefing.Proof[0].Cite != "go test ./internal/decisionpage" {
		t.Fatalf("ask was not repaired: %+v", ask.Decision)
	}
	ask.Reply <- levers.ChoiceResponse(0)
	if res := <-done; res.Err != nil || res.Tokens != 8 {
		t.Fatalf("result = %+v", res)
	}
	coachingCapture, err := os.ReadFile(filepath.Join(state, "args-2"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(coachingCapture), agentprotocol.CoachMessage) {
		t.Fatalf("coaching argv does not contain the shared prompt: %q", coachingCapture)
	}
	if got := readCapturedArgs(t, state, 4); got[len(got)-1] != "Human decision: Yes" {
		t.Fatalf("human prompt = %q", got[len(got)-1])
	}
}

func TestCoachConvertsUnstructuredDecisionRequestBeforeEndingStage(t *testing.T) {
	bin, state := statefulStub(t,
		`printf '%s\n' '{"type":"thread.started","thread_id":"thr-unstructured"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"I found an architecture mismatch. Reply with Human decision: choose one, and I will continue."}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`,
		`printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"watchtower_decision\":{\"kind\":\"choice\",\"question\":\"Which architecture?\",\"options\":[\"Keep the current model\",\"Expand the model\"],\"recommended\":0,\"why\":\"It preserves scope.\",\"consequences\":[\"The plan stays narrow.\",\"The plan expands state.\"],\"reversible\":\"Before implementation.\"}}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`,
		`printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`,
	)
	r := testRunner(bin)
	r.ExtraEnv = []string{"STATE=" + state}
	done, asks := stageRun(r, "executor", "execute")
	select {
	case ask := <-asks:
		if ask.Decision.Question != "Which architecture?" {
			t.Fatalf("ask = %+v", ask.Decision)
		}
		ask.Reply <- levers.ChoiceResponse(0)
	case res := <-done:
		t.Fatalf("runner ended instead of coaching the decision request: %+v", res)
	case <-time.After(2 * time.Second):
		t.Fatal("runner neither coached nor completed")
	}
	if res := <-done; res.Err != nil {
		t.Fatalf("result = %+v", res)
	}
	coaching := strings.Join(readCapturedArgs(t, state, 2), "\n")
	if !strings.Contains(coaching, "watchtower_decision") || !strings.Contains(coaching, "Nothing else") {
		t.Fatalf("unstructured-decision coaching prompt = %q", coaching)
	}
}

func TestCoachStopsAfterTwoIncompleteRetries(t *testing.T) {
	incomplete := `printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"watchtower_decision\":{\"question\":\"Proceed?\",\"options\":[\"Yes\",\"No\"],\"recommended\":0}}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`
	bin, state := statefulStub(t,
		`printf '%s\n' '{"type":"thread.started","thread_id":"thr-incomplete"}'`+"\n"+incomplete,
		incomplete,
		incomplete,
	)
	r := testRunner(bin)
	r.ExtraEnv = []string{"STATE=" + state}
	done, _ := stageRun(r, "executor", "execute")
	res := <-done
	if res.Err == nil || !strings.Contains(res.Err.Error(), "remained incomplete after 2 coaching attempts") {
		t.Fatalf("result = %+v", res)
	}
}

func TestProposalCallbacksReceiveSingleAndBatchMarkers(t *testing.T) {
	bin := writeStub(t, `
printf '%s\n' '{"type":"thread.started","thread_id":"thr-proposals"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"watchtower_proposal\":{\"title\":\"Single\",\"body\":\"One\"}}\n{\"watchtower_proposal_batch\":{\"tasks\":[{\"key\":\"a\",\"title\":\"First\",\"body\":\"A\"},{\"key\":\"b\",\"title\":\"Second\",\"body\":\"B\",\"depends_on\":[\"a\"]}]}}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`)
	r := testRunner(bin)
	var single runner.Proposal
	var batch []runner.Proposal
	r.OnProposal = func(_ string, proposal runner.Proposal) { single = proposal }
	r.OnProposalBatch = func(_ string, proposals []runner.Proposal) { batch = proposals }
	res := runTurn(t, context.Background(), r, t.TempDir())
	if res.Err != nil || single.Title != "Single" || len(batch) != 2 || batch[1].DependsOn[0] != "a" {
		t.Fatalf("result=%+v single=%+v batch=%+v", res, single, batch)
	}
}

func TestDependencyRequiresAcceptedDecision(t *testing.T) {
	bin := writeStub(t, `
printf '%s\n' '{"type":"thread.started","thread_id":"thr-dependency"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"watchtower_dependency\":{\"depends_on\":[\"GH-2\"]}}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`)
	res := runTurn(t, context.Background(), testRunner(bin), t.TempDir())
	if res.Err == nil || !strings.Contains(res.Err.Error(), "without an accepted decision") {
		t.Fatalf("result = %+v", res)
	}
}

func TestDependencyAfterAcceptedDecisionIsReturned(t *testing.T) {
	bin, state := statefulStub(t,
		`printf '%s\n' '{"type":"thread.started","thread_id":"thr-dependency"}'
	printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"watchtower_decision\":{\"question\":\"Proceed?\",\"options\":[\"Yes\",\"No\"],\"recommended\":0,\"why\":\"Safe.\",\"consequences\":[\"Runs.\",\"Stops.\"],\"reversible\":\"yes\"}}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`,
		`printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"watchtower_dependency\":{\"depends_on\":[\" GH-2 \",\"GH-3\",\"GH-2\"]}}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`,
	)
	r := testRunner(bin)
	r.ExtraEnv = []string{"STATE=" + state}
	done, asks := stageRun(r, "executor", "execute")
	(<-asks).Reply <- levers.ChoiceResponse(0)
	res := <-done
	if res.Err != nil || len(res.DependsOn) != 2 || res.DependsOn[0] != "GH-2" || res.DependsOn[1] != "GH-3" {
		t.Fatalf("result = %+v", res)
	}
}

func TestConcurrentDecisionThreadsNeverCross(t *testing.T) {
	bin := writeStub(t, `
count=0
if [ -f "$STATE/count" ]; then count=$(cat "$STATE/count"); fi
count=$((count + 1)); printf '%s' "$count" > "$STATE/count"
: > "$STATE/args-$count"
for arg in "$@"; do printf '%s\n' "$arg" >> "$STATE/args-$count"; done
if [ "$count" -eq 1 ]; then
  printf '%s\n' "{\"type\":\"thread.started\",\"thread_id\":\"thr-$IDENT\"}"
  printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"watchtower_decision\":{\"question\":\"Proceed?\",\"options\":[\"Yes\",\"No\"],\"recommended\":0,\"why\":\"Safe.\",\"consequences\":[\"Runs.\",\"Stops.\"],\"reversible\":\"yes\"}}"}}'
fi
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`)
	type invocation struct {
		id    string
		state string
	}
	invocations := []invocation{{id: "alpha", state: t.TempDir()}, {id: "beta", state: t.TempDir()}}
	var wg sync.WaitGroup
	for _, invocation := range invocations {
		invocation := invocation
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := testRunner(bin)
			r.ExtraEnv = []string{"STATE=" + invocation.state, "IDENT=" + invocation.id}
			done, asks := stageRun(r, "executor", "execute")
			(<-asks).Reply <- levers.ChoiceResponse(0)
			if res := <-done; res.Err != nil {
				t.Errorf("%s result = %+v", invocation.id, res)
			}
		}()
	}
	wg.Wait()
	for _, invocation := range invocations {
		got := strings.Join(readCapturedArgs(t, invocation.state, 2), "\n")
		if !strings.Contains(got, "thr-"+invocation.id) {
			t.Errorf("%s resume missing own thread: %s", invocation.id, got)
		}
		other := "alpha"
		if invocation.id == "alpha" {
			other = "beta"
		}
		if strings.Contains(got, "thr-"+other) {
			t.Errorf("%s resume contains %s thread: %s", invocation.id, other, got)
		}
	}
}
