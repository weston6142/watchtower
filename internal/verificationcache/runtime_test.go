package verificationcache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

func TestSealRecordsCopiedSnapshotWhenActiveCacheChanges(t *testing.T) {
	repo := initRepository(t)
	runtime := newTestRuntime(t, repo)
	lease, err := runtime.Acquire(context.Background(), Config{
		RepoDir: repo, BaseSHA: "base", BranchSHA: "branch", TreeSHA: "tree", Argv: []string{"go", "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()

	lockPath := filepath.Join(lease.ActiveRoot(), "gomodcache", "cache", "download", "module.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("cache-data"), 64*1024)
	for i := 0; i < 48; i++ {
		path := filepath.Join(lease.ActiveRoot(), "zz-payload", fmt.Sprintf("%03d.bin", i))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		contents := [][]byte{[]byte("first"), []byte("second-value")}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				_ = os.WriteFile(lockPath, contents[i%len(contents)], 0o600)
			}
		}
	}()
	if err := lease.Seal(); err != nil {
		close(stop)
		<-stopped
		t.Fatal(err)
	}
	close(stop)
	<-stopped
	evidence, err := lease.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.ValidateEvidence(evidence); err != nil {
		t.Fatalf("sealed snapshot did not match its manifest: %v", err)
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

func TestRebindTransfersLockAndBindsFinalIdentity(t *testing.T) {
	repo := initRepository(t)
	runtime := newTestRuntime(t, repo)
	initial := Config{
		RepoDir: repo, BaseSHA: "base", BranchSHA: "before", TreeSHA: "tree-before",
		Argv: []string{"go", "test", "./..."},
	}
	lease, err := runtime.Acquire(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}

	rebinder, ok := any(lease).(interface {
		Rebind(Config) (*Lease, error)
	})
	if !ok {
		_ = lease.Close()
		t.Fatal("active lease does not expose the lock-preserving rebind behavior")
	}
	final := initial
	final.BranchSHA = "after-repair"
	final.TreeSHA = "tree-after-repair"
	rebound, err := rebinder.Rebind(final)
	if err != nil {
		_ = lease.Close()
		t.Fatal(err)
	}
	defer rebound.Close()

	if lease.ID() == rebound.ID() {
		t.Fatal("rebind reused the pre-repair lease identity")
	}
	if lease.State() != StateQuarantined {
		t.Fatalf("old lease state = %q, want quarantined", lease.State())
	}
	if rebound.State() != StateActive {
		t.Fatalf("replacement lease state = %q, want active", rebound.State())
	}
	if rebound.BaseSHA() != final.BaseSHA || rebound.BranchSHA() != final.BranchSHA || rebound.TreeSHA() != final.TreeSHA {
		t.Fatalf("replacement identity = %q/%q/%q, want %q/%q/%q", rebound.BaseSHA(), rebound.BranchSHA(), rebound.TreeSHA(), final.BaseSHA, final.BranchSHA, final.TreeSHA)
	}
	if rebound.Repository() != lease.Repository() || rebound.CommandDigest() != lease.CommandDigest() {
		t.Fatalf("replacement changed repository or command identity: repository=%q/%q command=%q/%q", rebound.Repository(), lease.Repository(), rebound.CommandDigest(), lease.CommandDigest())
	}
	if rebound.SeedLeaseID() != "no-seed" {
		t.Fatalf("replacement seed lease = %q, want no-seed", rebound.SeedLeaseID())
	}
	quarantines := rebound.Quarantines()
	if len(quarantines) != 1 || quarantines[0].LeaseID != lease.ID() || quarantines[0].BranchSHA != initial.BranchSHA {
		t.Fatalf("replacement quarantine history = %+v, want old identity %q", quarantines, lease.ID())
	}

	oldManifest, err := readManifest(filepath.Join(lease.activeDir, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	if oldManifest.State != StateQuarantined || oldManifest.BranchSHA != initial.BranchSHA || oldManifest.TreeSHA != initial.TreeSHA {
		t.Fatalf("old manifest = %+v, want retained quarantined pre-repair identity", oldManifest)
	}
	newManifest, err := readManifest(filepath.Join(rebound.activeDir, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	if newManifest.State != StateActive || newManifest.BranchSHA != final.BranchSHA || newManifest.TreeSHA != final.TreeSHA {
		t.Fatalf("new manifest = %+v, want active final identity", newManifest)
	}

	if _, err := runtime.Acquire(context.Background(), final); !errors.Is(err, ErrLeaseBusy) {
		t.Fatalf("concurrent acquire error = %v, want ErrLeaseBusy", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("closing inert old lease: %v", err)
	}
	if _, err := runtime.Acquire(context.Background(), final); !errors.Is(err, ErrLeaseBusy) {
		t.Fatalf("acquire after closing inert old lease = %v, want ErrLeaseBusy", err)
	}
	if err := lease.Seal(); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("sealing inert old lease = %v, want ErrLeaseInvalid", err)
	}
	if err := lease.Quarantine("late mutation"); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("quarantining inert old lease = %v, want ErrLeaseInvalid", err)
	}
	if _, err := lease.Evidence(); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("reading inert old lease evidence = %v, want ErrLeaseInvalid", err)
	}

	if err := rebound.Seal(); err != nil {
		t.Fatal(err)
	}
	evidence, err := rebound.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if evidence.BranchSHA != final.BranchSHA || evidence.TreeSHA != final.TreeSHA || evidence.LeaseID != rebound.ID() {
		t.Fatalf("replacement evidence = %+v, want final identity", evidence)
	}
	if err := runtime.ValidateEvidence(evidence); err != nil {
		t.Fatalf("replacement evidence did not validate: %v", err)
	}
}

func TestRebindUsesOnlyFinalObservedIdentity(t *testing.T) {
	repo := initRepository(t)
	runtime := newTestRuntime(t, repo)
	initial := Config{RepoDir: repo, BaseSHA: "base", BranchSHA: "before", TreeSHA: "tree-before", Argv: []string{"go", "test"}}
	lease, err := runtime.Acquire(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	rebinder, ok := any(lease).(interface {
		Rebind(Config) (*Lease, error)
	})
	if !ok {
		t.Fatal("active lease does not expose the lock-preserving rebind behavior")
	}
	final := initial
	final.BranchSHA = "commit-three"
	final.TreeSHA = "tree-three"
	rebound, err := rebinder.Rebind(final)
	if err != nil {
		t.Fatal(err)
	}
	defer rebound.Close()
	if rebound.BranchSHA() != "commit-three" || rebound.TreeSHA() != "tree-three" {
		t.Fatalf("rebound identity = %q/%q, want only final observed identity", rebound.BranchSHA(), rebound.TreeSHA())
	}
	if len(rebound.Quarantines()) != 1 || rebound.Quarantines()[0].BranchSHA != "before" {
		t.Fatalf("rebound quarantine history = %+v, want only pre-repair identity", rebound.Quarantines())
	}
}

func TestRebindInterruptedReplacementIsReconciled(t *testing.T) {
	repo := initRepository(t)
	runtime := newTestRuntime(t, repo)
	initial := Config{RepoDir: repo, BaseSHA: "base", BranchSHA: "before", TreeSHA: "tree-before", Argv: []string{"go", "test"}}
	lease, err := runtime.Acquire(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}
	rebinder, ok := any(lease).(interface {
		Rebind(Config) (*Lease, error)
	})
	if !ok {
		_ = lease.Close()
		t.Fatal("active lease does not expose the lock-preserving rebind behavior")
	}
	final := initial
	final.BranchSHA = "after-repair"
	final.TreeSHA = "tree-after-repair"
	rebound, err := rebinder.Rebind(final)
	if err != nil {
		_ = lease.Close()
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rebound.Close(); err != nil {
		t.Fatal(err)
	}

	retry, err := runtime.Acquire(context.Background(), final)
	if err != nil {
		t.Fatal(err)
	}
	defer retry.Close()
	quarantines := retry.Quarantines()
	if len(quarantines) != 1 || quarantines[0].LeaseID != rebound.ID() {
		t.Fatalf("interrupted replacement quarantine = %+v, want lease %q", quarantines, rebound.ID())
	}
	for _, path := range []string{lease.ActiveRoot(), rebound.ActiveRoot()} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retained cache path %q: %v", path, err)
		}
	}
}

func TestReleasedLeaseWithReusedOwnerPIDIsReconciled(t *testing.T) {
	repo := initRepository(t)
	runtime := newTestRuntime(t, repo)
	config := Config{RepoDir: repo, BaseSHA: "base", BranchSHA: "branch", TreeSHA: "tree", Argv: []string{"go", "test"}}

	interrupted, err := runtime.Acquire(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if err := interrupted.Close(); err != nil {
		t.Fatal(err)
	}

	owner := exec.Command("sleep", "60")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = owner.Process.Kill()
		_ = owner.Wait()
	}()
	manifestPath := filepath.Join(interrupted.activeDir, manifestName)
	manifest, err := readManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest.State = StateActive
	manifest.OwnerPID = owner.Process.Pid
	if err := writeJSONAtomic(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}

	retry, err := runtime.Acquire(context.Background(), config)
	if err != nil {
		t.Fatalf("stale owner PID blocked recovery: %v", err)
	}
	defer retry.Close()
	if len(retry.Quarantines()) != 1 || retry.Quarantines()[0].LeaseID != interrupted.ID() {
		t.Fatalf("recovery quarantine = %+v, want lease %q", retry.Quarantines(), interrupted.ID())
	}
}

func TestVerificationCacheHelperProcess(t *testing.T) {
	if os.Getenv("GH48_HELPER") != "1" {
		return
	}
	ready := os.NewFile(uintptr(3), "ready")
	release := os.NewFile(uintptr(4), "release")
	defer ready.Close()
	defer release.Close()
	runtime, err := New(Config{
		CacheRoot: os.Getenv("GH48_CACHE_ROOT"),
		RepoDir:   os.Getenv("GH48_REPO"),
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := runtime.Acquire(context.Background(), Config{
		RepoDir: os.Getenv("GH48_REPO"), BaseSHA: "base", BranchSHA: "branch", TreeSHA: "tree",
		Argv: []string{"go", "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lease.ActiveRoot(), "partial.txt"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ready.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(release, []byte{0}); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptedHelperProcessIsQuarantinedBeforeRetry(t *testing.T) {
	repo := initRepository(t)
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	releaseReader, releaseWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyReader.Close()
	defer readyWriter.Close()
	defer releaseReader.Close()
	defer releaseWriter.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestVerificationCacheHelperProcess$", "-test.v")
	cmd.Env = append(os.Environ(), "GH48_HELPER=1", "GH48_CACHE_ROOT="+cacheRoot, "GH48_REPO="+repo)
	cmd.ExtraFiles = []*os.File{readyWriter, releaseReader}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(readyReader, []byte{0}); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("helper did not publish partial state: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("interrupted helper exited successfully")
	}

	runtime, err := New(Config{CacheRoot: cacheRoot, RepoDir: repo})
	if err != nil {
		t.Fatal(err)
	}
	retry, err := runtime.Acquire(context.Background(), Config{
		RepoDir: repo, BaseSHA: "base", BranchSHA: "branch", TreeSHA: "tree", Argv: []string{"go", "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer retry.Close()
	if len(retry.Quarantines()) != 1 || retry.Quarantines()[0].Reason != "interrupted active lease" {
		t.Fatalf("helper interruption quarantine = %+v", retry.Quarantines())
	}
	if _, err := os.Stat(filepath.Join(runtime.ActiveRoot(), retry.Quarantines()[0].LeaseID, "cache", "partial.txt")); err != nil {
		t.Fatalf("interrupted partial state was not retained: %v", err)
	}
	activeEntries, err := os.ReadDir(runtime.ActiveRoot())
	if err != nil {
		t.Fatal(err)
	}
	if len(activeEntries) == 0 {
		t.Fatal("interrupted active state was not retained for diagnosis")
	}
}

func TestCompleteCandidateWithMismatchedManagedScopeIsIneligible(t *testing.T) {
	repo := initRepository(t)
	runtime := newTestRuntime(t, repo)
	config := Config{RepoDir: repo, BaseSHA: "base", BranchSHA: "branch", TreeSHA: "tree", Argv: []string{"go", "test"}}

	complete, err := runtime.Acquire(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(complete.ActiveRoot(), "seed.txt"), []byte("known-good"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := complete.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := complete.Close(); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(filepath.Dir(complete.CompleteRoot()), manifestName)
	manifest, err := readManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest.ManagedScope = filepath.Join(runtime.repoRoot, "other-scope")
	if err := writeJSONAtomic(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}

	retry, err := runtime.Acquire(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer retry.Close()
	if retry.SeedLeaseID() != "no-seed" {
		t.Fatalf("mismatched managed scope was selected: %q", retry.SeedLeaseID())
	}
	if len(retry.Quarantines()) != 1 || !strings.Contains(retry.Quarantines()[0].Reason, "scope") {
		t.Fatalf("scope quarantine = %+v", retry.Quarantines())
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
