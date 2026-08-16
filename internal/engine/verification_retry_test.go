package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/store"
	"github.com/weston6142/watchtower/internal/verificationcache"
)

type gh79Fixture struct {
	e          *Engine
	s          *store.Store
	repo       string
	id         string
	worktree   string
	parentID   int64
	oldReceipt []byte
	head       string
	tree       string
}

func newGH79Fixture(t *testing.T, commands [][]string, stale, push bool) *gh79Fixture {
	t.Helper()
	e, s, repo := verificationEngine(t, "merge", commands, "")
	e.cfg.Train.TestCmd = append([]string(nil), commands[0]...)
	if push {
		remote := t.TempDir()
		if out, err := exec.Command("git", "-C", remote, "init", "-q", "--bare").CombinedOutput(); err != nil {
			t.Fatalf("init bare remote: %v: %s", err, out)
		}
		if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", remote).CombinedOutput(); err != nil {
			t.Fatalf("add bare remote: %v: %s", err, out)
		}
		e.cfg.Train.Push = true
	}
	fake := e.cfg.Runner.(*runner.FakeRunner)
	fake.OnStart = func(_, stage, _, _ string) error {
		if stage != "merge-verification" {
			return nil
		}
		return os.WriteFile(filepath.Join(repo, "diff"), []byte("dirty base\n"), 0o644)
	}
	id, err := e.CreateIssue("GH-79 matrix", "", "default", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err == nil {
		t.Fatal("dirty base unexpectedly finalized")
	}
	integration, ok, err := s.IssueIntegration(id)
	if err != nil || !ok || integration.State != store.IntegrationVerificationReady {
		t.Fatalf("initial integration = %+v ok=%v err=%v", integration, ok, err)
	}
	attempts, err := s.VerificationAttempts(id)
	if err != nil || len(attempts) != 1 || attempts[0].Status != store.VerificationAttemptPassed {
		t.Fatalf("initial verification attempts = %+v err=%v", attempts, err)
	}
	if out, err := exec.Command("git", "-C", repo, "checkout", "--", "diff").CombinedOutput(); err != nil {
		t.Fatalf("repair base: %v: %s", err, out)
	}
	fixture := &gh79Fixture{
		e: e, s: s, repo: repo, id: id, worktree: integration.Worktree,
		parentID: attempts[0].ID, oldReceipt: append([]byte(nil), attempts[0].ReceiptJSON...),
	}
	if stale {
		if err := os.WriteFile(filepath.Join(fixture.worktree, "stale-proof"), []byte("changed after proof\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"add", "stale-proof"}, {"commit", "-qm", "change after verification"}} {
			if out, err := exec.Command("git", append([]string{"-C", fixture.worktree}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v: %s", args, err, out)
			}
		}
	}
	fixture.head = strings.TrimSpace(gitOutput(t, fixture.worktree, "rev-parse", "HEAD"))
	fixture.tree = strings.TrimSpace(gitOutput(t, fixture.worktree, "rev-parse", "HEAD^{tree}"))
	script := fake.Scripts["merge-verification/merge-verifier"]
	script.Artifacts["merge-decision.json"] = ""
	fake.Scripts["merge-verification/merge-verifier"] = script
	fake.OnStart = nil
	return fixture
}

func gh79FailureInput(issueID string, parentID int64) failure.RecordInput {
	return failure.RecordInput{
		IssueID: issueID, Stage: "merge-verification", StageAttempt: int(parentID),
		FailureSite: failure.SiteFinalization, FailureClass: failure.ClassStateMismatch,
		RetryDisposition: failure.RetryAfterStateChange, RequiredStateChange: failure.StateVerification,
		Fingerprint: failure.BuildFingerprint(failure.FingerprintInputs{
			IssueID: issueID, Stage: "merge-verification", FailureSite: failure.SiteFinalization,
		}),
	}
}

func gh79EventCount(t *testing.T, s *store.Store, issueID string, typ core.EventType) int {
	t.Helper()
	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.IssueID == issueID && event.Type == typ {
			count++
		}
	}
	return count
}

func waitForGH79Integration(t *testing.T, s *store.Store, issueID, want string) store.IssueIntegration {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		integration, ok, err := s.IssueIntegration(issueID)
		if err != nil {
			t.Fatal(err)
		}
		if ok && integration.State == want {
			return integration
		}
		time.Sleep(10 * time.Millisecond)
	}
	integration, _, err := s.IssueIntegration(issueID)
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("integration did not reach %q: %+v", want, integration)
	return store.IssueIntegration{}
}

