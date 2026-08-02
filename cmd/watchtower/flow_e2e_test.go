package main_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCustomFlowTraversesCodexTreehouseMergePushAndCleanup(t *testing.T) {
	h := newFlowE2E(t, e2eOptions{})
	id := h.createIssue("custom infrastructure flow")
	h.waitForIssueState(id, "done", 20*time.Second)

	detail := h.issueDetail(id)
	if len(detail.Runs) != 3 {
		t.Fatalf("stage runs = %+v", detail.Runs)
	}
	wantStages := []string{"prepare-input", "change-repository", "integrate-safely"}
	for index, want := range wantStages {
		if detail.Runs[index].Stage != want || detail.Runs[index].Status != "succeeded" {
			t.Fatalf("stage run %d = %+v, want %s succeeded", index, detail.Runs[index], want)
		}
	}

	local := strings.TrimSpace(h.git(h.repo, "rev-parse", "main"))
	remote := strings.TrimSpace(h.git(h.remote, "rev-parse", "main"))
	if local != remote {
		t.Fatalf("local main %s != remote main %s", local, remote)
	}
	if body, err := os.ReadFile(filepath.Join(h.repo, "feature.txt")); err != nil || string(body) != "delivered\n" {
		t.Fatalf("landed feature = %q err %v", body, err)
	}
	if branch := strings.TrimSpace(h.git(h.repo, "branch", "--list", "issue/"+id)); branch != "" {
		t.Fatalf("issue branch remains: %q", branch)
	}
	if h.treehouseReturnCount() != 1 {
		t.Fatalf("treehouse returns = %d", h.treehouseReturnCount())
	}
	if _, err := os.Stat(h.leasedWorktree(id)); !os.IsNotExist(err) {
		t.Fatalf("leased worktree still exists: %v", err)
	}
}
