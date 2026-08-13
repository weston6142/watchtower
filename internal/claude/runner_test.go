package claude

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/agentprotocol"
	"github.com/weston6142/watchtower/internal/capability"
	capruntime "github.com/weston6142/watchtower/internal/capability/runtime"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/pkgs"
	"github.com/weston6142/watchtower/internal/plannerartifact"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/store"
)

func testPkgs() map[string]pkgs.Package {
	return map[string]pkgs.Package{
		"spec-writer": {Name: "spec-writer", Prompt: "write specs"},
		"executor":    {Name: "executor", Prompt: "execute plan"},
	}
}

func testCodeRunner(bin string) *CodeRunner {
	return &CodeRunner{Bin: bin, Packages: testPkgs(), Backend: passthroughTestBackend{}}
}

// passthroughTestBackend keeps stream, continuation, and invocation fixtures
// focused on their observable adapter behavior. Provider-boundary tests use
// NewPlatformBackend directly and therefore still exercise real containment.
type passthroughTestBackend struct{}

func (passthroughTestBackend) Preflight(contract capability.CompiledContract) (capability.EnforcementPlan, error) {
	controls := runner.RequiredControls(contract)
	proofs := make([]capability.ControlProof, 0, len(controls))
	for _, control := range controls {
		proofs = append(proofs, capability.ControlProof{Control: control, Proven: true})
	}
	return runner.NewEnforcementPlan(contract, "test-runtime", "passthrough", "1", proofs)
}

func (passthroughTestBackend) Wrap(request capruntime.ProcessRequest) (capruntime.ProcessRequest, error) {
	return request, nil
}

func claudeStageRequest(r *CodeRunner, issueID, stage, agent, workdir string) runner.StageRequest {
	contract := sealClaudeTestContract(capability.Contract{
		Version: capability.ContractVersion, EnginePolicyVersion: capability.EnginePolicyVersion,
		IssueID: issueID, Stage: stage, AttemptID: "attempt-test", Profile: "implementation",
		WorkspaceRoot: workdir,
		Operations: []capability.OperationClass{
			capability.OpWorkspaceRead, capability.OpWorkspaceMutate, capability.OpLocalProcess,
			capability.OpVCSRead, capability.OpVCSCommit, capability.OpPlannerArtifactApply,
		},
	})
	plan, err := r.Preflight(context.Background(), runner.PreflightRequest{
		IssueID: issueID, Stage: stage, Agent: agent, Workdir: workdir, Contract: contract,
	})
	if err != nil {
		panic(err)
	}
	return runner.StageRequest{IssueID: issueID, Stage: stage, Agent: agent, Workdir: workdir, Contract: contract, Plan: plan}
}

func sealClaudeTestContract(contract capability.Contract) capability.CompiledContract {
	body, _ := json.Marshal(contract)
	authority := contract
	authority.AttemptID = ""
	authorityBody, _ := json.Marshal(authority)
	contractSum, authoritySum := sha256.Sum256(body), sha256.Sum256(authorityBody)
	return capability.CompiledContract{
		Contract: contract, ContractID: hex.EncodeToString(contractSum[:]), AuthorityDigest: hex.EncodeToString(authoritySum[:]),
	}
}

func TestCodeRunnerReturnsStageResultEvidence(t *testing.T) {
	bin := writeClaudeStageResultStub(t, claudeStageResultMarker("implemented"))
	r := testCodeRunner(bin)
	res := <-r.Run(context.Background(), claudeStageRequest(r, "GH-67", "execute", "executor", t.TempDir()), make(chan runner.Ask))
	if res.Err != nil || res.StageEvidence == nil {
		t.Fatalf("result = %+v", res)
	}
	if got := res.StageEvidence.Execute.PlanTasks[0].Summary; got != "implemented" {
		t.Fatalf("task summary = %q", got)
	}
}

