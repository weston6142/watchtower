package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/plannerartifact"
	"github.com/weston6142/watchtower/internal/store"
)

func TestPlannerArtifactCommandKeepsPayloadOutOfArgv(t *testing.T) {
	dir := t.TempDir()
	coordinator, err := store.Open(filepath.Join(t.TempDir(), "coordinator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	authority, err := plannerartifact.CreateOrLoad(coordinator, plannerartifact.Binding{
		IssueID: "GH-62", Stage: "plan", Attempt: 1, Worktree: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	manifest := commandManifest()
	payload := "quotes ' \" backticks ` $()\n```json\n{\"key\":\"value\"}\n```"
	request := plannerartifact.WriteRequest{
		Manifest: manifest,
		Key:      "goal",
		Markdown: payload,
		Globs:    manifest.Sections[0].Globs,
	}
	requestBytes, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(requestPath, requestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WATCHTOWER_PLANNER_SESSION", "agent-private-session")
	args := []string{"planner-artifact", "apply", "--request-file", requestPath}
	if strings.Contains(strings.Join(args, " "), payload) {
		t.Fatal("request payload was interpolated into command arguments")
	}
	descriptor, err := authority.AttachPlannerArtifactDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPlannerArtifactCommandHelper$", "--")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "WATCHTOWER_PLANNER_HELPER=1", "WATCHTOWER_PLANNER_REQUEST="+requestPath, "WATCHTOWER_PLANNER_SESSION=agent-private-session")
	cmd.ExtraFiles = []*os.File{descriptor}
	output, err := cmd.CombinedOutput()
	_ = descriptor.Close()
	if err != nil {
		t.Fatalf("planner helper: %v: %s", err, output)
	}
	if got := string(output); !strings.HasPrefix(got, "section-validated goal\n") {
		t.Fatalf("stdout = %q", got)
	}
	if err := authority.VerifyBinding(plannerartifact.Binding{IssueID: "GH-62", Stage: "plan", Attempt: 1, Worktree: dir}); err != nil {
		t.Fatalf("authority after helper: %v", err)
	}
	plan, err := os.ReadFile(filepath.Join(dir, "plan.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plan), payload) {
		t.Fatalf("plan does not contain payload byte-for-byte: %q", plan)
	}
}

func TestPlannerArtifactCommandHelper(t *testing.T) {
	if os.Getenv("WATCHTOWER_PLANNER_HELPER") != "1" {
		return
	}
	requestPath := os.Getenv("WATCHTOWER_PLANNER_REQUEST")
	if err := runPlannerArtifact([]string{"planner-artifact", "apply", "--request-file", requestPath}, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func commandManifest() plannerartifact.Manifest {
	return plannerartifact.Manifest{Sections: []plannerartifact.ManifestEntry{
		{Key: "goal", Globs: []string{"internal/gh40/goal/**"}},
		{Key: "architecture", Globs: []string{"internal/gh40/architecture/**"}},
		{Key: "technology-stack", Globs: []string{"internal/gh40/technology/**"}},
		{Key: "execution-contract", Globs: []string{"internal/gh40/contract/**"}},
		{Key: "file-structure", Globs: []string{"internal/gh40/files/**"}},
		{Key: "task-0001", Globs: []string{"internal/gh40/task-0001/**"}},
		{Key: "verification", Globs: []string{"internal/gh40/verification/**"}},
	}}
}
