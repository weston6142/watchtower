package plannerartifact

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const recoveryStagingPrefix = ".watchtower-planner-recovery-"

type recoveryTarget struct {
	path    string
	existed bool
	data    []byte
	mode    os.FileMode
}

type pairRecovery struct {
	before   [2]recoveryTarget
	rename   func(string, string) error
	finished bool
}

func recoverDurablePair(worktree string, plan, touchset []byte) (*pairRecovery, error) {
	return recoverDurablePairWithRename(worktree, plan, touchset, os.Rename)
}

func recoverDurablePairWithRename(worktree string, plan, touchset []byte, rename func(string, string) error) (*pairRecovery, error) {
	if rename == nil {
		return nil, errors.New("planner recovery: rename operation is unavailable")
	}

	paths := [2]string{
		filepath.Join(worktree, "plan.md"),
		filepath.Join(worktree, "touchset.json"),
	}
	recovery := &pairRecovery{rename: rename}
	for i, path := range paths {
		target, err := snapshotRecoveryTarget(path)
		if err != nil {
			return nil, errors.New("planner recovery: durable pair target is unsafe or unavailable")
		}
		recovery.before[i] = target
	}

	staging, err := newRecoveryStaging(worktree)
	if err != nil {
		return nil, errors.New("planner recovery: cannot create private staging")
	}
	defer os.RemoveAll(staging)

	desired := [2][]byte{append([]byte(nil), plan...), append([]byte(nil), touchset...)}
	staged := [2]string{
		filepath.Join(staging, "next-plan.md"),
		filepath.Join(staging, "next-touchset.json"),
	}
	for i := range recovery.before {
		if err := writeRecoveryFile(staged[i], desired[i], recovery.before[i].mode); err != nil {
			return nil, errors.New("planner recovery: cannot stage durable pair")
		}
		if recovery.before[i].existed {
			backup := filepath.Join(staging, "before-"+filepath.Base(recovery.before[i].path))
			if err := writeRecoveryFile(backup, recovery.before[i].data, recovery.before[i].mode); err != nil {
				return nil, errors.New("planner recovery: cannot stage prior durable pair")
			}
		}
	}

	for i := range recovery.before {
		if err := rename(staged[i], recovery.before[i].path); err != nil {
			return recoveryFailed(recovery)
		}
	}
	if err := syncRecoveryDirectory(worktree); err != nil {
		return recoveryFailed(recovery)
	}
	for i := range recovery.before {
		if !recoveryFileMatches(recovery.before[i].path, desired[i], recovery.before[i].mode) {
			return recoveryFailed(recovery)
		}
	}

	return recovery, nil
}

func snapshotRecoveryTarget(path string) (recoveryTarget, error) {
	target := recoveryTarget{path: path, mode: 0o644}
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return target, nil
	}
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return recoveryTarget{}, errors.New("unsafe recovery target")
	}

	file, err := os.Open(path)
	if err != nil {
		return recoveryTarget{}, errors.New("unavailable recovery target")
	}
	data, readErr := io.ReadAll(file)
	opened, statErr := file.Stat()
	closeErr := file.Close()
	after, afterErr := os.Lstat(path)
	if readErr != nil || statErr != nil || closeErr != nil || afterErr != nil ||
		after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() ||
		!os.SameFile(before, opened) || !os.SameFile(opened, after) {
		return recoveryTarget{}, errors.New("unstable recovery target")
	}
	target.existed = true
	target.data = append([]byte(nil), data...)
	target.mode = after.Mode().Perm()
	return target, nil
}

func newRecoveryStaging(worktree string) (string, error) {
	staging, err := os.MkdirTemp(filepath.Dir(worktree), recoveryStagingPrefix)
	if err != nil {
		return "", err
	}
	if err := os.Chmod(staging, 0o700); err != nil {
		_ = os.RemoveAll(staging)
		return "", err
	}
	return staging, nil
}

func writeRecoveryFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err == nil {
		err = file.Chmod(mode.Perm())
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func syncRecoveryDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	err = directory.Sync()
	if closeErr := directory.Close(); err == nil {
		err = closeErr
	}
	return err
}

func recoveryFileMatches(path string, data []byte, mode os.FileMode) bool {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != mode.Perm() {
		return false
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, data) {
		return false
	}
	after, err := os.Lstat(path)
	return err == nil && after.Mode()&os.ModeSymlink == 0 && after.Mode().IsRegular() && os.SameFile(info, after)
}

func recoveryFailed(recovery *pairRecovery) (*pairRecovery, error) {
	if err := recovery.Rollback(); err != nil {
		return nil, errors.New("planner recovery: durable pair update failed and prior state could not be restored")
	}
	return nil, errors.New("planner recovery: durable pair update failed; prior state restored")
}

// Rollback restores the exact pair observed before recovery. It is safe to call
// after Commit or after an already successful rollback.
func (recovery *pairRecovery) Rollback() error {
	if recovery == nil || recovery.finished {
		return nil
	}
	worktree := filepath.Dir(recovery.before[0].path)
	staging, err := newRecoveryStaging(worktree)
	if err != nil {
		return errors.New("planner recovery: prior state could not be restored")
	}
	defer os.RemoveAll(staging)

	backups := [2]string{}
	for i, target := range recovery.before {
		if !target.existed {
			continue
		}
		backups[i] = filepath.Join(staging, "restore-"+filepath.Base(target.path))
		if err := writeRecoveryFile(backups[i], target.data, target.mode); err != nil {
			return errors.New("planner recovery: prior state could not be restored")
		}
	}

	restored := true
	for i, target := range recovery.before {
		if target.existed {
			if err := recovery.rename(backups[i], target.path); err != nil {
				restored = false
			}
			continue
		}
		if err := os.Remove(target.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			restored = false
		}
	}
	if err := syncRecoveryDirectory(worktree); err != nil {
		restored = false
	}
	for _, target := range recovery.before {
		if !recoveryTargetMatchesSnapshot(target) {
			restored = false
		}
	}
	if !restored {
		return errors.New("planner recovery: prior state could not be restored")
	}
	recovery.finished = true
	return nil
}

// Commit retains the recovered durable pair and disables later rollback.
func (recovery *pairRecovery) Commit() {
	if recovery != nil {
		recovery.finished = true
	}
}

func recoveryTargetMatchesSnapshot(target recoveryTarget) bool {
	if target.existed {
		return recoveryFileMatches(target.path, target.data, target.mode)
	}
	_, err := os.Lstat(target.path)
	return errors.Is(err, os.ErrNotExist)
}
