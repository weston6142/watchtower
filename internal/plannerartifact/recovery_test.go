package plannerartifact

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDurablePairRecoveryRestoresExactPair(t *testing.T) {
	durablePlan := []byte("# Implementation Plan\n\nDurable plan bytes.\n")
	durableTouchset := []byte(`{"globs":["internal/plannerartifact/**"]}`)
	tests := []struct {
		name         string
		plan         *recoveryFileState
		touchset     *recoveryFileState
		wantPlanMode os.FileMode
		wantSetMode  os.FileMode
	}{
		{
			name:         "both targets contain tampered bytes",
			plan:         &recoveryFileState{data: []byte("tampered plan"), mode: 0o600},
			touchset:     &recoveryFileState{data: []byte(`{"tampered":true}`), mode: 0o640},
			wantPlanMode: 0o600,
			wantSetMode:  0o640,
		},
		{
			name:         "plan is missing",
			touchset:     &recoveryFileState{data: []byte(`{"tampered":true}`), mode: 0o600},
			wantPlanMode: 0o644,
			wantSetMode:  0o600,
		},
		{
			name:         "touchset is missing",
			plan:         &recoveryFileState{data: []byte("tampered plan"), mode: 0o640},
			wantPlanMode: 0o640,
			wantSetMode:  0o644,
		},
		{
			name:         "both targets are missing",
			wantPlanMode: 0o644,
			wantSetMode:  0o644,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := t.TempDir()
			worktree := filepath.Join(parent, "worktree")
			if err := os.Mkdir(worktree, 0o755); err != nil {
				t.Fatal(err)
			}
			writeRecoveryState(t, filepath.Join(worktree, "plan.md"), tt.plan)
			writeRecoveryState(t, filepath.Join(worktree, "touchset.json"), tt.touchset)

			recovery, err := recoverDurablePair(worktree, durablePlan, durableTouchset)
			if err != nil {
				t.Fatalf("recover durable pair: %v", err)
			}
			if recovery == nil {
				t.Fatal("successful recovery returned no transaction")
			}
			recovery.Commit()

			assertRecoveryFile(t, filepath.Join(worktree, "plan.md"), durablePlan, tt.wantPlanMode)
			assertRecoveryFile(t, filepath.Join(worktree, "touchset.json"), durableTouchset, tt.wantSetMode)
			assertNoRecoveryStaging(t, parent)
		})
	}
}

func TestDurablePairRecoveryRejectsUnsafeTargetsWithoutMutation(t *testing.T) {
	tests := []struct {
		name       string
		unsafeName string
		makeUnsafe func(t *testing.T, path string)
	}{
		{
			name:       "plan symlink",
			unsafeName: "plan.md",
			makeUnsafe: func(t *testing.T, path string) {
				t.Helper()
				target := filepath.Join(filepath.Dir(path), "symlink-target")
				writeRecoveryState(t, target, &recoveryFileState{data: []byte("symlink authority"), mode: 0o600})
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:       "touchset symlink",
			unsafeName: "touchset.json",
			makeUnsafe: func(t *testing.T, path string) {
				t.Helper()
				target := filepath.Join(filepath.Dir(path), "symlink-target")
				writeRecoveryState(t, target, &recoveryFileState{data: []byte("symlink authority"), mode: 0o600})
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:       "plan directory",
			unsafeName: "plan.md",
			makeUnsafe: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:       "touchset directory",
			unsafeName: "touchset.json",
			makeUnsafe: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := t.TempDir()
			worktree := filepath.Join(parent, "worktree")
			if err := os.Mkdir(worktree, 0o755); err != nil {
				t.Fatal(err)
			}
			otherName := "touchset.json"
			if tt.unsafeName == otherName {
				otherName = "plan.md"
			}
			otherPath := filepath.Join(worktree, otherName)
			otherState := &recoveryFileState{data: []byte("other target authority"), mode: 0o640}
			writeRecoveryState(t, otherPath, otherState)
			unsafePath := filepath.Join(worktree, tt.unsafeName)
			tt.makeUnsafe(t, unsafePath)

			recovery, err := recoverDurablePair(worktree, []byte("durable plan"), []byte("durable touchset"))
			if err == nil {
				t.Fatal("unsafe target was accepted")
			}
			if recovery != nil {
				t.Fatal("unsafe target returned a transaction")
			}
			assertRecoveryFile(t, otherPath, otherState.data, otherState.mode)
			info, statErr := os.Lstat(unsafePath)
			if statErr != nil {
				t.Fatalf("unsafe target was removed: %v", statErr)
			}
			if strings.Contains(tt.name, "symlink") && info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("unsafe target mode = %s, want symlink", info.Mode())
			}
			if strings.Contains(tt.name, "directory") && !info.IsDir() {
				t.Fatalf("unsafe target mode = %s, want directory", info.Mode())
			}
			assertNoRecoveryStaging(t, parent)
		})
	}
}

