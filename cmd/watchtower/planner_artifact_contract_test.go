package main

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

func TestInstalledPlannerArtifactBoundaryUsesDaemonSocket(t *testing.T) {
	installed := os.Getenv("WATCHTOWER_INSTALLED_BIN")
	if installed == "" {
		installed = "/Users/weston.bushyeager/go/bin/watchtower"
	}
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("installed watchtower binary: %v", err)
	}
	repo, err := os.MkdirTemp("/tmp", "g72-repo-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repo) })
	if err := os.MkdirAll(filepath.Join(repo, ".watchtower"), 0o700); err != nil {
		t.Fatal(err)
	}
	home, err := os.MkdirTemp("/tmp", "g72-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	dataRoot := filepath.Join(home, ".local", "share", "watchtower")
	socketDir := repocfg.RepoDataDir(dataRoot, repo)
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(socketDir, sockFileName)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	coordinator, err := store.Open(filepath.Join(socketDir, "watchtower.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	manifest := commandManifest()
	authority, err := plannerartifact.CreateOrLoad(coordinator, plannerartifact.Binding{IssueID: "GH-72", Stage: "plan", Attempt: 1, Worktree: repo})
	if err != nil {
		t.Fatal(err)
	}
	goal := plannerartifact.WriteRequest{Manifest: manifest, Key: "goal", Markdown: "durable installed goal request", Globs: manifest.Sections[0].Globs}
	if err := authority.Apply(goal); err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "plan.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "touchset.json"), []byte("tampered installed touchset"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A restarted daemon has the same file-backed coordinator but no old live
	// authority. It must rebuild authority from durable state before the CLI can
	// apply the next section.
	engineInstance := engine.New(engine.Config{Store: coordinator, DataDir: filepath.Join(socketDir, "issues")})
	server := proto.NewServer(engineInstance, coordinator)
	go func() { _ = server.Serve(listener) }()

	request := plannerartifact.WriteRequest{Manifest: manifest, Key: "architecture", Markdown: "final installed architecture request", Globs: manifest.Sections[1].Globs}
	requestPath := filepath.Join(repo, "request.json")
	requestBytes, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestPath, requestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(installed, "planner-artifact", "apply", "--request-file", requestPath)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "HOME="+home, "WATCHTOWER_PLANNER_SESSION=private-installed-session")
	if len(cmd.ExtraFiles) != 0 || strings.Contains(strings.Join(cmd.Args, " "), request.Markdown) {
		t.Fatal("installed boundary placed descriptor or request body in process arguments")
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("installed CLI: %v output=%q", err, output)
	}
	if string(output) != "section-validated architecture\n" {
		t.Fatalf("installed CLI output = %q", output)
	}
	if strings.Contains(string(output), request.Markdown) || strings.Contains(string(output), "private-installed-session") {
		t.Fatalf("installed output disclosed request or private session: %q", output)
	}
	plan, err := os.ReadFile(filepath.Join(repo, "plan.md"))
	if err != nil || !strings.Contains(string(plan), goal.Markdown) || !strings.Contains(string(plan), request.Markdown) {
		t.Fatalf("installed daemon route did not publish plan: err=%v plan=%q", err, plan)
	}
	if strings.Count(string(plan), "key=goal") != 2 || strings.Count(string(plan), "key=architecture") != 2 {
		t.Fatalf("installed recovery did not retain exactly one goal and architecture anchor pair: %q", plan)
	}
	touchset, err := os.ReadFile(filepath.Join(repo, "touchset.json"))
	if err != nil || !strings.Contains(string(touchset), manifest.Sections[0].Globs[0]) || !strings.Contains(string(touchset), manifest.Sections[1].Globs[0]) {
		t.Fatalf("installed recovery touchset omitted durable union: err=%v touchset=%q", err, touchset)
	}
	status, _, storedManifest, storedSections, found, err := coordinator.LoadPlannerArtifact("GH-72", "plan", 1, authority.Binding().Worktree)
	if err != nil || !found || status != "active" || !strings.Contains(string(storedManifest), `"goal"`) || !strings.Contains(string(storedSections), goal.Markdown) || !strings.Contains(string(storedSections), request.Markdown) {
		t.Fatalf("durable installed authority state = status=%q found=%v manifest=%s sections=%s err=%v", status, found, storedManifest, storedSections, err)
	}
	for _, forbidden := range []string{goal.Markdown, request.Markdown, authority.CapabilityHandle(), "private-installed-session"} {
		if strings.Contains(strings.Join(cmd.Args, " "), forbidden) || strings.Contains(string(output), forbidden) {
			t.Fatalf("installed argv/output disclosed planner-private content: argv=%q output=%q", cmd.Args, output)
		}
	}
}
