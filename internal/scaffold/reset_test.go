package scaffold

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/weston6142/watchtower/internal/repocfg"
)

func TestPreparedResetCarriesForwardVerificationCommand(t *testing.T) {
	root := t.TempDir()
	if _, _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareReset(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.SetTestCommand(`scripts/verify --scope "all packages"`); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Commit(); err != nil {
		t.Fatal(err)
	}
	config, err := repocfg.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if config.TestCmd != `scripts/verify --scope "all packages"` {
		t.Fatalf("test command = %q", config.TestCmd)
	}
}

func TestResetReplacesCustomizedAndExtraFilesWithoutBackup(t *testing.T) {
	root := t.TempDir()
	if _, _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, ".watchtower", "config.yaml")
	if err := os.WriteFile(config, []byte("runner: fake\nslots: 99\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(root, ".watchtower", "extra.txt")
	if err := os.WriteFile(extra, []byte("remove me"), 0o644); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareReset(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Commit(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(config)
	if err != nil || strings.Contains(string(body), "slots: 99") {
		t.Fatalf("config was not replaced: %q err=%v", body, err)
	}
	if _, err := os.Stat(extra); !os.IsNotExist(err) {
		t.Fatalf("extra file survived reset: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "watchtower-reset") ||
			strings.Contains(entry.Name(), "watchtower-old") {
			t.Fatalf("retained reset backup %s", entry.Name())
		}
	}
}

func TestPrepareResetValidatesBeforeChangingTarget(t *testing.T) {
	root := t.TempDir()
	if _, _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, ".watchtower", "config.yaml")
	original := []byte("runner: fake\n")
	if err := os.WriteFile(config, original, 0o644); err != nil {
		t.Fatal(err)
	}
	bad := fstest.MapFS{
		"defaults/config.yaml": {Data: []byte("runner: [")},
	}
	if _, err := prepareResetFS(root, bad, "defaults"); err == nil {
		t.Fatal("malformed defaults accepted")
	}
	if body, err := os.ReadFile(config); err != nil || string(body) != string(original) {
		t.Fatalf("target changed before validation: %q err=%v", body, err)
	}
}

func TestApplyRenameFailureRestoresOriginalTree(t *testing.T) {
	root := t.TempDir()
	if _, _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, ".watchtower", "config.yaml")
	original := []byte("runner: fake\n")
	if err := os.WriteFile(config, original, 0o644); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareReset(root)
	if err != nil {
		t.Fatal(err)
	}
	realRename := prepared.rename
	calls := 0
	prepared.rename = func(oldPath, newPath string) error {
		calls++
		if calls == 2 {
			return errors.New("simulated final rename failure")
		}
		return realRename(oldPath, newPath)
	}
	if err := prepared.Apply(); err == nil {
		t.Fatal("Apply succeeded")
	}
	if body, err := os.ReadFile(config); err != nil || string(body) != string(original) {
		t.Fatalf("original tree not restored: %q err=%v", body, err)
	}
	if err := prepared.Cancel(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestRollbackRestoresOriginalTreeAfterAppliedSwap(t *testing.T) {
	root := t.TempDir()
	if _, _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, ".watchtower", "config.yaml")
	original := []byte("runner: fake\nslots: 7\n")
	if err := os.WriteFile(config, original, 0o644); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareReset(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Apply(); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(config); string(body) == string(original) {
		t.Fatal("Apply did not install defaults")
	}
	if err := prepared.Rollback(); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(config); err != nil || string(body) != string(original) {
		t.Fatalf("rollback did not restore original: %q err=%v", body, err)
	}
}
