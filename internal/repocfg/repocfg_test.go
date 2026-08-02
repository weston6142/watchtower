package repocfg

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	root := t.TempDir()
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runner != "codex" || cfg.Slots != 4 || cfg.ClaudeBin != "claude" ||
		cfg.CodexBin != "codex" || cfg.CodexModel != "gpt-5.6-luna" ||
		cfg.CodexEffort != "xhigh" {
		t.Fatalf("bad defaults: %+v", cfg)
	}
	if cfg.Flows != filepath.Join(root, ".watchtower", "flows") {
		t.Fatalf("flows not resolved: %s", cfg.Flows)
	}
	if cfg.Packages != filepath.Join(root, ".watchtower", "packages") {
		t.Fatalf("packages not resolved: %s", cfg.Packages)
	}
}

func TestLoadReadsFileAndFillsGaps(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".watchtower")
	os.MkdirAll(dir, 0o755)
	yaml := "runner: fake\nslots: 2\nflows: myflows\ntest_cmd: \"go test ./...\"\n"
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o644)
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runner != "fake" || cfg.Slots != 2 || cfg.TestCmd != "go test ./..." {
		t.Fatalf("file values not applied: %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.TestArgv, []string{"go", "test", "./..."}) {
		t.Fatalf("TestArgv = %#v", cfg.TestArgv)
	}
	if cfg.Flows != filepath.Join(root, "myflows") {
		t.Fatalf("relative flows not resolved: %s", cfg.Flows)
	}
	if cfg.ClaudeBin != "claude" { // gap filled from defaults
		t.Fatalf("gap not filled: %+v", cfg)
	}
	if cfg.CodexBin != "codex" || cfg.CodexModel != "gpt-5.6-luna" || cfg.CodexEffort != "xhigh" {
		t.Fatalf("codex gaps not filled: %+v", cfg)
	}
}

func TestLoadPreservesExplicitClaudeAndCodexOverrides(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".watchtower")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "runner: claude\ncodex_bin: /opt/codex\ncodex_model: custom-model\ncodex_effort: high\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runner != "claude" || cfg.CodexBin != "/opt/codex" ||
		cfg.CodexModel != "custom-model" || cfg.CodexEffort != "high" {
		t.Fatalf("explicit values lost: %+v", cfg)
	}
}

func TestLoadParsesQuotedTestCommandOnce(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".watchtower")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "config.yaml"),
		[]byte(`test_cmd: 'go test "./pkg with space"'`+"\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.TestArgv, []string{"go", "test", "./pkg with space"}) {
		t.Fatalf("TestArgv = %#v", cfg.TestArgv)
	}
}

func TestLoadRejectsMalformedTestCommand(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".watchtower")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "config.yaml"), []byte("test_cmd: 'go test \"unterminated'\n"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil {
		t.Fatal("malformed test_cmd accepted")
	}
}

func TestLoadTheme(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".watchtower"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".watchtower", "config.yaml"), []byte("theme: gruvbox\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Theme != "gruvbox" {
		t.Fatalf("Theme = %q", cfg.Theme)
	}
	// missing file: Theme stays empty (tui applies its own default)
	cfg, err = Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Theme != "" {
		t.Fatalf("default Theme = %q, want empty", cfg.Theme)
	}
}

func TestLoadBadYAMLErrors(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".watchtower")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("slots: [not an int"), 0o644)
	if _, err := Load(root); err == nil {
		t.Fatal("expected error for bad yaml")
	}
}

func TestFindRepoWalksUp(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".watchtower"), 0o755)
	nested := filepath.Join(root, "a", "b")
	os.MkdirAll(nested, 0o755)
	got, err := FindRepo(nested)
	if err != nil {
		t.Fatal(err)
	}
	// t.TempDir may be a symlink on macOS; compare resolved paths.
	wantR, _ := filepath.EvalSymlinks(root)
	gotR, _ := filepath.EvalSymlinks(got)
	if gotR != wantR {
		t.Fatalf("got %s want %s", got, root)
	}
}

func TestFindRepoNotFound(t *testing.T) {
	if _, err := FindRepo(t.TempDir()); err == nil {
		t.Fatal("expected error")
	}
}

func TestFindRepoFromLinkedWorktreeReturnsMainCheckout(t *testing.T) {
	repo := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(repo, "init", "-q", "-b", "develop")
	git(repo, "config", "user.email", "test@example.com")
	git(repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("watchtower\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(repo, "add", "README.md")
	git(repo, "commit", "-qm", "initial")
	if err := os.Mkdir(filepath.Join(repo, ".watchtower"), 0o755); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(t.TempDir(), "GH-41")
	git(repo, "worktree", "add", "-q", "-b", "issue/GH-41", worktree)

	got, err := FindRepo(worktree)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(repo)
	got, _ = filepath.EvalSymlinks(got)
	if got != want {
		t.Fatalf("FindRepo(linked worktree) = %q, want %q", got, want)
	}
}

func TestFindRepoFromLinkedWorktreeIgnoresWorktreeWatchtowerDirectory(t *testing.T) {
	repo := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(repo, "init", "-q", "-b", "develop")
	git(repo, "config", "user.email", "test@example.com")
	git(repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("watchtower\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(repo, "add", "README.md")
	git(repo, "commit", "-qm", "initial")
	if err := os.Mkdir(filepath.Join(repo, ".watchtower"), 0o755); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(t.TempDir(), "GH-42")
	git(repo, "worktree", "add", "-q", "-b", "issue/GH-42", worktree)
	if err := os.Mkdir(filepath.Join(worktree, ".watchtower"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := FindRepo(worktree)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(repo)
	got, _ = filepath.EvalSymlinks(got)
	if got != want {
		t.Fatalf("FindRepo(linked worktree with .watchtower) = %q, want %q", got, want)
	}
}

func TestFindRepoUsesDurableClaimBindingForExternalWorkspace(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".watchtower"), 0o755); err != nil {
		t.Fatal(err)
	}
	worktree := t.TempDir()
	cmd := exec.Command("git", "-C", worktree, "init", "-q")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.Mkdir(filepath.Join(worktree, ".watchtower"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := BindWorktree(repo, worktree); err != nil {
		t.Fatal(err)
	}

	got, err := FindRepo(worktree)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(repo)
	got, _ = filepath.EvalSymlinks(got)
	if got != want {
		t.Fatalf("FindRepo(external claimed workspace) = %q, want %q", got, want)
	}
}

func TestRepoIDStableAndShort(t *testing.T) {
	a := RepoID("/some/repo")
	if a != RepoID("/some/repo") || len(a) != 12 {
		t.Fatalf("bad id %q", a)
	}
	if a == RepoID("/other/repo") {
		t.Fatal("ids collide")
	}
}

func TestRepoIDTreatsSymlinkAliasesAsOneRepository(t *testing.T) {
	repo := t.TempDir()
	alias := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(repo, alias); err != nil {
		t.Fatal(err)
	}
	if RepoID(repo) != RepoID(alias) {
		t.Fatalf("RepoID differs for repository and symlink alias: %s != %s", RepoID(repo), RepoID(alias))
	}
}

func TestPullDefaultsOnPushDefaultsOff(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Pull || cfg.Push {
		t.Fatalf("want pull=true push=false by default, got %+v", cfg)
	}
}

func TestPullPushOverridableInConfig(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".watchtower")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("pull: false\npush: true\n"), 0o644)
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pull || !cfg.Push {
		t.Fatalf("config overrides not applied: %+v", cfg)
	}
}
