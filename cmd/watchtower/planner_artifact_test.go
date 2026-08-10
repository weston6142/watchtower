package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/plannerartifact"
	"github.com/weston6142/watchtower/internal/proto"
)

func TestPlannerArtifactCommandKeepsPayloadOutOfArgv(t *testing.T) {
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
	socketDir, err := os.MkdirTemp("/tmp", "g72-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "planner.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		scanner := bufio.NewScanner(conn)
		encoder := json.NewEncoder(conn)
		for scanner.Scan() {
			var command proto.Command
			if json.Unmarshal(scanner.Bytes(), &command) != nil {
				return
			}
			if command.Op == "planner_authority_issue" {
				_ = encoder.Encode(proto.Response{OK: true, PlannerHandle: "opaque"})
			} else {
				_ = encoder.Encode(proto.Response{OK: true, SectionKey: command.PlannerRequest.Key})
			}
		}
	}()
	var output bytes.Buffer
	err = runPlannerArtifact(append(args, "--socket", socket), strings.NewReader(""), &output)
	if err != nil {
		t.Fatalf("planner CLI: %v", err)
	}
	if got := output.String(); got != "section-validated goal\n" {
		t.Fatalf("stdout = %q", got)
	}
	if strings.Contains(output.String(), payload) || strings.Contains(output.String(), "opaque") {
		t.Fatal("planner payload or capability appeared in CLI output")
	}
}

func TestPlannerArtifactCLIUsesDaemonRouteWithoutDescriptor(t *testing.T) {
	manifest := commandManifest()
	request := plannerartifact.WriteRequest{Manifest: manifest, Key: "goal", Markdown: "final CLI section", Globs: manifest.Sections[0].Globs}
	requestPath := filepath.Join(t.TempDir(), "request.json")
	requestBytes, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestPath, requestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("/tmp", "g72-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "planner.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	commands := make(chan proto.Command, 2)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		scanner := bufio.NewScanner(conn)
		encoder := json.NewEncoder(conn)
		for scanner.Scan() {
			var command proto.Command
			if json.Unmarshal(scanner.Bytes(), &command) != nil {
				return
			}
			commands <- command
			switch command.Op {
			case "planner_authority_issue":
				_ = encoder.Encode(proto.Response{OK: true, PlannerHandle: "opaque-test-handle"})
			case "apply_planner_artifact":
				_ = encoder.Encode(proto.Response{OK: true, SectionKey: command.PlannerRequest.Key})
			}
		}
	}()

	var stdout bytes.Buffer
	err = runPlannerArtifact([]string{"planner-artifact", "apply", "--request-file", requestPath, "--socket", socket}, strings.NewReader(""), &stdout)
	if err != nil {
		t.Fatalf("daemon-backed CLI: %v", err)
	}
	if got := stdout.String(); got != "section-validated goal\n" {
		t.Fatalf("CLI output = %q", got)
	}
	close(commands)
	var got []proto.Command
	for command := range commands {
		got = append(got, command)
	}
	if len(got) != 2 || got[0].Op != "planner_authority_issue" || got[1].Op != "apply_planner_artifact" {
		t.Fatalf("daemon commands = %+v", got)
	}
	if got[0].PlannerRequest != nil || got[0].PlannerHandle != "" || got[1].PlannerHandle != "opaque-test-handle" || got[1].PlannerRequest == nil {
		t.Fatalf("CLI did not keep handle/request on daemon channel: %+v", got)
	}
	if strings.Contains(stdout.String(), request.Markdown) || strings.Contains(stdout.String(), "opaque-test-handle") {
		t.Fatal("CLI exposed request body or capability in output")
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