func TestCodeRunnerRejectsConflictingStageResultMarkers(t *testing.T) {
	bin := writeClaudeStageResultStub(t, claudeStageResultMarker("first"), claudeStageResultMarker("different"))
	r := testCodeRunner(bin)
	res := <-r.Run(context.Background(), claudeStageRequest(r, "GH-67", "execute", "executor", t.TempDir()), make(chan runner.Ask))
	if res.Err == nil || res.FailureClass != runner.FailureProtocol || res.StageEvidence != nil {
		t.Fatalf("result = %+v", res)
	}
}

func TestCodeRunnerRejectsStageResultBeforeFinalTurn(t *testing.T) {
	decision := `{"watchtower_decision":{"kind":"choice","question":"Apply the repair?","options":["Apply","Hold"],"recommended":0,"why":"The repair closes the gap.","consequences":["The repair is applied.","The stage remains incomplete."],"reversible":"Before the repair is committed."}}`
	bin := writeClaudeDecisionStageResultStub(t, claudeStageResultMarker("before decision")+"\n"+decision)
	r := testCodeRunner(bin)
	asks := make(chan runner.Ask, 1)
	done := r.Run(context.Background(), claudeStageRequest(r, "GH-67", "execute", "executor", t.TempDir()), asks)
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

func writeClaudeStageResultStub(t *testing.T, markers ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-result-stub")
	lines := []string{"#!/bin/sh", "set -eu", "cat > /dev/null &", claudeJSONLine(map[string]any{"type": "system", "subtype": "init", "session_id": "s-result"})}
	for _, marker := range markers {
		lines = append(lines, claudeJSONLine(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": marker}}}}))
	}
	lines = append(lines, claudeJSONLine(map[string]any{"type": "result", "is_error": false, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}))
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeClaudeDecisionStageResultStub(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-decision-result-stub")
	lines := []string{
		"#!/bin/sh",
		"set -eu",
		"read _task",
		claudeJSONLine(map[string]any{"type": "system", "subtype": "init", "session_id": "s-premature-result"}),
		claudeJSONLine(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}}),
		claudeJSONLine(map[string]any{"type": "result", "is_error": false, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}),
		"read _reply",
		claudeJSONLine(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "decision applied"}}}}),
		claudeJSONLine(map[string]any{"type": "result", "is_error": false, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func claudeStageResultMarker(summary string) string {
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

func claudeJSONLine(value any) string {
	body, _ := json.Marshal(value)
	return "printf '%s\\n' '" + string(body) + "'"
}

func run(t *testing.T, bin string, dir string) (<-chan runner.Result, chan runner.Ask) {
	t.Helper()
	c := testCodeRunner(bin)
	c.ExtraEnv = []string{"TEST_WORKDIR=" + dir}
	asks := make(chan runner.Ask, 1)
	return c.Run(context.Background(), claudeStageRequest(c, "GH-1", "spec", "spec-writer", dir), asks), asks
}

// runLines is run() with OnLine wired, for tests that assert on transcript
// output rather than on the result.
func runLines(t *testing.T, bin, dir string) ([]string, runner.Result) {
	t.Helper()
	var lines []string
	c := testCodeRunner(bin)
	c.ExtraEnv = []string{"TEST_WORKDIR=" + dir}
	c.OnLine = func(_, _, line string) {
		lines = append(lines, line)
	}
	asks := make(chan runner.Ask, 1)
	res := <-c.Run(context.Background(), claudeStageRequest(c, "GH-1", "spec", "spec-writer", dir), asks)
	return lines, res
}

type recordingGate struct {
	completed int
	actual    *int64
	failed    error
}

func (g *recordingGate) Admit(context.Context, runner.ToolCall) (runner.ToolDecision, error) {
	return runner.ToolDecision{Allowed: true, LeaseID: "lease-1"}, nil
}

func (g *recordingGate) Complete(
	_ context.Context, _ runner.ToolDecision, actual *int64, operationErr error,
) error {
	g.completed++
	if actual != nil {
		value := *actual
		g.actual = &value
	}
	g.failed = operationErr
	return nil
}

// The operator watching a tool-heavy stage needs to see the tools. A tool-only
// message used to fall through as empty assistant text, writing a blank line —
// so the door filled with nothing while the agent worked.
func TestOnLineReceivesToolCalls(t *testing.T) {
	lines, res := runLines(t, abs(t, "testdata/tools.sh"), t.TempDir())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "looking at the engine") {
		t.Fatalf("prose missing: %q", joined)
	}
	if !strings.Contains(joined, "↳ mcp__watchtower__local_process go test ./...") {
		t.Fatalf("tool line missing: %q", joined)
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			t.Fatalf("blank line written to transcript: %q", joined)
		}
	}
}

func TestHappyPathProducesArtifactAndTokens(t *testing.T) {
	dir := t.TempDir()
	done, _ := run(t, abs(t, "testdata/happy.sh"), dir)
	res := <-done
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if res.SessionID != "s-happy" || res.Tokens != 300 {
		t.Fatalf("res: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "spec.md")); err != nil {
		t.Fatal("stub should have written spec.md in workdir")
	}
}

func TestPlannerSessionEnvironmentReachesClaudeChildWithoutArgLeak(t *testing.T) {
	dir := t.TempDir()
	envCapture := filepath.Join(dir, "planner-env")
	argvCapture := filepath.Join(dir, "planner-argv")
	sentinel := "private-planner-session-value"
	bin := filepath.Join(dir, "claude-stub")
	script := `#!/bin/sh
set -eu
	printf '%s' "${WATCHTOWER_PLANNER_SESSION-}" > "$CAPTURE_ENV"
: > "$CAPTURE_ARGV"
for arg in "$@"; do printf '%s\n' "$arg" >> "$CAPTURE_ARGV"; done
cat > /dev/null &
printf '%s\n' '{"type":"system","subtype":"init","session_id":"s-planner"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"planner"}]}}'
printf '%s\n' '{"type":"result","is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := testCodeRunner(bin)
	r.ExtraEnv = []string{"CAPTURE_ENV=" + envCapture, "CAPTURE_ARGV=" + argvCapture}
	t.Setenv("WATCHTOWER_PLANNER_SESSION", sentinel)
	ctx := context.Background()
	res := <-r.RunPlanner(ctx, claudeStageRequest(r, "GH-1", "plan", "spec-writer", dir), make(chan runner.Ask), nil)
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
	dir := t.TempDir()
	capture := filepath.Join(dir, "descriptor")
	bin := filepath.Join(dir, "claude-stub")
	script := `#!/bin/sh
set -eu
if (printf x >&3) 2>/dev/null; then
  printf 'attached' > "$CAPTURE"
else
  printf 'absent' > "$CAPTURE"
fi
printf '%s\n' '{"type":"system","subtype":"init","session_id":"s-planner"}'
printf '%s\n' '{"type":"result","is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	coordinator, err := store.Open(filepath.Join(t.TempDir(), "coordinator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	authority, err := plannerartifact.CreateOrLoad(coordinator, plannerartifact.Binding{IssueID: "GH-72", Stage: "plan", Attempt: 1, Worktree: dir})
	if err != nil {
		t.Fatal(err)
	}
	r := testCodeRunner(bin)
	r.ExtraEnv = []string{"CAPTURE=" + capture}
	result := <-r.RunPlanner(runner.WithPlannerArtifactAuthority(context.Background(), authority), claudeStageRequest(r, "GH-72", "plan", "spec-writer", dir), make(chan runner.Ask), nil)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if got, err := os.ReadFile(capture); err != nil {
		t.Fatal(err)
	} else if string(got) != "absent" {
		t.Fatalf("planner descriptor reached Claude child: %q", got)
	}
}

func TestManagedEnvironmentOverlayReachesClaudeChild(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "environment")
	bin := filepath.Join(dir, "claude-stub")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$GOCACHE" "$GOMODCACHE" "$GOPATH" "$SENTINEL" > "$CAPTURE"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"s-managed"}'
printf '%s\n' '{"type":"result","is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOCACHE", "inherited-cache")
	t.Setenv("GOMODCACHE", "inherited-mod")
	t.Setenv("GOPATH", "inherited-path")
	r := testCodeRunner(bin)
	r.ExtraEnv = []string{"GOCACHE=extra-cache", "GOMODCACHE=extra-mod", "GOPATH=extra-path", "SENTINEL=keep", "CAPTURE=" + capture}
	ctx := runner.WithManagedEnvironment(context.Background(), []string{
		"GOCACHE=lease-cache", "GOMODCACHE=lease-mod", "GOPATH=lease-path",
	})
	res := <-r.Run(ctx, claudeStageRequest(r, "GH-48", "execute", "spec-writer", dir), make(chan runner.Ask))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if got, err := os.ReadFile(capture); err != nil {
		t.Fatal(err)
	} else if want := "lease-cache\nlease-mod\nlease-path\nkeep\n"; string(got) != want {
		t.Fatalf("Claude child environment = %q, want %q", got, want)
	}
}

func TestDecisionRoundTrip(t *testing.T) {
	done, asks := run(t, abs(t, "testdata/asker.sh"), t.TempDir())
	a := <-asks
	if a.Decision.Question != "Pick one" || a.Decision.Recommended != 1 {
		t.Fatalf("ask: %+v", a.Decision)
	}
	a.Reply <- levers.ChoiceResponse(1) // choose "b"
	res := <-done
	if res.Err != nil || res.SessionID != "s-ask" {
		t.Fatalf("res: %+v", res)
	}
}

func TestDependencyMarkerRequiresAcceptedDecision(t *testing.T) {
	done, _ := run(t, abs(t, "testdata/dependency-without-decision.sh"), t.TempDir())
	res := <-done
	if res.Err == nil || !strings.Contains(res.Err.Error(), "without an accepted decision") {
		t.Fatalf("result: %+v", res)
	}
}

func TestDependencyMarkerReturnsIDsAfterAcceptedDecision(t *testing.T) {
	done, asks := run(t, abs(t, "testdata/dependency-after-decision.sh"), t.TempDir())
	ask := <-asks
	ask.Reply <- levers.ChoiceResponse(0)
	res := <-done
	if res.Err != nil || len(res.DependsOn) != 2 ||
		res.DependsOn[0] != "GH-2" || res.DependsOn[1] != "GH-3" {
		t.Fatalf("result: %+v", res)
	}
}

func TestRunnerCoachesIncompleteDecision(t *testing.T) {
	done, asks := run(t, abs(t, "testdata/coached.sh"), t.TempDir())
	a := <-asks // must be the COACHED (v2) decision, not the v1 one
	if a.Decision.Why == "" || len(a.Decision.Consequences) != 2 || a.Decision.Briefing == nil ||
		len(a.Decision.Briefing.Proof) != 1 || a.Decision.Briefing.Proof[0].Cite != "go test ./internal/decisionpage" {
		t.Fatalf("ask not coached to v2: %+v", a.Decision)
	}
	a.Reply <- levers.ChoiceResponse(0)
	if res := <-done; res.Err != nil {
		t.Fatal(res.Err)
	}
}

func TestRunnerStopsAfterTwoIncompleteRetries(t *testing.T) {
	gate := &recordingGate{}
	c := testCodeRunner(abs(t, "testdata/coaching-exhausted.sh"))
	res := <-c.RunPlanner(
		context.Background(), claudeStageRequest(c, "GH-1", "spec", "spec-writer", t.TempDir()), make(chan runner.Ask, 1), gate,
	)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "claude decision remained incomplete after 2 coaching attempts") {
		t.Fatalf("result: %+v", res)
	}
	if res.Tokens != 6 || !res.TokensKnown {
		t.Fatalf("tokens: %d known:%v, want 6 known", res.Tokens, res.TokensKnown)
	}
	if gate.completed != 1 || gate.actual == nil || *gate.actual != 2 || gate.failed == nil ||
		!strings.Contains(gate.failed.Error(), "claude decision remained incomplete after 2 coaching attempts") {
		t.Fatalf("gate completion = completed:%d actual:%v err:%v", gate.completed, gate.actual, gate.failed)
	}
}

