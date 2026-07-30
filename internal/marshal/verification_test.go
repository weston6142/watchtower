package marshal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
