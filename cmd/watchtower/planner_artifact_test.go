package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	t.Chdir(dir)
	args := []string{"planner-artifact", "apply", "--request-file", requestPath}
	if strings.Contains(strings.Join(args, " "), payload) {
		t.Fatal("request payload was interpolated into command arguments")
	}
	descriptor, err := authority.AttachPlannerArtifactDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	saved, savedErr := syscall.Dup(3)
	if err := syscall.Dup2(int(descriptor.Fd()), 3); err != nil {
		descriptor.Close()
		if savedErr == nil {
			_ = syscall.Close(saved)
		}
		t.Fatal(err)
	}
	_ = descriptor.Close()
	defer func() {
		if savedErr == nil {
			_ = syscall.Dup2(saved, 3)
			_ = syscall.Close(saved)
			return
		}
		_ = syscall.Close(3)
	}()
	var stdout strings.Builder
	if err := runPlannerArtifact(args, strings.NewReader("ignored stdin"), &stdout); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "section-validated goal\n" {
		t.Fatalf("stdout = %q", got)
	}
	plan, err := os.ReadFile(filepath.Join(dir, "plan.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plan), payload) {
		t.Fatalf("plan does not contain payload byte-for-byte: %q", plan)
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
