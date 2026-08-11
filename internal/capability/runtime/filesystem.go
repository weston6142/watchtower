package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/touchset"
)

func (s *Session) ReadFile(path string) ([]byte, error) {
	canonical, err := touchset.CanonicalPath(path)
	if err != nil || !s.hasOperation(capability.OpWorkspaceRead) || !pathMatches(s.contract.Contract.Reads, canonical) {
		return nil, s.deny(capability.OpWorkspaceRead, path)
	}
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	info, err := s.root.Lstat(filepath.FromSlash(canonical))
	if err != nil || !info.Mode().IsRegular() || hardLinked(info) {
		return nil, s.deny(capability.OpWorkspaceRead, canonical)
	}
	body, err := s.root.ReadFile(filepath.FromSlash(canonical))
	if err == nil {
		s.record("runtime", "passed", "", capability.OpWorkspaceRead, []string{canonical})
	}
	return body, err
}

func (s *Session) WriteFile(path string, body []byte, mutation capability.MutationClass) error {
	canonical, err := touchset.CanonicalPath(path)
	if err != nil || !s.hasOperation(capability.OpWorkspaceMutate) || !grantAllows(s.contract.Contract.Writes, canonical, mutation) {
		return s.deny(capability.OpWorkspaceMutate, path)
	}
	if err := s.ensureOpen(); err != nil {
		return err
	}
	relative := filepath.FromSlash(canonical)
	info, statErr := s.root.Lstat(relative)
	switch mutation {
	case capability.MutationCreate:
		if statErr == nil {
			return s.deny(capability.OpWorkspaceMutate, canonical)
		}
	case capability.MutationModify:
		if statErr != nil || !info.Mode().IsRegular() || hardLinked(info) {
			return s.deny(capability.OpWorkspaceMutate, canonical)
		}
	default:
		return s.deny(capability.OpWorkspaceMutate, canonical)
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if parent := filepath.Dir(relative); parent != "." {
		if err := s.root.MkdirAll(parent, 0o755); err != nil {
			return err
		}
	}
	if err := s.root.WriteFile(relative, body, 0o644); err != nil {
		return err
	}
	s.record("runtime", "passed", "", capability.OpWorkspaceMutate, []string{canonical})
	return nil
}

func (s *Session) Remove(path string) error {
	canonical, err := touchset.CanonicalPath(path)
	if err != nil || !grantAllows(s.contract.Contract.Writes, canonical, capability.MutationDelete) {
		return s.deny(capability.OpWorkspaceMutate, path)
	}
	if err := s.ensureOpen(); err != nil {
		return err
	}
	if err := s.root.Remove(filepath.FromSlash(canonical)); err != nil {
		return err
	}
	s.record("runtime", "passed", "", capability.OpWorkspaceMutate, []string{canonical})
	return nil
}

func (s *Session) Rename(from, to string) error {
	canonicalFrom, fromErr := touchset.CanonicalPath(from)
	canonicalTo, toErr := touchset.CanonicalPath(to)
	if fromErr != nil || toErr != nil || !grantAllows(s.contract.Contract.Writes, canonicalFrom, capability.MutationRename) ||
		!grantAllows(s.contract.Contract.Writes, canonicalTo, capability.MutationRename) {
		return s.deny(capability.OpWorkspaceMutate, from, to)
	}
	if err := s.ensureOpen(); err != nil {
		return err
	}
	if err := s.root.Rename(filepath.FromSlash(canonicalFrom), filepath.FromSlash(canonicalTo)); err != nil {
		return err
	}
	s.record("runtime", "passed", "", capability.OpWorkspaceMutate, []string{canonicalFrom, canonicalTo})
	return nil
}

func (s *Session) Chmod(path string, mode os.FileMode) error {
	canonical, err := touchset.CanonicalPath(path)
	if err != nil || mode&^0o777 != 0 || !grantAllows(s.contract.Contract.Writes, canonical, capability.MutationMetadata) {
		return s.deny(capability.OpWorkspaceMutate, path)
	}
	if err := s.ensureOpen(); err != nil {
		return err
	}
	info, err := s.root.Lstat(filepath.FromSlash(canonical))
	if err != nil || !info.Mode().IsRegular() || hardLinked(info) {
		return s.deny(capability.OpWorkspaceMutate, canonical)
	}
	if err := s.root.Chmod(filepath.FromSlash(canonical), mode); err != nil {
		return err
	}
	s.record("runtime", "passed", "", capability.OpWorkspaceMutate, []string{canonical})
	return nil
}

func (s *Session) Symlink(target, path string) error {
	canonicalTarget, targetErr := touchset.CanonicalPath(target)
	canonicalPath, pathErr := touchset.CanonicalPath(path)
	if targetErr != nil || pathErr != nil || !pathMatches(s.contract.Contract.Reads, canonicalTarget) ||
		!grantAllows(s.contract.Contract.Writes, canonicalPath, capability.MutationLink) {
		return s.deny(capability.OpWorkspaceMutate, target, path)
	}
	if err := s.ensureOpen(); err != nil {
		return err
	}
	if _, err := s.root.Lstat(filepath.FromSlash(canonicalTarget)); err != nil {
		return s.deny(capability.OpWorkspaceMutate, canonicalTarget, canonicalPath)
	}
	if err := s.root.Symlink(filepath.FromSlash(canonicalTarget), filepath.FromSlash(canonicalPath)); err != nil {
		return err
	}
	s.record("runtime", "passed", "", capability.OpWorkspaceMutate, []string{canonicalTarget, canonicalPath})
	return nil
}

func pathMatches(grants []string, path string) bool {
	for _, grant := range grants {
		if matched, err := touchset.Match(grant, path); err == nil && matched {
			return true
		}
	}
	return false
}

func grantAllows(grants []capability.PathGrant, path string, mutation capability.MutationClass) bool {
	for _, grant := range grants {
		matched, err := touchset.Match(grant.Path, path)
		if err != nil || !matched {
			continue
		}
		for _, allowed := range grant.Mutations {
			if allowed == mutation {
				return true
			}
		}
	}
	return false
}

func hardLinked(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink > 1
}
