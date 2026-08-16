package engine

import (
	"context"
	"os"
	"os/exec"
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
	if err := e.writeVerificationReceipt(context.Background(), is, dir, nil); err != nil {
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

func TestWriteVerificationReceiptCacheIdentityMatchesCompleteEvidence(t *testing.T) {
	dir, head := initReceiptRepo(t)
	tree := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD^{tree}"))
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	runtime, err := verificationcache.New(verificationcache.Config{CacheRoot: cacheRoot, RepoDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	config := verificationcache.Config{
		RepoDir: dir, BaseSHA: head, BranchSHA: head, TreeSHA: tree, Argv: []string{"true"},
	}
	lease, err := runtime.Acquire(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{cfg: Config{CacheRoot: cacheRoot, Train: &marshal.Train{Repo: dir, TestCmd: []string{"true"}}}}
	is := &issueState{id: "GH-T", baseRef: head, wsPath: dir}
	if err := e.writeVerificationReceipt(context.Background(), is, dir, lease); err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	receipt, err := marshal.LoadVerification(filepath.Join(dir, "verification.json"))
	if err != nil {
		t.Fatal(err)
	}
	evidence := receipt.CacheEvidence
	if evidence == nil {
		t.Fatal("receipt is missing cache evidence")
	}
	if evidence.BaseSHA != head || evidence.BranchSHA != head || evidence.TreeSHA != tree ||
		evidence.Repository != lease.Repository() || evidence.LeaseID != lease.ID() ||
		evidence.ManagedScope != lease.ManagedScope() || evidence.SeedLeaseID != lease.SeedLeaseID() ||
		evidence.CommandDigest != verificationcache.CommandDigest(config.Argv) {
		t.Fatalf("receipt evidence = %+v, want complete evidence for the live lease", evidence)
	}
}

func TestWriteVerificationReceiptRejectsPreGateIdentityMismatch(t *testing.T) {
	dir, head := initReceiptRepo(t)
	marker := filepath.Join(t.TempDir(), "invoked")
	command := filepath.Join(t.TempDir(), "gate.sh")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf invoked > \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	tree := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD^{tree}"))
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	runtime, err := verificationcache.New(verificationcache.Config{CacheRoot: cacheRoot, RepoDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	config := verificationcache.Config{
		RepoDir: dir, BaseSHA: head, BranchSHA: "stale-branch", TreeSHA: tree,
		Argv: []string{command, marker},
	}
	lease, err := runtime.Acquire(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{cfg: Config{CacheRoot: cacheRoot, Train: &marshal.Train{Repo: dir, TestCmd: []string{command, marker}}}}
	is := &issueState{id: "GH-T", baseRef: head, wsPath: dir}
	if err := e.writeVerificationReceipt(context.Background(), is, dir, lease); err == nil {
		t.Fatal("pre-gate identity mismatch was accepted")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("configured gate ran despite pre-gate identity mismatch")
	}
	if lease.State() != verificationcache.StateQuarantined {
		t.Fatalf("lease state = %q, want quarantined", lease.State())
	}
	if entries, err := os.ReadDir(runtime.CompleteRoot()); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("complete cache entries = %d, want none", len(entries))
	}
	_ = lease.Close()
}

func TestWriteVerificationReceiptRejectsGateMutation(t *testing.T) {
	dir, head := initReceiptRepo(t)
	marker := filepath.Join(t.TempDir(), "invoked")
	command := filepath.Join(t.TempDir(), "gate.sh")
	script := "#!/bin/sh\nprintf invoked > \"$2\"\ngit -C \"$1\" commit --allow-empty -qm gate-mutation\n"
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	tree := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD^{tree}"))
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	runtime, err := verificationcache.New(verificationcache.Config{CacheRoot: cacheRoot, RepoDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	config := verificationcache.Config{
		RepoDir: dir, BaseSHA: head, BranchSHA: head, TreeSHA: tree,
		Argv: []string{command, dir, marker},
	}
	lease, err := runtime.Acquire(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{cfg: Config{CacheRoot: cacheRoot, Train: &marshal.Train{Repo: dir, TestCmd: []string{command, dir, marker}}}}
	is := &issueState{id: "GH-T", baseRef: head, wsPath: dir}
	if err := e.writeVerificationReceipt(context.Background(), is, dir, lease); err == nil {
		t.Fatal("gate-time repository mutation was accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "verification.json")); err == nil {
		t.Fatal("verification receipt was written after gate-time mutation")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("gate did not run: %v", err)
	}
	if lease.State() != verificationcache.StateQuarantined {
		t.Fatalf("lease state = %q, want quarantined", lease.State())
	}
	if entries, err := os.ReadDir(runtime.CompleteRoot()); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("complete cache entries = %d, want none", len(entries))
	}
	_ = lease.Close()
}

func TestVerificationCacheFinalizationUsesLiveIdentityAfterRestart(t *testing.T) {
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
	if out, err := exec.Command("git", "-C", dir, "commit", "--allow-empty", "-qm", "after-verification").CombinedOutput(); err != nil {
		t.Fatalf("mutate repository: %v: %s", err, out)
	}
	receipt := marshal.Verification{
		BaseSHA: head, BranchSHA: head, TreeSHA: tree, Passed: true, Commands: [][]string{{"true"}},
		CacheEvidence: marshal.NewCacheEvidence(evidence),
	}
	e := &Engine{cfg: Config{CacheRoot: cacheRoot, Train: &marshal.Train{Repo: dir, CacheRoot: cacheRoot, TestCmd: []string{"true"}}}}
	is := &issueState{id: "GH-T", baseRef: head, wsPath: dir}
	if err := e.validateFinalIdentity(is, marshal.MergeDecision{}, receipt, nil); err == nil {
		t.Fatal("stale receipt was accepted after restart")
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
	if err := e.validateFinalIdentity(is, decision, receipt, lease); err != nil {
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
			if err := e.validateFinalIdentity(is, decision, candidate, lease); err == nil {
				t.Fatal("altered cache evidence accepted")
			}
		})
	}
}

func TestVerificationCacheFinalizationRejectsEvidenceFromAnotherLease(t *testing.T) {
	dir, head := initReceiptRepo(t)
	tree := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD^{tree}"))
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	runtime, err := verificationcache.New(verificationcache.Config{CacheRoot: cacheRoot, RepoDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	config := verificationcache.Config{
		RepoDir: dir, BaseSHA: head, BranchSHA: head, TreeSHA: tree, Argv: []string{"true"},
	}
	currentLease, err := runtime.Acquire(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if err := currentLease.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := currentLease.Close(); err != nil {
		t.Fatal(err)
	}

	otherLease, err := runtime.Acquire(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if err := otherLease.Seal(); err != nil {
		t.Fatal(err)
	}
	otherEvidence, err := otherLease.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if err := otherLease.Close(); err != nil {
		t.Fatal(err)
	}
	receipt := marshal.Verification{
		BaseSHA: head, BranchSHA: head, TreeSHA: tree, Passed: true,
		Commands: [][]string{{"true"}}, CacheEvidence: &marshal.CacheEvidence{
			LeaseID: otherEvidence.LeaseID, State: string(otherEvidence.State), Repository: otherEvidence.Repository,
			ManagedScope: otherEvidence.ManagedScope, BaseSHA: otherEvidence.BaseSHA, BranchSHA: otherEvidence.BranchSHA,
			TreeSHA: otherEvidence.TreeSHA, CommandDigest: otherEvidence.CommandDigest, SeedLeaseID: otherEvidence.SeedLeaseID,
		},
	}
	e := &Engine{cfg: Config{
		CacheRoot: cacheRoot,
		Train:     &marshal.Train{Repo: dir, CacheRoot: cacheRoot, TestCmd: []string{"true"}},
	}}
	is := &issueState{id: "GH-T", baseRef: head, wsPath: dir}
	decision := marshal.MergeDecision{Decision: "merge", BranchCommit: head, BaseCommit: head}
	if err := e.validateFinalIdentity(is, decision, receipt, currentLease); err == nil {
		t.Fatal("valid evidence from a different lease was accepted")
	}
}

func TestFinalizationIdentityClassification(t *testing.T) {
	dir, head := initReceiptRepo(t)
	tree := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD^{tree}"))
	e := &Engine{}
	is := &issueState{id: "GH-79", baseRef: head, wsPath: dir}
	decision := marshal.MergeDecision{Decision: "merge", BranchCommit: head, BaseCommit: head}
	receipt := marshal.Verification{
		BaseSHA: head, BranchSHA: head, TreeSHA: tree, Passed: true,
		Commands: [][]string{{"true"}},
	}
	if err := e.validateFinalIdentity(is, decision, receipt, nil); err != nil {
		t.Fatalf("matching proof rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*marshal.Verification)
		want   StaleVerificationIdentityKind
	}{
		{name: "branch", mutate: func(value *marshal.Verification) { value.BranchSHA = "stale-branch" }, want: StaleVerificationBranch},
		{name: "tree", mutate: func(value *marshal.Verification) { value.TreeSHA = "stale-tree" }, want: StaleVerificationTree},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := receipt
			test.mutate(&candidate)
			err := e.validateFinalIdentity(is, decision, candidate, nil)
			kind, _, stale := StaleVerificationIdentity(err)
			if err == nil || !stale || kind != test.want {
				t.Fatalf("identity result = kind=%q stale=%v err=%v, want %q", kind, stale, err, test.want)
			}
		})
	}

	baseMismatch := receipt
	baseMismatch.BaseSHA = "stale-base"
	if err := e.validateFinalIdentity(is, decision, baseMismatch, nil); err == nil {
		t.Fatal("base mismatch was accepted")
	} else if _, _, stale := StaleVerificationIdentity(err); stale {
		t.Fatalf("base mismatch was classified as stale identity: %v", err)
	}
	decisionMismatch := decision
	decisionMismatch.BranchCommit = "stale-decision"
	if err := e.validateFinalIdentity(is, decisionMismatch, receipt, nil); err == nil {
		t.Fatal("merge decision mismatch was accepted")
	} else if kind, _, stale := StaleVerificationIdentity(err); !stale || kind != StaleVerificationBranch {
		t.Fatalf("merge decision mismatch result = kind=%q stale=%v err=%v, want stale branch identity", kind, stale, err)
	}
}

func TestFinalizationLegacyReceiptClassification(t *testing.T) {
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
	e := &Engine{cfg: Config{
		CacheRoot: cacheRoot,
		Train:     &marshal.Train{Repo: dir, CacheRoot: cacheRoot, TestCmd: []string{"true"}},
	}}
	is := &issueState{id: "GH-79", baseRef: head, wsPath: dir}
	decision := marshal.MergeDecision{Decision: "merge", BranchCommit: head, BaseCommit: head}
	legacy := marshal.Verification{
		BaseSHA: head, BranchSHA: head, TreeSHA: tree, Passed: true,
		Commands: [][]string{{"true"}},
	}
	if err := e.validateFinalIdentity(is, decision, legacy, lease); err == nil {
		t.Fatal("legacy cache-less receipt was accepted")
	} else if kind, _, stale := StaleVerificationIdentity(err); !stale || kind != StaleVerificationCache {
		t.Fatalf("legacy receipt result = kind=%q stale=%v err=%v", kind, stale, err)
	}
	evidence, err := lease.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	receipt := legacy
	receipt.CacheEvidence = marshal.NewCacheEvidence(evidence)
	if err := e.validateFinalIdentity(is, decision, receipt, lease); err != nil {
		t.Fatalf("matching cache proof rejected: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*marshal.CacheEvidence)
		want   StaleVerificationIdentityKind
	}{
		{name: "lease", mutate: func(value *marshal.CacheEvidence) { value.LeaseID = "other-lease" }, want: StaleVerificationLease},
		{name: "cache", mutate: func(value *marshal.CacheEvidence) { value.ManagedScope = filepath.Join(cacheRoot, "other") }, want: StaleVerificationCache},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := receipt
			copyEvidence := *receipt.CacheEvidence
			test.mutate(&copyEvidence)
			candidate.CacheEvidence = &copyEvidence
			err := e.validateFinalIdentity(is, decision, candidate, lease)
			kind, _, stale := StaleVerificationIdentity(err)
			if err == nil || !stale || kind != test.want {
				t.Fatalf("cache result = kind=%q stale=%v err=%v, want %q", kind, stale, err, test.want)
			}
		})
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWriteVerificationReceiptFailingCommand(t *testing.T) {
	dir, head := initReceiptRepo(t)
	e := &Engine{cfg: Config{Train: &marshal.Train{Repo: dir, TestCmd: []string{"false"}}}}
	is := &issueState{id: "GH-T", baseRef: head, wsPath: dir}
	if err := e.writeVerificationReceipt(context.Background(), is, dir, nil); err == nil {
		t.Fatal("expected error from failing test_cmd")
	}
}

func TestWriteVerificationReceiptRequiresTestCmd(t *testing.T) {
	dir, head := initReceiptRepo(t)
	e := &Engine{cfg: Config{}}
	is := &issueState{id: "GH-T", baseRef: head, wsPath: dir}
	if err := e.writeVerificationReceipt(context.Background(), is, dir, nil); err == nil {
		t.Fatal("missing test_cmd was accepted")
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
	if err := e.writeVerificationReceipt(context.Background(), is, dir, nil); err != nil {
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