func TestDurablePairRecoveryRollsBackSecondPublishFailure(t *testing.T) {
	tests := []struct {
		name     string
		plan     *recoveryFileState
		touchset *recoveryFileState
	}{
		{
			name:     "both targets originally present",
			plan:     &recoveryFileState{data: []byte("original plan"), mode: 0o600},
			touchset: &recoveryFileState{data: []byte("original touchset"), mode: 0o640},
		},
		{
			name:     "plan originally absent",
			touchset: &recoveryFileState{data: []byte("original touchset"), mode: 0o600},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := t.TempDir()
			worktree := filepath.Join(parent, "worktree")
			if err := os.Mkdir(worktree, 0o755); err != nil {
				t.Fatal(err)
			}
			planPath := filepath.Join(worktree, "plan.md")
			touchsetPath := filepath.Join(worktree, "touchset.json")
			writeRecoveryState(t, planPath, tt.plan)
			writeRecoveryState(t, touchsetPath, tt.touchset)
			failed := false
			rename := func(oldPath, newPath string) error {
				if !failed && newPath == touchsetPath {
					failed = true
					return errors.New("injected second publish failure")
				}
				return os.Rename(oldPath, newPath)
			}

			recovery, err := recoverDurablePairWithRename(
				worktree, []byte("durable plan"), []byte("durable touchset"), rename)
			if err == nil {
				t.Fatal("second publish failure was not reported")
			}
			if recovery != nil {
				t.Fatal("failed publication returned a successful transaction")
			}
			assertRecoveryState(t, planPath, tt.plan)
			assertRecoveryState(t, touchsetPath, tt.touchset)
			assertNoRecoveryStaging(t, parent)
		})
	}
}

func TestDurablePairRecoveryRollsBackPostPublishMismatch(t *testing.T) {
	parent := t.TempDir()
	worktree := filepath.Join(parent, "worktree")
	if err := os.Mkdir(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(worktree, "plan.md")
	touchsetPath := filepath.Join(worktree, "touchset.json")
	planBefore := &recoveryFileState{data: []byte("original plan"), mode: 0o600}
	touchsetBefore := &recoveryFileState{data: []byte("original touchset"), mode: 0o640}
	writeRecoveryState(t, planPath, planBefore)
	writeRecoveryState(t, touchsetPath, touchsetBefore)
	tampered := false
	rename := func(oldPath, newPath string) error {
		if err := os.Rename(oldPath, newPath); err != nil {
			return err
		}
		if !tampered && newPath == touchsetPath {
			tampered = true
			if err := os.WriteFile(planPath, []byte("post-publish tampering"), 0o600); err != nil {
				return err
			}
		}
		return nil
	}

	recovery, err := recoverDurablePairWithRename(
		worktree, []byte("durable plan"), []byte("durable touchset"), rename)
	if err == nil {
		t.Fatal("post-publish mismatch was not reported")
	}
	if recovery != nil {
		t.Fatal("mismatched publication returned a successful transaction")
	}
	assertRecoveryState(t, planPath, planBefore)
	assertRecoveryState(t, touchsetPath, touchsetBefore)
	assertNoRecoveryStaging(t, parent)
}

func TestDurablePairRecoveryCanRollBackAfterSuccessfulPublish(t *testing.T) {
	parent := t.TempDir()
	worktree := filepath.Join(parent, "worktree")
	if err := os.Mkdir(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(worktree, "plan.md")
	touchsetPath := filepath.Join(worktree, "touchset.json")
	planBefore := &recoveryFileState{data: []byte("original plan"), mode: 0o600}
	writeRecoveryState(t, planPath, planBefore)

	recovery, err := recoverDurablePair(worktree, []byte("durable plan"), []byte("durable touchset"))
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Rollback(); err != nil {
		t.Fatalf("rollback successful recovery: %v", err)
	}
	assertRecoveryState(t, planPath, planBefore)
	assertRecoveryState(t, touchsetPath, nil)
	assertNoRecoveryStaging(t, parent)
}

func TestDurablePairRecoveryCommitDisablesRollback(t *testing.T) {
	parent := t.TempDir()
	worktree := filepath.Join(parent, "worktree")
	if err := os.Mkdir(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(worktree, "plan.md")
	touchsetPath := filepath.Join(worktree, "touchset.json")

	recovery, err := recoverDurablePair(worktree, []byte("durable plan"), []byte("durable touchset"))
	if err != nil {
		t.Fatal(err)
	}
	recovery.Commit()
	if err := recovery.Rollback(); err != nil {
		t.Fatalf("rollback after commit: %v", err)
	}
	assertRecoveryFile(t, planPath, []byte("durable plan"), 0o644)
	assertRecoveryFile(t, touchsetPath, []byte("durable touchset"), 0o644)
	assertNoRecoveryStaging(t, parent)
}

type recoveryFileState struct {
	data []byte
	mode os.FileMode
}

func writeRecoveryState(t *testing.T, path string, state *recoveryFileState) {
	t.Helper()
	if state == nil {
		return
	}
	if err := os.WriteFile(path, state.data, state.mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, state.mode); err != nil {
		t.Fatal(err)
	}
}

func assertRecoveryState(t *testing.T, path string, state *recoveryFileState) {
	t.Helper()
	if state == nil {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s exists after rollback: %v", filepath.Base(path), err)
		}
		return
	}
	assertRecoveryFile(t, path, state.data, state.mode)
}

func assertRecoveryFile(t *testing.T, path string, want []byte, wantMode os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("%s mode = %s, want regular file", filepath.Base(path), info.Mode())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s bytes = %q, want %q", filepath.Base(path), got, want)
	}
	if info.Mode().Perm() != wantMode {
		t.Fatalf("%s mode = %#o, want %#o", filepath.Base(path), info.Mode().Perm(), wantMode)
	}
}

func assertNoRecoveryStaging(t *testing.T, parent string) {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".watchtower-planner-recovery-") {
			t.Fatalf("recovery staging remains: %s", entry.Name())
		}
	}
}
