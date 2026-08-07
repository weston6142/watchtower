package verificationcache

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCommandDigestPreservesExactArgv(t *testing.T) {
	first := CommandDigest([]string{"go", "test", "./...", "-run", "a b", "$(not-a-shell)", "quote'"})
	second := CommandDigest([]string{"go", "test", "./...", "-run", "a b", "$(not-a-shell)", "quote'"})
	changedOrder := CommandDigest([]string{"go", "test", "./...", "-run", "$(not-a-shell)", "a b", "quote'"})
	changedBoundary := CommandDigest([]string{"go", "test", "./...", "-run", "a", "b", "$(not-a-shell)", "quote'"})

	if first == "" || first != second {
		t.Fatalf("digest is not deterministic: %q %q", first, second)
	}
	if first == changedOrder || first == changedBoundary {
		t.Fatalf("digest discarded argv boundaries: %q %q %q", first, changedOrder, changedBoundary)
	}
	if len(first) != 64 {
		t.Fatalf("digest length = %d, want SHA-256 hex", len(first))
	}
}

func TestLeaseManagedEnvironmentAndCanonicalRepositoryIdentity(t *testing.T) {
	repo := initRepository(t)
	runtime := newTestRuntime(t, repo)

	lease, err := runtime.Acquire(context.Background(), Config{
		RepoDir:   repo,
		BaseSHA:   "base",
		BranchSHA: "branch",
		TreeSHA:   "tree",
		Argv:      []string{"go", "test", "./..."},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()

	if !filepath.IsAbs(lease.Repository()) || !strings.HasSuffix(lease.Repository(), ".git") {
		t.Fatalf("repository identity = %q, want absolute git common directory", lease.Repository())
	}
	env := lease.ManagedEnvironment()
	values := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || value == "" {
			t.Fatalf("invalid managed environment entry %q", entry)
		}
		if _, exists := values[key]; exists {
			t.Fatalf("duplicate managed key %q in %v", key, env)
		}
		values[key] = value
	}
	for _, key := range ManagedVariables() {
		if values[key] == "" || !strings.HasPrefix(values[key], lease.ActiveRoot()) {
			t.Fatalf("%s = %q is not lease-specific under %q", key, values[key], lease.ActiveRoot())
		}
	}
	if got := lease.ManagedScope(); got != lease.ActiveRoot() {
		t.Fatalf("managed scope = %q, want %q", got, lease.ActiveRoot())
	}
}

func TestLeaseSeedsCopyOnWriteAndPreservesCompleteSnapshot(t *testing.T) {
	repo := initRepository(t)
	runtime := newTestRuntime(t, repo)
	config := Config{RepoDir: repo, BaseSHA: "base", BranchSHA: "branch", TreeSHA: "tree", Argv: []string{"go", "test"}}

	first, err := runtime.Acquire(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	seedFile := filepath.Join(first.ActiveRoot(), "seed.txt")
	if err := os.MkdirAll(first.ActiveRoot(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seedFile, []byte("known-good"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.Seal(); err != nil {
		t.Fatal(err)
	}
	firstEvidence, err := first.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if firstEvidence.State != StateComplete {
		t.Fatalf("sealed state = %q", firstEvidence.State)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := runtime.Acquire(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.SeedLeaseID() != first.ID() {
		t.Fatalf("seed lease = %q, want %q", second.SeedLeaseID(), first.ID())
	}
	secondFile := filepath.Join(second.ActiveRoot(), "seed.txt")
	if got, err := os.ReadFile(secondFile); err != nil || string(got) != "known-good" {
		t.Fatalf("seed copy = %q, err = %v", got, err)
	}
	if err := os.WriteFile(secondFile, []byte("attempt mutation"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(first.CompleteRoot(), "seed.txt")); err != nil || string(got) != "known-good" {
		t.Fatalf("complete snapshot changed: %q, err = %v", got, err)
	}
}

func TestLeaseRefusesToStealLiveOwner(t *testing.T) {
	repo := initRepository(t)
	runtime := newTestRuntime(t, repo)
	config := Config{RepoDir: repo, BaseSHA: "base", BranchSHA: "branch", TreeSHA: "tree", Argv: []string{"go", "test"}}

	lease, err := runtime.Acquire(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()

	_, err = runtime.Acquire(context.Background(), config)
	if !errors.Is(err, ErrLeaseBusy) {
		t.Fatalf("second acquire error = %v, want ErrLeaseBusy", err)
	}
}

func TestInterruptedActiveLeaseIsQuarantinedBeforeRetry(t *testing.T) {
	repo := initRepository(t)
	runtime := newTestRuntime(t, repo)
	config := Config{RepoDir: repo, BaseSHA: "base", BranchSHA: "branch", TreeSHA: "tree", Argv: []string{"go", "test"}}

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
	quarantines := retry.Quarantines()
	if len(quarantines) != 1 || quarantines[0].LeaseID != interrupted.ID() {
		t.Fatalf("quarantine evidence = %+v, want interrupted lease %q", quarantines, interrupted.ID())
	}
	q := quarantines[0]
	if q.Reason == "" || q.Repository != retry.Repository() || q.CommandDigest != retry.CommandDigest() {
		t.Fatalf("incomplete quarantine evidence = %+v", q)
	}
	if _, err := os.Stat(filepath.Join(interrupted.ActiveRoot(), "partial.txt")); err != nil {
		t.Fatalf("quarantined active state was not retained: %v", err)
	}
}

func TestMalformedCompleteCandidateIsRetainedAndIneligible(t *testing.T) {
	repo := initRepository(t)
	runtime := newTestRuntime(t, repo)
	config := Config{RepoDir: repo, BaseSHA: "base", BranchSHA: "branch", TreeSHA: "tree", Argv: []string{"go", "test"}}

	candidate := filepath.Join(runtime.CompleteRoot(), "malformed")
	if err := os.MkdirAll(candidate, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "manifest.json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}

	lease, err := runtime.Acquire(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	quarantines := lease.Quarantines()
	if len(quarantines) != 1 || quarantines[0].Reason == "" {
		t.Fatalf("malformed candidate was not recorded: %+v", quarantines)
	}
	if _, err := os.Stat(filepath.Join(candidate, "manifest.json")); err != nil {
		t.Fatalf("malformed candidate was removed: %v", err)
	}
}

func newTestRuntime(t *testing.T, repo string) *Runtime {
	t.Helper()
	runtime, err := New(Config{CacheRoot: filepath.Join(t.TempDir(), "cache"), RepoDir: repo})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func initRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	command := exec.Command("git", "init", "-q", repo)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	return repo
}

func TestJSONEvidenceIsMachineReadable(t *testing.T) {
	value := Evidence{LeaseID: "lease", State: StateComplete, Repository: "/repo", CommandDigest: strings.Repeat("a", 64)}
	if _, err := json.Marshal(value); err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(value, Evidence{}) {
		t.Fatal("test evidence unexpectedly empty")
	}
}
