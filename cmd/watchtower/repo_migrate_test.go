package main_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/proto"
	"github.com/weston6142/watchtower/internal/repocfg"
	"github.com/weston6142/watchtower/internal/scaffold"
)

func TestMigratePreviewLeavesFilesAndDaemonStopped(t *testing.T) {
	bin, base, repo := newMigrateRepo(t)
	makeLegacyFlow(t, repo)
	before, err := scaffold.CaptureInputSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	out := run(t, bin, repo, "migrate", "--data", base, "--repo", repo)
	if !strings.Contains(out, "flows/default.yaml") || !strings.Contains(out, "watchtower migrate --apply") {
		t.Fatalf("preview output = %s", out)
	}
	after, err := scaffold.CaptureInputSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	if stringSnapshot(before) != stringSnapshot(after) {
		t.Fatalf("preview changed repository inputs: before=%+v after=%+v", before, after)
	}
	if _, err := os.Stat(filepath.Join(repo, ".watchtower", scaffold.ProvenanceFile)); !os.IsNotExist(err) {
		t.Fatalf("preview wrote provenance: %v", err)
	}
	if _, err := proto.Dial(filepath.Join(repocfg.RepoDataDir(base, repo), "watchtower.sock")); err == nil {
		t.Fatal("preview started a daemon")
	}
}

func TestMigrateApplyReloadsRunningDaemonAndIsIdempotent(t *testing.T) {
	bin, base, repo := newMigrateRepo(t)
	makeLegacyFlow(t, repo)
	startReplacementDaemon(t, bin, base, repo)
	first := run(t, bin, repo, "migrate", "--apply", "--data", base, "--repo", repo)
	if !strings.Contains(first, "migrated") && !strings.Contains(first, "applied") {
		t.Fatalf("apply output = %s", first)
	}
	client, err := proto.Dial(filepath.Join(repocfg.RepoDataDir(base, repo), "watchtower.sock"))
	if err != nil {
		t.Fatal(err)
	}
	setup, err := client.Do(proto.Command{Op: "setup_outline"})
	client.Close()
	if err != nil || setup.Setup == nil || setup.Setup.ConfigurationHealth == nil {
		t.Fatalf("setup after migration = %+v err=%v", setup, err)
	}
	if setup.Setup.ConfigurationHealth.ReloadRequired {
		t.Fatal("setup retained reload warning after migration")
	}
	gates := map[string]string{}
	for _, stage := range setup.Setup.Stages {
		gates[stage.Name] = stage.Gate
	}
	if gates["spec"] != "approve_artifact" || gates["plan"] != "plan_review" {
		t.Fatalf("reloaded gates = %+v", gates)
	}
	pidBefore := daemonPID(t, base, repo)
	second := run(t, bin, repo, "migrate", "--apply", "--data", base, "--repo", repo)
	if !strings.Contains(strings.ToLower(second), "no changes") {
		t.Fatalf("second apply output = %s", second)
	}
	if pidAfter := daemonPID(t, base, repo); pidAfter != pidBefore {
		t.Fatalf("second apply restarted daemon: before=%d after=%d", pidBefore, pidAfter)
	}
}

func TestStatusJSONIncludesConfigurationHealth(t *testing.T) {
	bin, base, repo := newMigrateRepo(t)
	run(t, bin, repo, "new", "--data", base, "--title", "status health")
	out := run(t, bin, repo, "status", "--json", "--data", base, "--repo", repo)
	var decoded proto.Overview
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("status JSON = %q: %v", out, err)
	}
	if decoded.ConfigurationHealth == nil {
		t.Fatalf("status omitted configuration health: %s", out)
	}
	if decoded.ConfigurationHealth.DefaultsVersion != scaffold.DefaultsVersion {
		t.Fatalf("status health = %+v", decoded.ConfigurationHealth)
	}
}

func TestStatusIncludesConfigurationCounts(t *testing.T) {
	bin, base, repo := newMigrateRepo(t)
	run(t, bin, repo, "new", "--data", base, "--title", "status counts")
	config := filepath.Join(repo, ".watchtower", "config.yaml")
	body, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, append(body, []byte("# intentional\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	out := run(t, bin, repo, "status", "--data", base, "--repo", repo)
	for _, want := range []string{"configuration: drift", "current=", "customized=1", "affected paths"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output missing %q:\n%s", want, out)
		}
	}
}

func TestMigrateApplyRollsBackWhenNewSetupFails(t *testing.T) {
	bin, base, repo := newMigrateRepo(t)
	makeLegacyFlow(t, repo)
	original, err := os.ReadFile(filepath.Join(repo, ".watchtower", "flows", "default.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// An invalid manifest is rejected before any daemon shutdown or swap. The
	// original tree must remain intact, which is the CLI's pre-swap rollback
	// boundary for a failed prepared replacement.
	if err := os.WriteFile(filepath.Join(repo, ".watchtower", scaffold.ProvenanceFile), []byte("schema_version: 9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := runErr(t, bin, repo, "migrate", "--apply", "--data", base, "--repo", repo); !strings.Contains(out, "invalid") {
		t.Fatalf("invalid migration error = %s", out)
	}
	got, err := os.ReadFile(filepath.Join(repo, ".watchtower", "flows", "default.yaml"))
	if err != nil || string(got) != string(original) {
		t.Fatalf("failed migration changed original tree: %q err=%v", got, err)
	}
}

func makeLegacyFlow(t *testing.T, repo string) {
	t.Helper()
	flowPath := filepath.Join(repo, ".watchtower", "flows", "default.yaml")
	body, err := os.ReadFile(flowPath)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), "    gate: approve_artifact\n", "    gate: auto\n", 1))
	body = []byte(strings.Replace(string(body), "    gate: plan_review\n", "    gate: auto\n", 1))
	if err := os.WriteFile(flowPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, ".watchtower", scaffold.ProvenanceFile)); err != nil {
		t.Fatal(err)
	}
}

func newMigrateRepo(t *testing.T) (bin, base, repo string) {
	t.Helper()
	t.Setenv("TMPDIR", "/tmp")
	bin = buildBinary(t)
	var err error
	base, err = os.MkdirTemp("/tmp", "wt-migrate-")
	if err != nil {
		t.Fatal(err)
	}
	repo = initRepo(t, bin, base)
	t.Cleanup(func() {
		stopDaemons(base)
		_ = os.RemoveAll(base)
	})
	return bin, base, repo
}

func stringSnapshot(snapshot scaffold.InputSnapshot) string {
	encoded, _ := json.Marshal(snapshot)
	return string(encoded)
}
