//go:build unix

package runner_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/runner"
)

func TestProcessTreeTerminateAndWaitReapsDescendants(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	tree, err := runner.StartProcessTree(context.Background(), runner.ProcessSpec{
		Path: "/bin/sh",
		Args: []string{"-c", `trap '' TERM; (trap '' TERM; while :; do sleep 1; done) & echo $! > "$1"; while :; do sleep 1; done`, "process-tree", pidFile},
	})
	if err != nil {
		t.Fatal(err)
	}
	var childPID int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		body, readErr := os.ReadFile(pidFile)
		if readErr == nil {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(body)))
			if childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("descendant pid was not published")
	}
	if err := tree.TerminateAndWait(100 * time.Millisecond); err == nil {
		t.Fatal("forced process-tree termination reported success")
	}
	if err := syscall.Kill(childPID, 0); err == nil {
		t.Fatalf("descendant %d is still alive", childPID)
	}
}
