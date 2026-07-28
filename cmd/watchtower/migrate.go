package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Legacy names left behind by the pre-rename binary, migrated on first run.
const (
	legacyStateDirName = "guildhall"
	legacyDBPrefix     = "guildhall.db"
	legacySockFileName = "guildhall.sock"
)

// migrateStateDir moves pre-rename user state to the watchtower location. It
// is the first statement of main() because defaultData() is evaluated as a
// flag default in every subcommand, so by the time flags parse the path has
// already been baked in.
func migrateStateDir() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil // no home, no managed state to migrate
	}
	share := filepath.Join(home, ".local", "share")
	return migrateStateTree(
		filepath.Join(share, legacyStateDirName),
		filepath.Join(share, "watchtower"),
		os.Args,
	)
}

// migrateStateTree is the testable core of migrateStateDir. See spec §A1.
func migrateStateTree(oldRoot, newRoot string, args []string) error {
	// Step 0: an explicit -data/--data means the caller opted out of the
	// managed location. Checked before any stat so tests and scripts that
	// point at a temp dir never touch the developer's live state.
	if hasDataFlag(args) {
		return nil
	}
	// Step 1: nothing to move, or already migrated. Never merge two trees.
	if _, err := os.Stat(oldRoot); err != nil {
		return nil
	}
	if _, err := os.Stat(newRoot); err == nil {
		return nil
	}
	// Step 2: no copy-fallback — a cross-device rename surfaces to the user
	// as an instruction to move the tree by hand.
	if err := os.Rename(oldRoot, newRoot); err != nil {
		return fmt.Errorf("migrating state dir: %w", err)
	}
	// Step 3: rename the per-repo databases. Failing here is fatal: a moved
	// tree whose guildhall.db never became watchtower.db would let SQLite
	// create a fresh empty database, a migration that looks like a clean
	// start while every issue is gone.
	repos := filepath.Join(newRoot, "repos")
	entries, err := os.ReadDir(repos)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("migrating state dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := migrateRepoDir(filepath.Join(repos, e.Name())); err != nil {
			return fmt.Errorf("migrating state dir: %w", err)
		}
	}
	return nil
}

// migrateRepoDir renames guildhall.db and any journal siblings inside one
// repo's data dir, then drops files that name a dead process.
func migrateRepoDir(dir string) error {
	files, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		name := f.Name()
		if !strings.HasPrefix(name, legacyDBPrefix) {
			continue
		}
		// Preserves -journal/-wal/-shm suffixes if a journal mode is ever
		// enabled; today only the plain file exists.
		suffix := strings.TrimPrefix(name, legacyDBPrefix)
		if err := os.Rename(filepath.Join(dir, name), filepath.Join(dir, "watchtower.db"+suffix)); err != nil {
			return err
		}
	}
	// Step 4: best-effort. Worst case a stale socket makes `repos` report a
	// daemon as running; that is not worth failing the migration over.
	for _, stale := range []string{legacySockFileName, pidFileName} {
		if err := os.Remove(filepath.Join(dir, stale)); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "watchtower: removing stale %s: %v\n", stale, err)
		}
	}
	return nil
}

// hasDataFlag reports whether an explicit data-dir flag appears anywhere in
// args. flag.Parse cannot answer this: parsing happens per-subcommand, long
// after migration must have run.
func hasDataFlag(args []string) bool {
	for _, a := range args {
		if a == "-data" || a == "--data" ||
			strings.HasPrefix(a, "-data=") || strings.HasPrefix(a, "--data=") {
			return true
		}
	}
	return false
}
