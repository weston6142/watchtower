package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/agentprotocol"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/pkgs"
	"github.com/weston6142/watchtower/internal/runner"
)

func testPkgs() map[string]pkgs.Package {
	return map[string]pkgs.Package{
		"spec-writer": {Name: "spec-writer", Prompt: "write specs"},
	}
}

func run(t *testing.T, bin string, dir string) (<-chan runner.Result, chan runner.Ask) {
	t.Helper()
	c := &CodeRunner{Bin: bin, Packages: testPkgs()}
	asks := make(chan runner.Ask, 1)
	return c.Run(context.Background(), "GH-1", "spec", "spec-writer", dir, asks), asks
}

// runLines is run() with OnLine wired, for tests that assert on transcript
// output rather than on the result.
func runLines(t *testing.T, bin, dir string) ([]string, runner.Result) {
	t.Helper()
	var lines []string
	c := &CodeRunner{Bin: bin, Packages: testPkgs(), OnLine: func(_, _, line string) {
		lines = append(lines, line)
	}}
	asks := make(chan runner.Ask, 1)
	res := <-c.Run(context.Background(), "GH-1", "spec", "spec-writer", dir, asks)
	return lines, res
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
	if !strings.Contains(joined, "↳ Bash go test ./...") {
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
	done, _ := run(t, abs(t, "testdata/coaching-exhausted.sh"), t.TempDir())
	res := <-done
	if res.Err == nil || !strings.Contains(res.Err.Error(), "claude decision remained incomplete after 2 coaching attempts") {
		t.Fatalf("result: %+v", res)
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
