package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/plannerartifact"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/store"
)

func TestRealCodexPlannerArtifactBoundary(t *testing.T) {
	root := moduleRoot(t)
	helper := buildWatchtowerHelper(t, root)
	for _, tc := range []struct {
		name       string
		bindOther  bool
		noDesc     bool
		wantErr    bool
		wantOutput bool
	}{
		{name: "authorized", wantOutput: true},
		{name: "mismatched-binding", bindOther: true, wantErr: true},
		{name: "missing-descriptor", noDesc: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := t.TempDir()
			if err := plannerartifact.Prepare(current); err != nil {
				t.Fatal(err)
			}
			coordinator, err := store.Open(filepath.Join(t.TempDir(), "coordinator.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer coordinator.Close()
			boundWorktree := current
			if tc.bindOther {
				boundWorktree = t.TempDir()
			}
			authority, err := plannerartifact.CreateOrLoad(coordinator, plannerartifact.Binding{
				IssueID: "GH-62", Stage: "plan", Attempt: 1, Worktree: boundWorktree,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer authority.Close()

			request := plannerartifact.WriteRequest{
				Manifest: contractManifest(), Key: "goal", Markdown: "boundary section", Globs: []string{"internal/goal/**"},
			}
			requestBytes, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			requestPath := filepath.Join(t.TempDir(), "request.json")
			if err := os.WriteFile(requestPath, requestBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			capture := map[string]string{}
			for _, name := range []string{"argv", "environment", "stderr", "apply"} {
				capture[name] = filepath.Join(t.TempDir(), name)
			}
			marker := "authority-test-secret-marker"
			shim := writeBoundaryShim(t, helper, requestPath, capture)
			r := testRunner(shim)
			r.ExtraEnv = []string{
				"BOUNDARY_HELPER=" + helper,
				"BOUNDARY_REQUEST=" + requestPath,
				"BOUNDARY_CAPTURE_ARGV=" + capture["argv"],
				"BOUNDARY_CAPTURE_ENV=" + capture["environment"],
				"BOUNDARY_CAPTURE_STDERR=" + capture["stderr"],
				"BOUNDARY_CAPTURE_APPLY=" + capture["apply"],
			}
			t.Setenv("WATCHTOWER_PLANNER_SESSION", marker)
			var lines []string
			r.OnLine = func(_, _, line string) { lines = append(lines, line) }
			ctx := context.Background()
			if !tc.noDesc {
				ctx = runner.WithPlannerArtifactAuthority(ctx, authority)
			}
			result := <-r.RunPlanner(ctx, "GH-62", "plan", "executor", current, make(chan runner.Ask), nil)
			if tc.wantErr != (result.Err != nil) {
				t.Fatalf("result error = %v, want error=%v", result.Err, tc.wantErr)
			}
			if tc.wantOutput {
				plan, err := os.ReadFile(filepath.Join(current, "plan.md"))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Count(string(plan), "key=goal") != 2 {
					t.Fatalf("authorized pair = %q", plan)
				}
				apply, _ := os.ReadFile(capture["apply"])
				if string(apply) != "section-validated goal\n" {
					t.Fatalf("apply response = %q", apply)
				}
				if len(lines) == 0 {
					t.Fatal("real provider stream produced no lines")
				}
			} else {
				plan, _ := os.ReadFile(filepath.Join(current, "plan.md"))
				if string(plan) != "# Implementation Plan\n\n" {
					t.Fatalf("rejected request mutated plan: %q", plan)
				}
			}
			captures := captureBoundary(t, capture, lines, result)
			for name, value := range captures {
				for _, secret := range []string{marker, "planner-authority", "planner-authority-server"} {
					if strings.Contains(value, secret) {
						t.Errorf("%s leaked %q: %s", name, secret, value)
					}
				}
			}
		})
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime caller unavailable")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func buildWatchtowerHelper(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "watchtower")
	cmd := exec.Command("go", "build", "-o", path, "./cmd/watchtower")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build watchtower helper: %v: %s", err, output)
	}
	return path
}

func writeBoundaryShim(t *testing.T, helper, request string, capture map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex-shim")
	apply := fmt.Sprintf("%q planner-artifact apply --request-file %q", helper, request)
	script := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' "$@" > %q
env > %q
exec 2> %q
if %s 3<&3 > %q; then
  printf '%%s\n' '{"type":"thread.started","thread_id":"boundary-thread"}'
  printf '%%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"boundary line"}}'
  printf '%%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
else
  exit 1
fi
`, capture["argv"], capture["environment"], capture["stderr"], apply, capture["apply"])
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func captureBoundary(t *testing.T, capture map[string]string, lines []string, result runner.Result) map[string]string {
	t.Helper()
	values := map[string]string{"lines": strings.Join(lines, "\n")}
	for name, path := range capture {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		values[name] = string(data)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	values["result"] = string(encoded)
	return values
}

func contractManifest() plannerartifact.Manifest {
	return plannerartifact.Manifest{Sections: []plannerartifact.ManifestEntry{
		{Key: "goal", Globs: []string{"internal/goal/**"}},
		{Key: "architecture", Globs: []string{"internal/architecture/**"}},
		{Key: "technology-stack", Globs: []string{"internal/technology/**"}},
		{Key: "execution-contract", Globs: []string{"internal/contract/**"}},
		{Key: "file-structure", Globs: []string{"internal/files/**"}},
		{Key: "task-0001", Globs: []string{"internal/task/**"}},
		{Key: "verification", Globs: []string{"internal/verification/**"}},
	}}
}
