package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/agentprotocol"
	"github.com/weston6142/watchtower/internal/pkgs"
	"github.com/weston6142/watchtower/internal/runner"
)

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

func runTurn(t *testing.T, ctx context.Context, r *CodeRunner, workdir string) runner.Result {
	t.Helper()
	asks := make(chan runner.Ask, 1)
	return <-r.Run(ctx, "GH-1", "execute", "executor", workdir, asks)
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
		`sandbox_mode="danger-full-access"`,
		`approval_policy="never"`,
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
	res := <-r.Run(context.Background(), "GH-1", "execute", "missing", t.TempDir(), asks)
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