func TestGH79VerificationRetryMatrix(t *testing.T) {
	t.Run("pre-journal legacy receipt is imported on explicit retry", func(t *testing.T) {
		e, s, repo := verificationEngine(t, "merge", [][]string{{"true"}}, "")
		id, err := e.CreateIssue("GH-79 legacy receipt", "", "default", levers.Matrix{}, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		fake := e.cfg.Runner.(*runner.FakeRunner)
		finalScript := fake.Scripts["merge-verification/merge-verifier"]
		finalScript.Fail = true
		fake.Scripts["merge-verification/merge-verifier"] = finalScript
		startErr := e.StartIssue(context.Background(), id)
		if startErr == nil {
			t.Fatal("fixture final review unexpectedly succeeded")
		}
		finalScript.Fail = false
		fake.Scripts["merge-verification/merge-verifier"] = finalScript
		path := filepath.Join(repo, ".worktrees", id)
		if out, err := exec.Command("git", "-C", repo, "worktree", "add", path, "issue/"+id).CombinedOutput(); err != nil {
			t.Fatalf("restore approved fixture worktree after %v: %v: %s", startErr, err, out)
		}
		t.Cleanup(func() { _, _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", path).CombinedOutput() })
		base := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD"))
		oldTree := strings.TrimSpace(gitOutput(t, path, "rev-parse", "HEAD^{tree}"))
		oldReceipt, err := json.Marshal(marshal.Verification{
			BaseSHA: base, BranchSHA: base, TreeSHA: oldTree, Passed: true,
			Commands: [][]string{{"true"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		artifactDir := filepath.Join(e.issueDir(id), "artifacts")
		if err := os.MkdirAll(artifactDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(artifactDir, "verification.json"), oldReceipt, 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("git", "-C", path, "commit", "--allow-empty", "-qm", "post-verification change").CombinedOutput(); err != nil {
			t.Fatalf("mutate legacy worktree: %v: %s", err, out)
		}
		currentHead := strings.TrimSpace(gitOutput(t, path, "rev-parse", "HEAD"))
		decision, err := json.Marshal(marshal.MergeDecision{
			Decision: "merge", BranchCommit: currentHead, BaseCommit: base,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(artifactDir, "merge-decision.json"), decision, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := s.SetIssueIntegration(store.IssueIntegration{
			IssueID: id, State: store.IntegrationVerificationReady, PreSHA: base,
			Worktree: path, Branch: "issue/" + id,
		}); err != nil {
			t.Fatal(err)
		}
		script := fake.Scripts["merge-verification/merge-verifier"]
		script.Artifacts["merge-decision.json"] = string(decision)
		fake.Scripts["merge-verification/merge-verifier"] = script

		if err := e.RetryStage(context.Background(), id); err != nil {
			t.Fatalf("legacy explicit retry: %v", err)
		}
		attempts, err := s.VerificationAttempts(id)
		if err != nil || len(attempts) != 2 {
			t.Fatalf("legacy attempts = %+v err=%v", attempts, err)
		}
		if attempts[0].Status != store.VerificationAttemptQuarantined ||
			string(attempts[0].ReceiptJSON) != string(oldReceipt) ||
			attempts[1].Status != store.VerificationAttemptPassed {
			t.Fatalf("legacy attempt history = %+v", attempts)
		}
	})

	t.Run("identity mismatches remain independently classified", func(t *testing.T) {
		dir, head := initReceiptRepo(t)
		tree := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD^{tree}"))
		is := &issueState{id: "GH-79", baseRef: head, wsPath: dir}
		decision := marshal.MergeDecision{Decision: "merge", BranchCommit: head, BaseCommit: head}
		receipt := marshal.Verification{BaseSHA: head, BranchSHA: head, TreeSHA: tree, Passed: true, Commands: [][]string{{"true"}}}
		e := &Engine{}
		for _, test := range []struct {
			name   string
			mutate func(*marshal.Verification)
			want   StaleVerificationIdentityKind
		}{
			{name: "branch", mutate: func(value *marshal.Verification) { value.BranchSHA = "stale" }, want: StaleVerificationBranch},
			{name: "tree", mutate: func(value *marshal.Verification) { value.TreeSHA = "stale" }, want: StaleVerificationTree},
		} {
			t.Run(test.name, func(t *testing.T) {
				candidate := receipt
				test.mutate(&candidate)
				kind, _, stale := StaleVerificationIdentity(e.validateFinalIdentity(is, decision, candidate, nil))
				if !stale || kind != test.want {
					t.Fatalf("kind=%q stale=%v, want %q", kind, stale, test.want)
				}
			})
		}

		cacheRoot := filepath.Join(t.TempDir(), "cache")
		runtime, err := verificationcache.New(verificationcache.Config{CacheRoot: cacheRoot, RepoDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		lease, err := runtime.Acquire(context.Background(), verificationcache.Config{
			RepoDir: dir, BaseSHA: head, BranchSHA: head, TreeSHA: tree, Argv: []string{"true"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := lease.Seal(); err != nil {
			t.Fatal(err)
		}
		e = &Engine{cfg: Config{CacheRoot: cacheRoot, Train: &marshal.Train{Repo: dir, CacheRoot: cacheRoot, TestCmd: []string{"true"}}}}
		evidence, err := lease.Evidence()
		if err != nil {
			t.Fatal(err)
		}
		receipt.CacheEvidence = marshal.NewCacheEvidence(evidence)
		for _, test := range []struct {
			name   string
			mutate func(*marshal.CacheEvidence)
			want   StaleVerificationIdentityKind
		}{
			{name: "lease", mutate: func(value *marshal.CacheEvidence) { value.LeaseID = "stale-lease" }, want: StaleVerificationLease},
			{name: "cache", mutate: func(value *marshal.CacheEvidence) { value.ManagedScope = filepath.Join(cacheRoot, "stale") }, want: StaleVerificationCache},
		} {
			t.Run(test.name, func(t *testing.T) {
				candidate := receipt
				copyEvidence := *receipt.CacheEvidence
				test.mutate(&copyEvidence)
				candidate.CacheEvidence = &copyEvidence
				kind, _, stale := StaleVerificationIdentity(e.validateFinalIdentity(is, decision, candidate, lease))
				if !stale || kind != test.want {
					t.Fatalf("kind=%q stale=%v, want %q", kind, stale, test.want)
				}
			})
		}
		_ = lease.Close()
	})

	t.Run("GH-66 stale proof succeeds exactly once and duplicate delivery converges", func(t *testing.T) {
		fixture := newGH79Fixture(t, [][]string{{"true"}}, true, true)
		if err := fixture.e.RetryStage(context.Background(), fixture.id); err != nil {
			t.Fatal(err)
		}
		attempts, err := fixture.s.VerificationAttempts(fixture.id)
		if err != nil || len(attempts) != 2 {
			t.Fatalf("attempts = %+v err=%v", attempts, err)
		}
		if attempts[0].Status != store.VerificationAttemptQuarantined ||
			string(attempts[0].ReceiptJSON) != string(fixture.oldReceipt) ||
			attempts[1].Status != store.VerificationAttemptPassed {
			t.Fatalf("attempt history = %+v", attempts)
		}
		var fresh marshal.Verification
		if err := json.Unmarshal(attempts[1].ReceiptJSON, &fresh); err != nil {
			t.Fatal(err)
		}
		if fresh.BaseSHA == "" || fresh.BranchSHA != fixture.head || fresh.TreeSHA != fixture.tree ||
			fresh.CacheEvidence == nil || fresh.CacheEvidence.BranchSHA != fixture.head ||
			fresh.CacheEvidence.TreeSHA != fixture.tree {
			t.Fatalf("fresh receipt = %+v", fresh)
		}
		if got := gh79EventCount(t, fixture.s, fixture.id, core.EvMergeStarted); got != 2 {
			t.Fatalf("merge started events = %d, want one failed attempt and one successful retry", got)
		}
		if got := gh79EventCount(t, fixture.s, fixture.id, core.EvIssueMerged); got != 1 {
			t.Fatalf("merged events = %d", got)
		}
		if got := gh79EventCount(t, fixture.s, fixture.id, core.EvPublishSucceeded); got != 1 {
			t.Fatalf("publish events = %d", got)
		}
		integration, ok, err := fixture.s.IssueIntegration(fixture.id)
		if err != nil || !ok || integration.State != store.IntegrationMerged {
			t.Fatalf("integration = %+v ok=%v err=%v", integration, ok, err)
		}
		historyBefore, err := fixture.s.FailureHistory(context.Background(), fixture.id)
		if err != nil {
			t.Fatal(err)
		}
		duplicate, err := fixture.s.BeginVerificationRetry(context.Background(), store.VerificationRetry{
			IssueID: fixture.id, ParentID: fixture.parentID, RetryKey: fmt.Sprintf("stale-finalization:%d", fixture.parentID),
			Reason: "duplicate delivery", Failure: gh79FailureInput(fixture.id, fixture.parentID),
		})
		if err != nil {
			t.Fatal(err)
		}
		if duplicate.ID != attempts[1].ID {
			t.Fatalf("duplicate child = %d, want %d", duplicate.ID, attempts[1].ID)
		}
		attempts, err = fixture.s.VerificationAttempts(fixture.id)
		if err != nil || len(attempts) != 2 {
			t.Fatalf("duplicate attempts = %+v err=%v", attempts, err)
		}
		historyAfter, err := fixture.s.FailureHistory(context.Background(), fixture.id)
		if err != nil || len(historyAfter) != len(historyBefore) {
			t.Fatalf("duplicate failure history = before=%+v after=%+v err=%v", historyBefore, historyAfter, err)
		}
	})

	t.Run("matching proof and unrelated finalization keep existing retry behavior", func(t *testing.T) {
		t.Run("matching proof", func(t *testing.T) {
			matching := newGH79Fixture(t, [][]string{{"true"}}, false, false)
			before, err := matching.s.StageRuns(matching.id)
			if err != nil {
				t.Fatal(err)
			}
			if err := matching.e.RetryStage(context.Background(), matching.id); err != nil {
				t.Fatal(err)
			}
			after, err := matching.s.StageRuns(matching.id)
			if err != nil || len(after) != len(before) {
				t.Fatalf("matching retry reran a provider: before=%+v after=%+v err=%v", before, after, err)
			}
		})
		t.Run("unrelated finalization", func(t *testing.T) {
			unrelated := newGH79Fixture(t, [][]string{{"true"}}, false, false)
			if err := unrelated.e.RetryStage(context.Background(), unrelated.id); err != nil {
				t.Fatal(err)
			}
			attempts, err := unrelated.s.VerificationAttempts(unrelated.id)
			if err != nil || len(attempts) != 1 {
				t.Fatalf("unrelated attempts = %+v err=%v", attempts, err)
			}
		})
	})

	t.Run("stale proof without explicit retry remains failed closed", func(t *testing.T) {
		fixture := newGH79Fixture(t, [][]string{{"true"}}, true, false)
		attempts, err := fixture.s.VerificationAttempts(fixture.id)
		if err != nil || len(attempts) != 1 || attempts[0].Status != store.VerificationAttemptPassed {
			t.Fatalf("attempts without retry = %+v err=%v", attempts, err)
		}
		integration, ok, err := fixture.s.IssueIntegration(fixture.id)
		if err != nil || !ok || integration.State != store.IntegrationVerificationReady {
			t.Fatalf("integration without retry = %+v ok=%v err=%v", integration, ok, err)
		}
		if got := gh79EventCount(t, fixture.s, fixture.id, core.EvIssueMerged); got != 0 {
			t.Fatalf("unexpected merge without retry: %d", got)
		}
	})

	t.Run("restart before transition commit is safely reissued", func(t *testing.T) {
		fixture := newGH79Fixture(t, [][]string{{"true"}}, true, false)
		fixture.s.FailNextVerificationRetryForTest()
		if err := fixture.e.RetryStage(context.Background(), fixture.id); err == nil {
			t.Fatal("injected pre-commit retry unexpectedly succeeded")
		}
		attempts, err := fixture.s.VerificationAttempts(fixture.id)
		if err != nil || len(attempts) != 1 || attempts[0].Status != store.VerificationAttemptPassed {
			t.Fatalf("pre-commit attempts = %+v err=%v", attempts, err)
		}
		if err := fixture.e.RetryStage(context.Background(), fixture.id); err != nil {
			t.Fatal(err)
		}
		attempts, err = fixture.s.VerificationAttempts(fixture.id)
		if err != nil || len(attempts) != 2 {
			t.Fatalf("reissued attempts = %+v err=%v", attempts, err)
		}
	})

	t.Run("restart after transition commit resumes the child", func(t *testing.T) {
		fixture := newGH79Fixture(t, [][]string{{"true"}}, true, false)
		child, err := fixture.s.BeginVerificationRetry(context.Background(), store.VerificationRetry{
			IssueID: fixture.id, ParentID: fixture.parentID, RetryKey: "restart-after-commit",
			Reason: "stale tree identity", Failure: gh79FailureInput(fixture.id, fixture.parentID),
		})
		if err != nil {
			t.Fatal(err)
		}
		restarted := New(fixture.e.cfg)
		if err := restarted.Rehydrate(); err != nil {
			t.Fatal(err)
		}
		integration := waitForGH79Integration(t, fixture.s, fixture.id, store.IntegrationMerged)
		attempts, err := fixture.s.VerificationAttempts(fixture.id)
		if err != nil || len(attempts) != 2 || attempts[1].ID != child.ID || attempts[1].Status != store.VerificationAttemptPassed {
			t.Fatalf("post-restart attempts = %+v err=%v", attempts, err)
		}
		if integration.State != store.IntegrationMerged {
			t.Fatalf("post-restart integration = %+v", integration)
		}
	})

	t.Run("second reverification failure stays stopped", func(t *testing.T) {
		gate := filepath.Join(t.TempDir(), "gate.sh")
		marker := filepath.Join(t.TempDir(), "fail-gate")
		if err := os.WriteFile(gate, []byte("#!/bin/sh\nif [ -f \"$1\" ]; then exit 1; fi\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		fixture := newGH79Fixture(t, [][]string{{gate, marker}}, true, false)
		if err := os.WriteFile(marker, []byte("fail\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := fixture.e.RetryStage(context.Background(), fixture.id)
		if err == nil {
			t.Fatal("failed reverification unexpectedly succeeded")
		}
		attempts, err := fixture.s.VerificationAttempts(fixture.id)
		if err != nil || len(attempts) != 2 || attempts[1].Status != store.VerificationAttemptFailed {
			t.Fatalf("failed reverification attempts = %+v err=%v", attempts, err)
		}
		if strings.Contains(attempts[1].Reason, gate) || strings.Contains(attempts[1].Reason, marker) {
			t.Fatalf("failed reverification persisted raw diagnostic: %q", attempts[1].Reason)
		}
		integration, ok, err := fixture.s.IssueIntegration(fixture.id)
		if err != nil || !ok || integration.State != store.IntegrationReverificationFailed {
			t.Fatalf("failed reverification integration = %+v ok=%v err=%v", integration, ok, err)
		}
		attempts, err = fixture.s.VerificationAttempts(fixture.id)
		if err != nil || len(attempts) != 2 {
			t.Fatalf("automatic retry created an attempt: %+v err=%v", attempts, err)
		}
		if gh79EventCount(t, fixture.s, fixture.id, core.EvIssueMerged) != 0 {
			t.Fatal("failed reverification merged the issue")
		}
		if _, err := os.Stat(fixture.worktree); err != nil {
			t.Fatalf("failed reverification worktree was not preserved: %v", err)
		}
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
		restarted := New(fixture.e.cfg)
		if err := restarted.Rehydrate(); err != nil {
			t.Fatal(err)
		}
		if err := restarted.RetryStage(context.Background(), fixture.id); err != nil {
			t.Fatalf("retry failed reverification after restart: %v", err)
		}
		integration, ok, err = fixture.s.IssueIntegration(fixture.id)
		if err != nil || !ok || integration.State != store.IntegrationMerged {
			t.Fatalf("recovered reverification integration = %+v ok=%v err=%v", integration, ok, err)
		}
	})
}
