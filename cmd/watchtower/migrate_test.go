package main

import (
	"os"
	"path/filepath"
	"testing"
)

// plantOldTree builds a pre-rename state tree with one repo's worth of files.
func plantOldTree(t *testing.T, oldRoot string) {
	t.Helper()
	repo := filepath.Join(oldRoot, "repos", "03dac843b906")
	if err := os.MkdirAll(filepath.Join(repo, "issues", "GH-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"guildhall.db":         "sqlite",
		"guildhall.db-wal":     "wal",
		"guildhall.sock":       "",
		"daemon.pid":           "999999",
		"daemon.log":           "log",
		"issues/GH-1/ISSUE.md": "# GH-1",
	} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrateStateTree(t *testing.T) {
	t.Run("happy path renames tree and databases", func(t *testing.T) {
		root := t.TempDir()
		oldRoot, newRoot := filepath.Join(root, "guildhall"), filepath.Join(root, "watchtower")
		plantOldTree(t, oldRoot)

		if err := migrateStateTree(oldRoot, newRoot, []string{"watchtower", "tower"}); err != nil {
			t.Fatalf("migrate: %v", err)
		}

		if _, err := os.Stat(oldRoot); !os.IsNotExist(err) {
			t.Error("old root survived the migration")
		}
		repo := filepath.Join(newRoot, "repos", "03dac843b906")
		for _, want := range []string{"watchtower.db", "watchtower.db-wal", "daemon.log", "issues/GH-1/ISSUE.md"} {
			if _, err := os.Stat(filepath.Join(repo, want)); err != nil {
				t.Errorf("expected %s: %v", want, err)
			}
		}
		// Guards the flagship silent failure: a leftover guildhall.db means
		// store.Open finds nothing and SQLite creates an empty database.
		for _, gone := range []string{"guildhall.db", "guildhall.db-wal", "guildhall.sock", "daemon.pid"} {
			if _, err := os.Stat(filepath.Join(repo, gone)); !os.IsNotExist(err) {
				t.Errorf("expected %s to be gone", gone)
			}
		}
		if b, err := os.ReadFile(filepath.Join(repo, "watchtower.db")); err != nil || string(b) != "sqlite" {
			t.Errorf("db contents not preserved: %q %v", b, err)
		}
	})

	t.Run("no-op when old root absent", func(t *testing.T) {
		root := t.TempDir()
		oldRoot, newRoot := filepath.Join(root, "guildhall"), filepath.Join(root, "watchtower")
		if err := migrateStateTree(oldRoot, newRoot, []string{"watchtower", "tower"}); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		if _, err := os.Stat(newRoot); !os.IsNotExist(err) {
			t.Error("migration created a new root out of nothing")
		}
	})

	t.Run("no-op when new root already present", func(t *testing.T) {
		root := t.TempDir()
		oldRoot, newRoot := filepath.Join(root, "guildhall"), filepath.Join(root, "watchtower")
		plantOldTree(t, oldRoot)
		if err := os.MkdirAll(newRoot, 0o755); err != nil {
			t.Fatal(err)
		}

		if err := migrateStateTree(oldRoot, newRoot, []string{"watchtower", "tower"}); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		// Never merge two trees: both roots must be left exactly as found.
		if _, err := os.Stat(filepath.Join(oldRoot, "repos", "03dac843b906", "guildhall.db")); err != nil {
			t.Errorf("old tree was disturbed: %v", err)
		}
		if entries, err := os.ReadDir(newRoot); err != nil || len(entries) != 0 {
			t.Errorf("new tree was written into: %v %v", entries, err)
		}
	})

	t.Run("skipped when an explicit data flag is present", func(t *testing.T) {
		for _, args := range [][]string{
			{"watchtower", "tower", "--data", "/tmp/x"},
			{"watchtower", "tower", "-data", "/tmp/x"},
			{"watchtower", "tower", "--data=/tmp/x"},
			{"watchtower", "tower", "-data=/tmp/x"},
		} {
			root := t.TempDir()
			oldRoot, newRoot := filepath.Join(root, "guildhall"), filepath.Join(root, "watchtower")
			plantOldTree(t, oldRoot)

			if err := migrateStateTree(oldRoot, newRoot, args); err != nil {
				t.Fatalf("%v: %v", args, err)
			}
			if _, err := os.Stat(newRoot); !os.IsNotExist(err) {
				t.Errorf("%v: migration ran despite an explicit data flag", args)
			}
		}
	})

	t.Run("errors when an inner database rename fails", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		root := t.TempDir()
		oldRoot, newRoot := filepath.Join(root, "guildhall"), filepath.Join(root, "watchtower")
		plantOldTree(t, oldRoot)
		repo := filepath.Join(oldRoot, "repos", "03dac843b906")
		if err := os.Chmod(repo, 0o500); err != nil { // read+exec: renames inside fail
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(filepath.Join(newRoot, "repos", "03dac843b906"), 0o755) })

		err := migrateStateTree(oldRoot, newRoot, []string{"watchtower", "tower"})
		if err == nil {
			t.Fatal("expected an error so main exits before any store opens")
		}
		if _, serr := os.Stat(filepath.Join(newRoot, "repos", "03dac843b906", "watchtower.db")); !os.IsNotExist(serr) {
			t.Error("watchtower.db was created despite the failure")
		}
	})
}

func TestHasDataFlag(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"watchtower", "tower"}, false},
		{[]string{"watchtower", "new", "--title", "no data here"}, false},
		{[]string{"watchtower", "tower", "--data", "/tmp/x"}, true},
		{[]string{"watchtower", "tower", "-data", "/tmp/x"}, true},
		{[]string{"watchtower", "tower", "--data=/tmp/x"}, true},
		{[]string{"watchtower", "tower", "-data=/tmp/x"}, true},
		{[]string{"watchtower", "daemon", "--repo", "/r", "--data", "/d"}, true},
	} {
		if got := hasDataFlag(tc.args); got != tc.want {
			t.Errorf("hasDataFlag(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}
