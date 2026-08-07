package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/verificationcache"
)

func initReceiptRepo(t *testing.T) (dir, headSHA string) {
	t.Helper()
	dir = t.TempDir()
	initGitRepo(t, dir)
	return dir, strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
}

func TestWriteVerificationReceipt(t *testing.T) {
	dir, head := initReceiptRepo(t)
	e := &Engine{cfg: Config{Train: &marshal.Train{Repo: dir, TestCmd: []string{"true"}}}}
	is := &issueState{id: "GH-T", baseRef: head, wsPath: dir}
	if err := e.writeVerificationReceipt(context.Background(), is, dir); err != nil {
		t.Fatalf("writeVerificationReceipt: %v", err)
	}
	receipt, err := marshal.LoadVerification(filepath.Join(dir, "verification.json"))
	if err != nil {
		t.Fatalf("load receipt: %v", err)
	}
	if receipt.BaseSHA != head || receipt.BranchSHA != head {
		t.Fatalf("receipt identity %s/%s, want %s", receipt.BaseSHA, receipt.BranchSHA, head)
	}
	if !receipt.Includes([]string{"true"}) {
		t.Fatalf("receipt commands %v missing configured test_cmd", receipt.Commands)
	}
}

func TestVerificationCacheWriteReceiptIncludesCompleteEvidence(t *testing.T) {
	dir, head := initReceiptRepo(t)
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	runtime, err := verificationcache.New(verificationcache.Config{CacheRoot: cacheRoot, RepoDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := runtime.Acquire(context.Background(), verificationcache.Config{
		RepoDir: dir, BaseSHA: head, BranchSHA: head,
		TreeSHA: strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD^{tree}")), Argv: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{cfg: Config{Train: &marshal.Train{Repo: dir, TestCmd: []string{"true"}}}}
	is := &issueState{id: "GH-T", baseRef: head, wsPath: dir}
	if err := e.writeVerificationReceipt(context.Background(), is, dir, lease); err != nil {
		t.Fatalf("writeVerificationReceipt: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	receipt, err := marshal.LoadVerification(filepath.Join(dir, "verification.json"))
	if err != nil {
		t.Fatalf("load receipt: %v", err)
	}
	if receipt.CacheEvidence == nil || receipt.CacheEvidence.State != string(verificationcache.StateComplete) {
		t.Fatalf("receipt cache evidence = %+v, want complete evidence", receipt.CacheEvidence)
	}
	if got, want := receipt.CacheEvidence.CommandDigest, verificationcache.CommandDigest([]string{"true"}); got != want {
		t.Fatalf("receipt command digest = %q, want %q", got, want)
	}
}

func TestVerificationCacheFinalizationRejectsAlteredLeaseAndScopeEvidence(t *testing.T) {
	dir, head := initReceiptRepo(t)
	tree := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD^{tree}"))
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
	evidence, err := lease.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	receipt := marshal.Verification{
		BaseSHA: head, BranchSHA: head, TreeSHA: tree, Passed: true,
		Commands: [][]string{{"true"}}, CacheEvidence: &marshal.CacheEvidence{
			LeaseID: evidence.LeaseID, State: string(evidence.State), Repository: evidence.Repository,
			ManagedScope: evidence.ManagedScope, BaseSHA: evidence.BaseSHA, BranchSHA: evidence.BranchSHA,
			TreeSHA: evidence.TreeSHA, CommandDigest: evidence.CommandDigest, SeedLeaseID: evidence.SeedLeaseID,
		},
	}
	e := &Engine{cfg: Config{
		CacheRoot: cacheRoot,
		Train:     &marshal.Train{Repo: dir, CacheRoot: cacheRoot, TestCmd: []string{"true"}},
	}}
	is := &issueState{id: "GH-T", baseRef: head, wsPath: dir}
	decision := marshal.MergeDecision{Decision: "merge", BranchCommit: head, BaseCommit: head}
	if err := e.validateFinalIdentity(is, decision, receipt); err != nil {
		t.Fatalf("matching cache evidence rejected: %v", err)
	}
	for name, mutate := range map[string]func(*marshal.CacheEvidence){
		"lease": func(value *marshal.CacheEvidence) { value.LeaseID = "other-lease" },
		"scope": func(value *marshal.CacheEvidence) { value.ManagedScope = filepath.Join(cacheRoot, "other") },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := receipt
			copyEvidence := *receipt.CacheEvidence
			mutate(&copyEvidence)
			candidate.CacheEvidence = &copyEvidence
			if err := e.validateFinalIdentity(is, decision, candidate); err == nil {
				t.Fatal("altered cache evidence accepted")
			}
		})
	}
}

func TestWriteVerificationReceiptFailingCommand(t *testing.T) {
	dir, head := initReceiptRepo(t)
	e := &Engine{cfg: Config{Train: &marshal.Train{Repo: dir, TestCmd: []string{"false"}}}}
	is := &issueState{id: "GH-T", baseRef: head, wsPath: dir}
	if err := e.writeVerificationReceipt(context.Background(), is, dir); err == nil {
		t.Fatal("expected error from failing test_cmd")
	}
}

func TestWriteVerificationReceiptNoTestCmd(t *testing.T) {
	dir, head := initReceiptRepo(t)
	e := &Engine{cfg: Config{}}
	is := &issueState{id: "GH-T", baseRef: head, wsPath: dir}
	if err := e.writeVerificationReceipt(context.Background(), is, dir); err != nil {
		t.Fatalf("expected nil for missing test_cmd, got %v", err)
	}
	if _, err := marshal.LoadVerification(filepath.Join(dir, "verification.json")); err == nil {
		t.Fatal("receipt must not be written without test_cmd")
	}
}

func TestMergeBarrierOverwritesAgentVerificationReceipt(t *testing.T) {
	e, s, _ := verificationEngine(t, "merge", [][]string{{"go", "test", "./..."}}, "")
	id, err := e.CreateIssue("wrong agent receipt", "", "default", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatalf("merge-barrier stage should land with an engine-authored receipt: %v", err)
	}
	if !hasEvent(t, s, id, core.EvIssueMerged) {
		t.Fatal("engine-authored receipt did not reach merge")
	}
}

func TestWriteVerificationReceiptOverwritesAgentReceipt(t *testing.T) {
	dir, head := initReceiptRepo(t)
	agentReceipt := `{"base_sha":"` + head + `","branch_sha":"` + head +
		`","tree_sha":"deadbeef","passed":true,"commands":[["go","test","./..."]]}`
	if err := os.WriteFile(filepath.Join(dir, "verification.json"), []byte(agentReceipt), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &Engine{cfg: Config{Train: &marshal.Train{Repo: dir, TestCmd: []string{"true"}}}}
	is := &issueState{id: "GH-T", baseRef: head, wsPath: dir}
	if err := e.writeVerificationReceipt(context.Background(), is, dir); err != nil {
		t.Fatal(err)
	}
	receipt, err := marshal.LoadVerification(filepath.Join(dir, "verification.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Includes([]string{"true"}) {
		t.Fatalf("agent receipt was not overwritten: %v", receipt.Commands)
	}
}
