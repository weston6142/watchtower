package engine

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/weston6142/watchtower/internal/marshal"
)

func initReceiptRepo(t *testing.T) (dir, headSHA string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	run("init", "-q")
	run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "base")
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return dir, string(out[:40])
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
