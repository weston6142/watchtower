package codex

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/engine"
	"github.com/weston6142/watchtower/internal/plannerartifact"
	"github.com/weston6142/watchtower/internal/proto"
	"github.com/weston6142/watchtower/internal/repocfg"
	"github.com/weston6142/watchtower/internal/store"
)

func TestInstalledCodexPlannerBoundary(t *testing.T) {
	installed := os.Getenv("WATCHTOWER_INSTALLED_BIN")
	if installed == "" {
		installed = "/Users/weston.bushyeager/go/bin/watchtower"
	}
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("installed watchtower binary: %v", err)
	}
	for _, issueID := range []string{"GH-63", "GH-64"} {
		t.Run(issueID, func(t *testing.T) {
			repo, home, coordinator, listener, authority, requestPath := startInstalledDaemon(t, issueID)
			request := installedContractRequest(issueID)
			data, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(requestPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(installed, "planner-artifact", "apply", "--request-file", requestPath)
			cmd.Dir = repo
			cmd.Env = append(os.Environ(), "HOME="+home, "WATCHTOWER_PLANNER_SESSION=private-"+issueID)
			if len(cmd.ExtraFiles) != 0 || strings.Contains(strings.Join(cmd.Args, " "), request.Markdown) {
				t.Fatal("installed Codex boundary used a descriptor or placed the body in argv")
			}
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("installed planner route: %v output=%q", err, output)
			}
			if string(output) != "section-validated goal\n" {
				t.Fatalf("installed planner output = %q", output)
			}
			if strings.Contains(string(output), request.Markdown) || strings.Contains(string(output), "private-"+issueID) {
				t.Fatalf("installed planner output disclosed request or private session: %q", output)
			}
			plan, err := os.ReadFile(filepath.Join(repo, "plan.md"))
			if err != nil || !strings.Contains(string(plan), request.Markdown) {
				t.Fatalf("installed planner did not publish final goal: err=%v plan=%q", err, plan)
			}
			status, _, _, sections, found, err := coordinator.LoadPlannerArtifact(issueID, "plan", 1, authority.Binding().Worktree)
			if err != nil || !found || status != "active" || !strings.Contains(string(sections), request.Markdown) {
				t.Fatalf("durable planner state = status=%q found=%v sections=%s err=%v", status, found, sections, err)
			}
			_ = listener
			_ = authority
		})
	}
}

func startInstalledDaemon(t *testing.T, issueID string) (repo, home string, coordinator *store.Store, listener net.Listener, authority *plannerartifact.Authority, requestPath string) {
	t.Helper()
	var err error
	repo, err = os.MkdirTemp("/tmp", "g72-repo-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repo) })
	if err := os.MkdirAll(filepath.Join(repo, ".watchtower"), 0o700); err != nil {
		t.Fatal(err)
	}
	home, err = os.MkdirTemp("/tmp", "g72-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	dataRoot := filepath.Join(home, ".local", "share", "watchtower")
	socketDir := repocfg.RepoDataDir(dataRoot, repo)
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err = net.Listen("unix", filepath.Join(socketDir, "watchtower.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	coordinator, err = store.Open(filepath.Join(socketDir, "watchtower.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	authority, err = plannerartifact.CreateOrLoad(coordinator, plannerartifact.Binding{IssueID: issueID, Stage: "plan", Attempt: 1, Worktree: repo})
	if err != nil {
		t.Fatal(err)
	}
	engineInstance := engine.New(engine.Config{Store: coordinator, DataDir: filepath.Join(socketDir, "issues")})
	engineInstance.RegisterPlannerAuthority(authority)
	go func() { _ = proto.NewServer(engineInstance, coordinator).Serve(listener) }()
	requestPath = filepath.Join(repo, "request.json")
	return repo, home, coordinator, listener, authority, requestPath
}

func installedContractRequest(issueID string) plannerartifact.WriteRequest {
	manifest := plannerartifact.Manifest{Sections: []plannerartifact.ManifestEntry{
		{Key: "goal", Globs: []string{"internal/" + strings.ToLower(issueID) + "/goal/**"}},
		{Key: "architecture", Globs: []string{"internal/" + strings.ToLower(issueID) + "/architecture/**"}},
		{Key: "technology-stack", Globs: []string{"internal/" + strings.ToLower(issueID) + "/technology/**"}},
		{Key: "execution-contract", Globs: []string{"internal/" + strings.ToLower(issueID) + "/contract/**"}},
		{Key: "file-structure", Globs: []string{"internal/" + strings.ToLower(issueID) + "/files/**"}},
		{Key: "task-0001", Globs: []string{"internal/" + strings.ToLower(issueID) + "/task/**"}},
		{Key: "verification", Globs: []string{"internal/" + strings.ToLower(issueID) + "/verification/**"}},
	}}
	return plannerartifact.WriteRequest{Manifest: manifest, Key: "goal", Markdown: "final reviewed " + issueID + " goal", Globs: manifest.Sections[0].Globs}
}
