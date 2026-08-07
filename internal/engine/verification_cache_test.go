package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/verificationcache"
)

func TestVerificationCacheHarnessQuarantinesInterruptionAndPreservesCompleteSnapshot(t *testing.T) {
	repo, head := initReceiptRepo(t)
	tree := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD^{tree}"))
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	runtime, err := verificationcache.New(verificationcache.Config{CacheRoot: cacheRoot, RepoDir: repo})
	if err != nil {
		t.Fatal(err)
	}
	config := verificationcache.Config{
		RepoDir: repo, BaseSHA: head, BranchSHA: head, TreeSHA: tree, Argv: []string{"go", "test", "./..."},
	}
	complete, err := runtime.Acquire(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	knownGood := filepath.Join(complete.ActiveRoot(), "known-good.txt")
	if err := os.WriteFile(knownGood, []byte("known-good"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := complete.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := complete.Close(); err != nil {
		t.Fatal(err)
	}
	completeBytes, err := os.ReadFile(filepath.Join(complete.CompleteRoot(), "known-good.txt"))
	if err != nil {
		t.Fatal(err)
	}

	interrupted, err := runtime.Acquire(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(interrupted.ActiveRoot(), "partial.txt"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := interrupted.Close(); err != nil {
		t.Fatal(err)
	}

	retry, err := runtime.Acquire(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer retry.Close()
	if retry.SeedLeaseID() != complete.ID() {
		t.Fatalf("retry seed = %q, want preserved complete lease %q", retry.SeedLeaseID(), complete.ID())
	}
	var found bool
	for _, disposition := range retry.Quarantines() {
		if disposition.LeaseID == interrupted.ID() && strings.Contains(disposition.Reason, "interrupted") {
			found = true
		}
	}
	if !found {
		t.Fatalf("retry quarantine evidence = %+v, want interrupted lease", retry.Quarantines())
	}
	if got, err := os.ReadFile(filepath.Join(complete.CompleteRoot(), "known-good.txt")); err != nil || string(got) != string(completeBytes) {
		t.Fatalf("complete snapshot changed after retry: %q, %v", got, err)
	}
}

func TestVerificationCacheHarnessRejectsCorruptCompleteSnapshotWithoutDeletingIt(t *testing.T) {
	repo, head := initReceiptRepo(t)
	tree := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD^{tree}"))
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	runtime, err := verificationcache.New(verificationcache.Config{CacheRoot: cacheRoot, RepoDir: repo})
	if err != nil {
		t.Fatal(err)
	}
	config := verificationcache.Config{
		RepoDir: repo, BaseSHA: head, BranchSHA: head, TreeSHA: tree, Argv: []string{"go", "test"},
	}
	complete, err := runtime.Acquire(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(complete.ActiveRoot(), "known-good.txt")
	if err := os.WriteFile(path, []byte("known-good"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := complete.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := complete.Close(); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(complete.CompleteRoot(), "known-good.txt")
	if err := os.WriteFile(corrupt, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	retry, err := runtime.Acquire(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer retry.Close()
	if retry.SeedLeaseID() != "no-seed" {
		t.Fatalf("corrupt candidate was selected as seed: %q", retry.SeedLeaseID())
	}
	if len(retry.Quarantines()) == 0 || !strings.Contains(retry.Quarantines()[0].Reason, "integrity") {
		t.Fatalf("corruption disposition = %+v", retry.Quarantines())
	}
	if got, err := os.ReadFile(corrupt); err != nil || string(got) != "corrupt" {
		t.Fatalf("corrupt candidate was removed or repaired: %q, %v", got, err)
	}
}

func TestVerificationCacheHarnessDoesNotStealLiveLease(t *testing.T) {
	repo, head := initReceiptRepo(t)
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	runtime, err := verificationcache.New(verificationcache.Config{CacheRoot: cacheRoot, RepoDir: repo})
	if err != nil {
		t.Fatal(err)
	}
	config := verificationcache.Config{RepoDir: repo, BaseSHA: head, BranchSHA: head, TreeSHA: "tree", Argv: []string{"true"}}
	lease, err := runtime.Acquire(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if _, err := runtime.Acquire(context.Background(), config); !errors.Is(err, verificationcache.ErrLeaseBusy) {
		t.Fatalf("second live acquire error = %v, want ErrLeaseBusy", err)
	}
}

func TestVerificationCacheHarnessFailingReplayQuarantinesWithoutReadiness(t *testing.T) {
	e, s, _ := verificationEngine(t, "merge", [][]string{{"true"}}, "")
	cacheRoot := t.TempDir()
	e.cfg.CacheRoot = cacheRoot
	e.cfg.Train.CacheRoot = cacheRoot
	e.cfg.Train.TestCmd = []string{"false"}
	id, err := e.CreateIssue("failing cache replay", "", "default", nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err == nil {
		t.Fatal("failing cache-managed replay unexpectedly succeeded")
	}
	if hasEvent(t, s, id, core.EvVerificationReady) {
		t.Fatal("failing cache-managed replay created verification_ready")
	}
	quarantines, err := filepath.Glob(filepath.Join(cacheRoot, "verification-cache", "*", "quarantine", "*.json"))
	if err != nil || len(quarantines) == 0 {
		t.Fatalf("quarantine records = %v, err=%v", quarantines, err)
	}
	if _, err := os.Stat(filepath.Join(e.issueDir(id), "artifacts", "verification.json")); !os.IsNotExist(err) {
		t.Fatalf("failing replay archived a successful receipt: %v", err)
	}
}
