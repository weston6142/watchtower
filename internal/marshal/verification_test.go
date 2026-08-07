package marshal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/verificationcache"
)

func TestLoadVerificationRejectsMalformedFailedAndEmptyCommands(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "verification.json")
	cases := []string{
		`not json`,
		`{"base_sha":"b","branch_sha":"h","tree_sha":"t","passed":false,"commands":[["go","test"]]}`,
		`{"base_sha":"b","branch_sha":"h","tree_sha":"t","passed":true,"commands":[]}`,
		`{"base_sha":"b","branch_sha":"h","tree_sha":"t","passed":true,"commands":[[]]}`,
		`{"base_sha":"b","branch_sha":"h","tree_sha":"t","passed":true,"commands":[["","test"]]}`,
	}
	for _, body := range cases {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadVerification(path); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestVerificationAppliesToTreeAndIncludesConfiguredGate(t *testing.T) {
	receipt := Verification{
		BaseSHA: "base", BranchSHA: "branch", TreeSHA: "tree", Passed: true,
		Commands: [][]string{{"go", "test", "./..."}, {"go", "vet", "./..."}},
	}
	if !receipt.AppliesTo("tree") || receipt.AppliesTo("different") {
		t.Fatalf("AppliesTo returned wrong result")
	}
	if !receipt.Includes([]string{"go", "test", "./..."}) ||
		receipt.Includes([]string{"go", "test", "./pkg"}) {
		t.Fatalf("Includes returned wrong result")
	}
	path := filepath.Join(t.TempDir(), "verification.json")
	body, _ := json.Marshal(receipt)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadVerification(path)
	if err != nil || !reflect.DeepEqual(loaded, receipt) {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
}

func TestReplayPassesArgumentsWithoutShell(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "args.txt")
	script := filepath.Join(dir, "record.sh")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + output + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Replay(context.Background(), dir, [][]string{
		{script, "hello world", "; touch escaped"},
	}); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(args) != "hello world\n; touch escaped\n" {
		t.Fatalf("argv changed: %q", args)
	}
	if _, err := os.Stat(filepath.Join(dir, "escaped")); !os.IsNotExist(err) {
		t.Fatalf("shell interpreted argument: %v", err)
	}
}

func TestReplayReportsFailingArgv(t *testing.T) {
	err := Replay(context.Background(), t.TempDir(), [][]string{{"false"}})
	if err == nil || !strings.Contains(err.Error(), "false") {
		t.Fatalf("Replay error = %v", err)
	}
}

func TestVerificationCacheLoadStrictlyValidatesEvidence(t *testing.T) {
	evidence := CacheEvidence{
		LeaseID: "lease-1", State: "complete", Repository: "/repo/.git",
		ManagedScope: "/cache/lease-1", BaseSHA: "base", BranchSHA: "branch", TreeSHA: "tree",
		CommandDigest: verificationcache.CommandDigest([]string{"go", "test", "a b"}), SeedLeaseID: "no-seed",
	}
	receipt := Verification{
		BaseSHA: "base", BranchSHA: "branch", TreeSHA: "tree", Passed: true,
		Commands: [][]string{{"go", "test", "a b"}}, CacheEvidence: &evidence,
	}
	validPath := filepath.Join(t.TempDir(), "valid.json")
	body, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(validPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if loaded, err := LoadVerification(validPath); err != nil || loaded.CacheEvidence == nil {
		t.Fatalf("valid cache evidence rejected: %+v %v", loaded, err)
	}

	for name, mutate := range map[string]func(*CacheEvidence){
		"missing lease": func(value *CacheEvidence) { value.LeaseID = "" },
		"active state":  func(value *CacheEvidence) { value.State = "active" },
		"missing scope": func(value *CacheEvidence) { value.ManagedScope = "" },
		"bad digest":    func(value *CacheEvidence) { value.CommandDigest = "not-a-digest" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := receipt
			candidate.CacheEvidence = &evidence
			mutate(candidate.CacheEvidence)
			path := filepath.Join(t.TempDir(), "verification.json")
			body, _ := json.Marshal(candidate)
			if err := os.WriteFile(path, body, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadVerification(path); err == nil {
				t.Fatal("invalid cache evidence accepted")
			}
		})
	}

	unknown := strings.Replace(string(body), `"seed_lease_id":"no-seed"`, `"seed_lease_id":"no-seed","unexpected":true`, 1)
	unknownPath := filepath.Join(t.TempDir(), "unknown.json")
	if err := os.WriteFile(unknownPath, []byte(unknown), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadVerification(unknownPath); err == nil {
		t.Fatal("unknown nested cache evidence field accepted")
	}
	trailingPath := filepath.Join(t.TempDir(), "trailing.json")
	if err := os.WriteFile(trailingPath, append(body, []byte("\n{}")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadVerification(trailingPath); err == nil {
		t.Fatal("trailing JSON accepted")
	}
}

func TestVerificationCacheRejectsEveryCurrentIdentityMismatch(t *testing.T) {
	evidence := CacheEvidence{
		LeaseID: "lease-1", State: "complete", Repository: "/repo/.git",
		ManagedScope: "/cache/lease-1", BaseSHA: "base", BranchSHA: "branch", TreeSHA: "tree",
		CommandDigest: verificationcache.CommandDigest([]string{"go", "test"}), SeedLeaseID: "no-seed",
	}
	want := CacheIdentity{
		LeaseID: "lease-1", Repository: "/repo/.git", ManagedScope: "/cache/lease-1",
		BaseSHA: "base", BranchSHA: "branch", TreeSHA: "tree", CommandDigest: evidence.CommandDigest,
	}
	if err := evidence.ValidateAgainst(want); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*CacheIdentity){
		"repository":  func(value *CacheIdentity) { value.Repository = "/other/.git" },
		"base":        func(value *CacheIdentity) { value.BaseSHA = "other" },
		"branch":      func(value *CacheIdentity) { value.BranchSHA = "other" },
		"tree":        func(value *CacheIdentity) { value.TreeSHA = "other" },
		"lease":       func(value *CacheIdentity) { value.LeaseID = "other" },
		"scope":       func(value *CacheIdentity) { value.ManagedScope = "/other" },
		"argv digest": func(value *CacheIdentity) { value.CommandDigest = strings.Repeat("f", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := want
			mutate(&candidate)
			if err := evidence.ValidateAgainst(candidate); err == nil {
				t.Fatal("identity mismatch accepted")
			}
		})
	}
}

func TestVerificationCacheReplayWithEnvironmentReplacesOnlyManagedKeys(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "record.sh")
	output := filepath.Join(dir, "output")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$GOCACHE\" \"$GOMODCACHE\" \"$SENTINEL\" \"$1\" > \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("GOCACHE", "inherited-cache"); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv("GOCACHE")
	if err := os.Setenv("GOMODCACHE", "inherited-mod"); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv("GOMODCACHE")
	if err := os.Setenv("SENTINEL", "keep-me"); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv("SENTINEL")
	if err := ReplayWithEnvironment(context.Background(), dir, [][]string{{script, "argument;still-argv", output}}, []string{"GOCACHE=lease-cache", "GOMODCACHE=lease-mod"}); err != nil {
		t.Fatal(err)
	}
	lines, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(lines), "lease-cache\nlease-mod\nkeep-me\nargument;still-argv\n"; got != want {
		t.Fatalf("child environment/argv = %q, want %q", got, want)
	}
}
