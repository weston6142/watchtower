package main_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/proto"
	"github.com/weston6142/watchtower/internal/repocfg"
)

func TestTowerReconnectsAfterDaemonReplacement(t *testing.T) {
	bin, base, repo := newRepo(t)
	firstTitle := "GH-37 first focus"
	secondTitle := "GH-37 second focus"
	firstID := strings.TrimSpace(lastLine(run(t, bin, repo, "new", "--data", base, "--preset", "strict", "--title", firstTitle)))
	secondID := strings.TrimSpace(lastLine(run(t, bin, repo, "new", "--data", base, "--preset", "strict", "--title", secondTitle)))
	if firstID == "" || secondID == "" || firstID == secondID {
		t.Fatalf("created issue IDs = %q, %q", firstID, secondID)
	}
	deadline := time.Now().Add(5 * time.Second)
	issuesReady := false
	for time.Now().Before(deadline) {
		issues := run(t, bin, repo, "issues", "--data", base)
		if strings.Contains(issues, firstTitle) && strings.Contains(issues, secondTitle) {
			issuesReady = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !issuesReady {
		t.Fatalf("issues did not become visible:\n%s", run(t, bin, repo, "issues", "--data", base))
	}

	tower := startTowerProcess(t, bin, base, repo)
	tower.waitForOutput(firstTitle, 5*time.Second)

	socket := filepath.Join(repocfg.RepoDataDir(base, repo), "watchtower.sock")
	originalPID := daemonPID(t, base, repo)
	if err := syscall.Kill(originalPID, syscall.SIGKILL); err != nil {
		t.Fatalf("stop original daemon %d: %v", originalPID, err)
	}
	waitForProcessState(t, originalPID, false, 5*time.Second)
	waitForSocketUnavailable(t, socket, 5*time.Second)
	reconnecting := tower.waitForOutput("reconnecting", 5*time.Second)
	if !tower.alive() {
		t.Fatal("tower exited during daemon outage")
	}
	if strings.Contains(strings.ToLower(reconnecting), "unix") {
		t.Fatalf("reconnecting view exposed a raw Unix-socket error:\n%s", reconnecting)
	}

	tower.send("p")
	time.Sleep(100 * time.Millisecond)
	tower.send("?")
	tower.waitForOutput("every key in the control room", 3*time.Second)
	tower.send("?")

	if processAlive(originalPID) {
		t.Fatalf("original daemon %d is still alive", originalPID)
	}
	dataDir := repocfg.RepoDataDir(base, repo)
	if body, err := os.ReadFile(filepath.Join(dataDir, "daemon.pid")); err == nil {
		pid := parsePID(t, body)
		if pid != originalPID && processAlive(pid) {
			t.Fatalf("tower spawned daemon %d while original %d was down", pid, originalPID)
		}
	}

	tower.resetOutput()
	replacement := startReplacementDaemon(t, bin, base, repo)
	tower.waitForOutput(firstTitle, 8*time.Second)
	if !tower.alive() {
		t.Fatal("tower exited after the first daemon replacement")
	}
	if running := strings.Count(run(t, bin, repo, "repos", "--data", base), "running"); running != 1 {
		t.Fatalf("running daemon count = %d, want 1", running)
	}
	if hasRepoEvent(t, base, repo, firstID, core.EvIssuePaused) {
		t.Fatal("connection-dependent input during outage was replayed after recovery")
	}

	run(t, bin, repo, "abandon", "--data", base, firstID)
	stopProcess(t, replacement)
	replacement = nil
	waitForSocketUnavailable(t, socket, 5*time.Second)
	tower.resetOutput()
	tower.waitForOutput("reconnecting", 5*time.Second)

	secondReplacement := startReplacementDaemon(t, bin, base, repo)
	tower.waitForOutput(secondTitle, 8*time.Second)
	latest := ansi.Strip(tower.snapshot())
	if strings.Contains(latest, firstTitle) {
		t.Fatalf("removed focused issue remained visible after fallback:\n%s", latest)
	}

	tower.send("q")
	select {
	case <-tower.done:
	case <-time.After(3 * time.Second):
		t.Fatal("tower did not exit after q")
	}
	if processAlive(secondReplacement.Process.Pid) == false {
		t.Fatal("replacement daemon stopped unexpectedly before tower shutdown")
	}
	if body, err := os.ReadFile(filepath.Join(dataDir, "daemon.pid")); err == nil {
		pid := parsePID(t, body)
		if pid != secondReplacement.Process.Pid {
			t.Fatalf("daemon PID after tower shutdown = %d, want replacement %d", pid, secondReplacement.Process.Pid)
		}
	}
	stopProcess(t, secondReplacement)
}

func parsePID(t *testing.T, body []byte) int {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func hasRepoEvent(t *testing.T, base, repo string, issueID string, eventType core.EventType) bool {
	t.Helper()
	client, err := proto.Dial(filepath.Join(repocfg.RepoDataDir(base, repo), "watchtower.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	response, err := client.Do(proto.Command{Op: "tail", SinceSeq: 0})
	if err != nil || !response.OK {
		t.Fatalf("tail: %v response=%+v", err, response)
	}
	for _, event := range response.Events {
		if event.IssueID == issueID && event.Type == eventType {
			return true
		}
	}
	return false
}
