package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoverRejectedWorkspaceReconstructsTrustedBaseline(t *testing.T) {
	repo, trusted, trustedTree := recoveryRepository(t)
	provider := GitWorktree{Repo: repo}
	path, _, err := provider.Acquire("GH-68")
	if err != nil {
		t.Fatal(err)
	}
	writeRecoveryFile(t, path, "approved.txt", "rejected\n")
	writeRecoveryFile(t, path, "outside.txt", "must disappear\n")
	gitRecovery(t, path, "add", "approved.txt", "outside.txt")
	gitRecovery(t, path, "commit", "-m", "rejected bytes")
	observed := gitRecovery(t, path, "rev-parse", "HEAD")
	writeRecoveryFile(t, path, "indexed.txt", "rejected index\n")
	gitRecovery(t, path, "add", "indexed.txt")
	if err := os.Symlink("approved.txt", filepath.Join(path, "rejected-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(path, "approved.txt"), filepath.Join(path, "rejected-hardlink")); err != nil {
		t.Fatal(err)
	}

	recovered, release, err := provider.Recover(RecoveryRequest{
		IssueID: "GH-68", RejectedPath: path, Provider: provider.Name(), Repository: repo,
		Branch: "issue/GH-68", TrustedCommit: trusted, TrustedTree: trustedTree,
		ObservedCommit: observed, ObservedRef: observed, CapabilityAttemptID: "attempt-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if got := gitRecovery(t, recovered, "rev-parse", "HEAD"); got != trusted {
		t.Fatalf("recovered HEAD = %s, want %s", got, trusted)
	}
	if got := gitRecovery(t, recovered, "rev-parse", "HEAD^{tree}"); got != trustedTree {
		t.Fatalf("recovered tree = %s, want %s", got, trustedTree)
	}
	if got := gitRecovery(t, recovered, "status", "--porcelain"); got != "" {
		t.Fatalf("recovered workspace is dirty: %q", got)
	}
	for _, name := range []string{"outside.txt", "indexed.txt", "rejected-link", "rejected-hardlink"} {
		if _, err := os.Lstat(filepath.Join(recovered, name)); !os.IsNotExist(err) {
			t.Fatalf("rejected %s survived recovery: %v", name, err)
		}
	}
}

func TestRecoveryRefusesAmbiguousOrBroadTargets(t *testing.T) {
	repo, trusted, trustedTree := recoveryRepository(t)
	provider := GitWorktree{Repo: repo}
	path, release, err := provider.Acquire("GH-68")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	observed := gitRecovery(t, path, "rev-parse", "HEAD")
	base := RecoveryRequest{
		IssueID: "GH-68", RejectedPath: path, Provider: provider.Name(), Repository: repo,
		Branch: "issue/GH-68", TrustedCommit: trusted, TrustedTree: trustedTree,
		ObservedCommit: observed, ObservedRef: observed, CapabilityAttemptID: "attempt-1",
	}
	link := filepath.Join(t.TempDir(), "linked-worktree")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*RecoveryRequest)
	}{
		{"repository root", func(r *RecoveryRequest) { r.RejectedPath = repo }},
		{"wrong issue", func(r *RecoveryRequest) { r.IssueID = "GH-69" }},
		{"wrong branch", func(r *RecoveryRequest) { r.Branch = "issue/GH-69" }},
		{"changed ref", func(r *RecoveryRequest) { r.ObservedRef = strings.Repeat("a", 40) }},
		{"symlink target", func(r *RecoveryRequest) { r.RejectedPath = link }},
		{"missing baseline", func(r *RecoveryRequest) { r.TrustedCommit = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := base
			test.mutate(&request)
			if _, _, err := provider.Recover(request); err == nil {
				t.Fatal("ambiguous recovery target was accepted")
			}
			if got := gitRecovery(t, path, "rev-parse", "HEAD"); got != observed {
				t.Fatalf("ref changed after refused recovery: %s", got)
			}
		})
	}
}

func TestRecoveryIsReplaySafe(t *testing.T) {
	repo, trusted, trustedTree := recoveryRepository(t)
	provider := GitWorktree{Repo: repo}
	path, _, err := provider.Acquire("GH-68")
	if err != nil {
		t.Fatal(err)
	}
	writeRecoveryFile(t, path, "approved.txt", "rejected\n")
	gitRecovery(t, path, "add", "approved.txt")
	gitRecovery(t, path, "commit", "-m", "rejected")
	observed := gitRecovery(t, path, "rev-parse", "HEAD")
	request := RecoveryRequest{
		IssueID: "GH-68", RejectedPath: path, Provider: provider.Name(), Repository: repo,
		Branch: "issue/GH-68", TrustedCommit: trusted, TrustedTree: trustedTree,
		ObservedCommit: observed, ObservedRef: observed, CapabilityAttemptID: "attempt-1",
	}
	recovered, _, err := provider.Recover(request)
	if err != nil {
		t.Fatal(err)
	}
	request.RejectedPath = recovered
	replayed, release, err := provider.Recover(request)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if replayed != recovered || gitRecovery(t, replayed, "status", "--porcelain") != "" {
		t.Fatalf("replay returned %q for %q", replayed, recovered)
	}
}

func recoveryRepository(t *testing.T) (string, string, string) {
	t.Helper()
	repo := t.TempDir()
	gitRecovery(t, repo, "init", "-b", "main")
	gitRecovery(t, repo, "config", "user.email", "watchtower@example.invalid")
	gitRecovery(t, repo, "config", "user.name", "Watchtower Test")
	writeRecoveryFile(t, repo, "approved.txt", "trusted\n")
	gitRecovery(t, repo, "add", "approved.txt")
	gitRecovery(t, repo, "commit", "-m", "trusted")
	return repo, gitRecovery(t, repo, "rev-parse", "HEAD"), gitRecovery(t, repo, "rev-parse", "HEAD^{tree}")
}

func writeRecoveryFile(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitRecovery(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
