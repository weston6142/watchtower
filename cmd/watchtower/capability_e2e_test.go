package main_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/proto"
	"github.com/weston6142/watchtower/internal/repocfg"
)

func TestCapabilityE2EArtifactReadonlyAndScopedMutation(t *testing.T) {
	h := newFlowE2E(t, e2eOptions{mode: "profile-matrix"})
	id := h.createIssue("capability scoped mutation")
	h.waitForIssueState(id, "done", 20*time.Second)
	if body, err := os.ReadFile(filepath.Join(h.repo, "feature.txt")); err != nil || string(body) != "delivered\n" {
		t.Fatalf("scoped mutation = %q err=%v", body, err)
	}
	for path, want := range map[string]string{
		"created.txt":   "created\n",
		"renamed.txt":   "renamed\n",
		"review.txt":    "reviewed\n",
		"docs/guide.md": "documented\n",
	} {
		body, err := os.ReadFile(filepath.Join(h.repo, path))
		if err != nil || string(body) != want {
			t.Fatalf("scoped mutation %s = %q err=%v", path, body, err)
		}
	}
	for _, path := range []string{"delete.txt", "rename.txt"} {
		if _, err := os.Stat(filepath.Join(h.repo, path)); !os.IsNotExist(err) {
			t.Fatalf("scoped deletion/rename left %s: %v", path, err)
		}
	}
	client, err := proto.Dial(h.socketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	response, err := client.Do(proto.Command{Op: "setup_outline", IssueID: id})
	if err != nil || !response.OK || response.Setup == nil {
		t.Fatalf("setup = %+v err=%v", response, err)
	}
	want := map[string]string{
		"prepare-input": "artifact", "inspect-input": "inspect", "change-repository": "implementation",
		"review-change": "review", "document-change": "librarian", "integrate-safely": "final-review",
	}
	for _, stage := range response.Setup.Stages {
		if profile := want[stage.Name]; profile != "" {
			if stage.CapabilityProfile != profile || stage.EffectiveCapability == nil || stage.EffectiveCapability.Validation != "passed" {
				t.Fatalf("stage capability %s = %+v", stage.Name, stage)
			}
		}
	}
	for stage := range want {
		found := false
		for _, run := range h.issueDetail(id).Runs {
			found = found || run.Stage == stage && run.Status == "succeeded"
		}
		if !found {
			t.Fatalf("profile stage %s did not execute successfully: %+v", stage, h.issueDetail(id).Runs)
		}
	}
	conflictResolved := false
	for _, run := range h.issueDetail(id).Runs {
		conflictResolved = conflictResolved || run.Stage == "conflict-resolution" && run.Status == "succeeded"
	}
	if !conflictResolved {
		t.Fatalf("engine-owned conflict resolution did not run: %+v", h.issueDetail(id).Runs)
	}
}

func TestCapabilityE2ELifecycleBypassesHaveNoEffect(t *testing.T) {
	h := newFlowE2E(t, e2eOptions{mode: "runtime-denial"})
	before := strings.TrimSpace(h.git(h.remote, "rev-parse", "main"))
	id := h.createIssue("agent lifecycle bypass")
	h.waitForIssueState(id, "failed", 20*time.Second)
	time.Sleep(750 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(h.root, "prohibited-side-effect")); !os.IsNotExist(err) {
		t.Fatalf("provider descendant survived runtime denial: %v", err)
	}
	after := strings.TrimSpace(h.git(h.remote, "rev-parse", "main"))
	if after != before {
		t.Fatalf("agent receipt bypass moved remote: %s -> %s", before, after)
	}
	receipt := filepath.Join(repocfg.RepoDataDir(h.base, h.repo), "issues", id, "artifacts", "verification.json")
	if _, err := os.Stat(receipt); !os.IsNotExist(err) {
		t.Fatalf("agent-authored receipt became durable engine evidence: %v", err)
	}
}

func TestCapabilityE2EDishonestProviderCannotAdvance(t *testing.T) {
	h := newFlowE2E(t, e2eOptions{mode: "outside-mutation"})
	before := strings.TrimSpace(h.git(h.remote, "rev-parse", "main"))
	id := h.createIssue("dishonest provider")
	h.waitForIssueState(id, "failed", 20*time.Second)
	if _, err := os.Stat(filepath.Join(h.repo, "outside.txt")); !os.IsNotExist(err) {
		t.Fatalf("dishonest mutation reached base checkout: %v", err)
	}
	after := strings.TrimSpace(h.git(h.remote, "rev-parse", "main"))
	if after != before {
		t.Fatalf("dishonest provider advanced remote: %s -> %s", before, after)
	}
	if h.eventExists(id, "verification_ready") || h.eventExists(id, "issue_merged") {
		t.Fatalf("dishonest provider reached lifecycle authority:\n%s", h.diagnostics(id))
	}
}

func TestCapabilityE2ERecoveryAndRestartFailClosed(t *testing.T) {
	h := newFlowE2E(t, e2eOptions{mode: "pause-change"})
	id := h.createIssue("capability restart")
	h.waitForFile(filepath.Join(h.lanes, "change-started"), 10*time.Second)
	h.killDaemon()
	h.waitForFile(filepath.Join(h.lanes, "change-aborted"), 10*time.Second)
	if body, err := os.ReadFile(filepath.Join(h.repo, "feature.txt")); err != nil || string(body) != "base\n" {
		t.Fatalf("interrupted unvalidated bytes changed base: body=%q err=%v", body, err)
	}
	run(h.t, h.bin, h.repo, "status", "--data", h.base)
	h.waitForIssueState(id, "failed", 10*time.Second)
	if body, err := os.ReadFile(filepath.Join(h.repo, "feature.txt")); err != nil || string(body) != "base\n" {
		t.Fatalf("restart accepted interrupted unvalidated bytes: body=%q err=%v", body, err)
	}
}

func TestCapabilityE2EEngineLifecycleStillCompletes(t *testing.T) {
	h := newFlowE2E(t, e2eOptions{})
	id := h.createIssue("engine lifecycle")
	h.waitForIssueState(id, "done", 20*time.Second)
	local := strings.TrimSpace(h.git(h.repo, "rev-parse", "main"))
	remote := strings.TrimSpace(h.git(h.remote, "rev-parse", "main"))
	if local != remote {
		t.Fatalf("engine publication parity: local=%s remote=%s", local, remote)
	}
	receipt := filepath.Join(repocfg.RepoDataDir(h.base, h.repo), "issues", id, "artifacts", "verification.json")
	if body, err := os.ReadFile(receipt); err != nil || !strings.Contains(string(body), `"cache_evidence"`) {
		t.Fatalf("engine verification receipt = %q err=%v", body, err)
	}
	if h.treehouseReturnCount() != 1 {
		t.Fatalf("workspace returns = %d", h.treehouseReturnCount())
	}
	if _, err := os.Stat(h.leasedWorktree(id)); !os.IsNotExist(err) {
		t.Fatalf("engine did not release issue worktree: %v", err)
	}
}

func TestCapabilityE2EProviderProcessRunsInsideContainment(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			h := newFlowE2E(t, e2eOptions{mode: "provider-containment", runner: provider})
			id := h.createIssue(provider + " provider containment")
			h.waitForIssueState(id, "failed", 20*time.Second)
			if _, err := os.Stat(filepath.Join(h.root, "provider-escaped")); !os.IsNotExist(err) {
				t.Fatalf("%s provider bypassed containment: %v", provider, err)
			}
			started := false
			for _, run := range h.issueDetail(id).Runs {
				started = started || run.SessionID == "provider-contained"
			}
			if !started {
				t.Fatalf("%s provider did not start inside the supported containment backend:\n%s", provider, h.diagnostics(id))
			}
		})
	}
}