// An agent may emit a low-importance decision and keep working in the same
// turn. The real CLI absorbs a mid-turn user message into the running turn
// (steering) — it never starts a new turn for it — so replying immediately
// leaves the runner waiting forever for a turn that will never come.
func TestMidTurnDecisionReplyDoesNotDeadlock(t *testing.T) {
	done, asks := run(t, abs(t, "testdata/midturn.sh"), t.TempDir())
	a := <-asks
	a.Reply <- levers.ChoiceResponse(0)
	select {
	case res := <-done:
		if res.Err != nil {
			t.Fatal(res.Err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runner deadlocked: reply sent mid-turn swallowed the final result")
	}
}

func TestDecisionRoundTripReturnsFreeformText(t *testing.T) {
	dir := t.TempDir()
	done, asks := run(t, abs(t, "testdata/freeform.sh"), dir)
	a := <-asks
	if a.Decision.Kind != levers.DecisionFreeform {
		t.Fatalf("ask: %+v", a.Decision)
	}
	a.Reply <- levers.FreeformResponse("Change the retry limit to three.")
	if res := <-done; res.Err != nil {
		t.Fatal(res.Err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "decision-reply.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "Human decision: Change the retry limit to three.") {
		t.Fatalf("reply = %q", got)
	}
}

func TestChoiceDecisionRoundTripReturnsFreeformText(t *testing.T) {
	dir := t.TempDir()
	done, asks := run(t, abs(t, "testdata/choice-note.sh"), dir)
	a := <-asks
	if a.Decision.Kind != levers.DecisionChoice || a.Decision.AllowFreeform {
		t.Fatalf("ask = %+v", a.Decision)
	}
	a.Reply <- levers.FreeformResponse("Use four retries.")
	if res := <-done; res.Err != nil {
		t.Fatal(res.Err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "decision-reply.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "Human decision: Use four retries.") {
		t.Fatalf("reply = %q", got)
	}
}

func TestErrorResultFails(t *testing.T) {
	done, _ := run(t, abs(t, "testdata/failer.sh"), t.TempDir())
	if res := <-done; res.Err == nil {
		t.Fatal("expected error result")
	}
}

func abs(t *testing.T, rel string) string {
	t.Helper()
	p, err := filepath.Abs(rel)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestThinkingTokens(t *testing.T) {
	for in, want := range map[string]string{
		"low": "1024", "medium": "8192", "high": "32768", "": "", "wat": "",
	} {
		if got := ThinkingTokens(in); got != want {
			t.Errorf("ThinkingTokens(%q) = %q, want %q", in, got, want)
		}
	}
}

// The setup inspector shows the first user message verbatim. If TaskMessage and
// the runner's stdin ever drift, the panel lies about the prompt — which is the
// entire feature. This test wires them together so they cannot.
func TestTaskMessageMatchesRunnerInput(t *testing.T) {
	dir := t.TempDir()
	done, _ := run(t, abs(t, "testdata/stdin.sh"), dir)
	if res := <-done; res.Err != nil {
		t.Fatal(res.Err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "stdin.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// run() drives stage "spec", issue "GH-1", package "spec-writer".
	want := string(UserMessage(agentprotocol.TaskMessage("spec", "GH-1")))
	if string(got) != want {
		t.Fatalf("runner wrote %q, TaskMessage yields %q", got, want)
	}
}
