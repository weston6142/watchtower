package repocfg

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	root := t.TempDir()
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runner != "claude" || cfg.Slots != 4 || cfg.ClaudeBin != "claude" {
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
	if cfg.Flows != filepath.Join(root, "myflows") {
		t.Fatalf("relative flows not resolved: %s", cfg.Flows)
	}
	if cfg.ClaudeBin != "claude" { // gap filled from defaults
		t.Fatalf("gap not filled: %+v", cfg)
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

func TestRepoIDStableAndShort(t *testing.T) {
	a := RepoID("/some/repo")
	if a != RepoID("/some/repo") || len(a) != 12 {
		t.Fatalf("bad id %q", a)
	}
	if a == RepoID("/other/repo") {
		t.Fatal("ids collide")
	}
}
