package main_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/store"
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

func TestCustomFlowReverifiesMovedBaseBeforeMerge(t *testing.T) {
	h := newFlowE2E(t, e2eOptions{mode: "advance-base"})
	id := h.createIssue("moved base")
	h.waitForIssueState(id, "done", 20*time.Second)
	if !h.eventExists(id, "issue_merged") {
		t.Fatalf("issue did not merge after moved-base replay:\n%s", h.diagnostics(id))
	}
	if _, err := os.Stat(filepath.Join(h.repo, "base-advanced.txt")); err != nil {
		t.Fatalf("advanced base content missing: %v", err)
	}
}

func TestCustomFlowRetriesPublicationWithoutRemerging(t *testing.T) {
	h := newFlowE2E(t, e2eOptions{mode: "publish-fail"})
	id := h.createIssue("publication retry")
	h.waitForIntegrationState(id, store.IntegrationPublishPending, 20*time.Second)
	before := h.mergeCommitCount()
	h.repairOrigin()
	run(h.t, h.bin, h.repo, "retry", "--data", h.base, id)
	h.waitForIssueState(id, "done", 20*time.Second)
	if after := h.mergeCommitCount(); after != before {
		t.Fatalf("publication retry created another merge: %d -> %d", before, after)
	}
}

func TestCustomFlowRetriesTreehouseCleanupWithoutRemerging(t *testing.T) {
	h := newFlowE2E(t, e2eOptions{mode: "cleanup-fail-once"})
	id := h.createIssue("cleanup retry")
	h.waitForIssueState(id, "cleanup_needed", 20*time.Second)
	before := h.mergeCommitCount()
	run(h.t, h.bin, h.repo, "retry", "--data", h.base, id)
	h.waitForIssueState(id, "done", 20*time.Second)
	if after := h.mergeCommitCount(); after != before {
		t.Fatalf("cleanup retry created another merge: %d -> %d", before, after)
	}
	if h.treehouseReturnCount() != 1 {
		t.Fatalf("successful returns = %d", h.treehouseReturnCount())
	}
}

func TestCustomFlowDaemonRestartPreservesWorkspaceForStageRetry(t *testing.T) {
	h := newFlowE2E(t, e2eOptions{mode: "pause-change"})
	id := h.createIssue("restart")
	h.waitForFile(filepath.Join(h.lanes, "change-started"), 10*time.Second)
	h.killDaemon()
	h.waitForFile(filepath.Join(h.lanes, "change-aborted"), 10*time.Second)
	if err := os.WriteFile(filepath.Join(h.lanes, "change-released"), []byte("go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(h.t, h.bin, h.repo, "status", "--data", h.base)
	h.waitForIssueState(id, "failed", 10*time.Second)
	run(h.t, h.bin, h.repo, "retry", "--data", h.base, id)
	h.waitForIssueState(id, "done", 20*time.Second)
	if body, err := os.ReadFile(filepath.Join(h.repo, "feature.txt")); err != nil || string(body) != "delivered\n" {
		t.Fatalf("landed feature = %q err %v", body, err)
	}
}
