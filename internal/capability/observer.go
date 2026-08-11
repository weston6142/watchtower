package capability

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

type EngineWrite struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type FileIdentity struct {
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	Mode       uint32 `json:"mode"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256,omitempty"`
	LinkTarget string `json:"link_target,omitempty"`
	Device     uint64 `json:"device,omitempty"`
	Inode      uint64 `json:"inode,omitempty"`
	LinkCount  uint64 `json:"link_count,omitempty"`
}

type GitIdentity struct {
	Branch         string            `json:"branch"`
	Head           string            `json:"head"`
	Tree           string            `json:"tree"`
	IndexDigest    string            `json:"index_digest"`
	WorktreeDigest string            `json:"worktree_digest"`
	Refs           map[string]string `json:"refs"`
	RemoteDigest   string            `json:"remote_digest"`
}

type Baseline struct {
	Workspace       string                  `json:"workspace"`
	Entries         map[string]FileIdentity `json:"entries"`
	Git             GitIdentity             `json:"git"`
	EngineWrites    map[string]string       `json:"engine_writes,omitempty"`
	Digest          string                  `json:"digest"`
	CommonGitDigest string                  `json:"common_git_digest"`
}

type DeltaEntry struct {
	Path           string        `json:"path"`
	FromPath       string        `json:"from_path,omitempty"`
	Mutation       MutationClass `json:"mutation"`
	Kind           string        `json:"kind,omitempty"`
	SHA256         string        `json:"sha256,omitempty"`
	LinkTarget     string        `json:"link_target,omitempty"`
	ExternalTarget bool          `json:"external_target,omitempty"`
	EngineOwned    bool          `json:"engine_owned,omitempty"`
	Content        string        `json:"-"`
}

type GitDelta struct {
	Before          GitIdentity `json:"before"`
	After           GitIdentity `json:"after"`
	BranchChanged   bool        `json:"branch_changed"`
	HeadChanged     bool        `json:"head_changed"`
	TreeChanged     bool        `json:"tree_changed"`
	IndexChanged    bool        `json:"index_changed"`
	WorktreeChanged bool        `json:"worktree_changed"`
	RefsChanged     bool        `json:"refs_changed"`
	RemoteChanged   bool        `json:"remote_changed"`
}

type Delta struct {
	Workspace string       `json:"workspace"`
	Entries   []DeltaEntry `json:"entries"`
	Git       GitDelta     `json:"git"`
	Digest    string       `json:"digest"`
}

type Observer struct {
	DescendantsReaped func() bool
}

func (o Observer) Capture(workspace string, engineWrites []EngineWrite) (Baseline, error) {
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return Baseline{}, fmt.Errorf("resolve workspace: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return Baseline{}, err
	}
	entries, err := snapshotFiles(root)
	if err != nil {
		return Baseline{}, err
	}
	git, commonDigest, err := snapshotGit(root)
	if err != nil {
		if _, statErr := os.Lstat(filepath.Join(root, ".git")); !errors.Is(statErr, os.ErrNotExist) {
			return Baseline{}, err
		}
		git = GitIdentity{}
		commonDigest = hashObserverBytes([]byte("no-git-workspace"))
	}
	exclusions := make(map[string]string, len(engineWrites))
	for _, write := range engineWrites {
		path, pathErr := canonicalObservedPath(write.Path)
		if pathErr != nil || !validSHA256(write.SHA256) {
			return Baseline{}, fmt.Errorf("invalid engine write exclusion %q", write.Path)
		}
		if previous, ok := exclusions[path]; ok && previous != write.SHA256 {
			return Baseline{}, fmt.Errorf("conflicting engine write exclusion %q", path)
		}
		exclusions[path] = write.SHA256
	}
	baseline := Baseline{Workspace: root, Entries: entries, Git: git, EngineWrites: exclusions, CommonGitDigest: commonDigest}
	baseline.Digest, err = baselineDigest(baseline)
	return baseline, err
}

func (o Observer) Compare(baseline Baseline) (Delta, error) {
	if o.DescendantsReaped != nil && !o.DescendantsReaped() {
		return Delta{}, fmt.Errorf("agent descendants are not reaped")
	}
	entries, err := snapshotFiles(baseline.Workspace)
	if err != nil {
		return Delta{}, err
	}
	git, _, err := snapshotGit(baseline.Workspace)
	if err != nil {
		if _, statErr := os.Lstat(filepath.Join(baseline.Workspace, ".git")); !errors.Is(statErr, os.ErrNotExist) {
			return Delta{}, err
		}
		git = GitIdentity{}
	}
	delta := Delta{Workspace: baseline.Workspace}
	deleted := make(map[string]FileIdentity)
	created := make(map[string]FileIdentity)
	for path, before := range baseline.Entries {
		after, ok := entries[path]
		if !ok {
			deleted[path] = before
			continue
		}
		if before.Kind != after.Kind || before.SHA256 != after.SHA256 || before.LinkTarget != after.LinkTarget {
			mutation := MutationModify
			if before.Kind == "symlink" || after.Kind == "symlink" {
				mutation = MutationLink
			}
			delta.Entries = append(delta.Entries, deltaEntry(baseline, after, path, "", mutation))
		}
		if before.Mode != after.Mode {
			delta.Entries = append(delta.Entries, deltaEntry(baseline, after, path, "", MutationMetadata))
		}
		if before.Inode != after.Inode || before.LinkCount != after.LinkCount {
			delta.Entries = append(delta.Entries, deltaEntry(baseline, after, path, "", MutationLink))
		}
	}
	for path, after := range entries {
		if _, ok := baseline.Entries[path]; !ok {
			created[path] = after
		}
	}
	for oldPath, before := range deleted {
		renamed := ""
		for newPath, after := range created {
			if before.Kind == after.Kind && before.SHA256 != "" && before.SHA256 == after.SHA256 && before.LinkTarget == after.LinkTarget {
				renamed = newPath
				break
			}
		}
		if renamed != "" {
			delta.Entries = append(delta.Entries, deltaEntry(baseline, created[renamed], renamed, oldPath, MutationRename))
			delete(created, renamed)
			continue
		}
		delta.Entries = append(delta.Entries, DeltaEntry{Path: oldPath, Mutation: MutationDelete, Kind: before.Kind})
	}
	for path, after := range created {
		mutation := MutationCreate
		if after.Kind == "symlink" || after.LinkCount > 1 {
			mutation = MutationLink
		}
		delta.Entries = append(delta.Entries, deltaEntry(baseline, after, path, "", mutation))
	}
	sort.Slice(delta.Entries, func(i, j int) bool {
		if delta.Entries[i].Path == delta.Entries[j].Path {
			return delta.Entries[i].Mutation < delta.Entries[j].Mutation
		}
		return delta.Entries[i].Path < delta.Entries[j].Path
	})
	delta.Git = GitDelta{
		Before: baseline.Git, After: git,
		BranchChanged:   baseline.Git.Branch != git.Branch,
		HeadChanged:     baseline.Git.Head != git.Head,
		TreeChanged:     baseline.Git.Tree != git.Tree,
		IndexChanged:    baseline.Git.IndexDigest != git.IndexDigest,
		WorktreeChanged: baseline.Git.WorktreeDigest != git.WorktreeDigest,
		RefsChanged:     !reflect.DeepEqual(baseline.Git.Refs, git.Refs),
		RemoteChanged:   baseline.Git.RemoteDigest != git.RemoteDigest,
	}
	encoded, err := json.Marshal(struct {
		Entries []DeltaEntry `json:"entries"`
		Git     GitDelta     `json:"git"`
	}{delta.Entries, delta.Git})
	if err != nil {
		return Delta{}, err
	}
	delta.Digest = hashObserverBytes(encoded)
	return delta, nil
}

func snapshotFiles(root string) (map[string]FileIdentity, error) {
	entries := make(map[string]FileIdentity)
	err := filepath.WalkDir(root, func(path string, dir os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == ".git" || strings.HasPrefix(rel, ".git/") {
			if dir.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		identity := FileIdentity{Path: rel, Mode: uint32(info.Mode()), Size: info.Size(), Kind: fileKind(info.Mode())}
		identity.Device, identity.Inode, identity.LinkCount = statIdentity(info.Sys())
		if info.Mode()&os.ModeSymlink != 0 {
			identity.LinkTarget, err = os.Readlink(path)
			if err != nil {
				return err
			}
			identity.SHA256 = hashObserverBytes([]byte(identity.LinkTarget))
		} else if info.Mode().IsRegular() {
			identity.SHA256, err = hashFile(path)
			if err != nil {
				return err
			}
		}
		entries[rel] = identity
		return nil
	})
	return entries, err
}

func snapshotGit(root string) (GitIdentity, string, error) {
	branch, err := gitOutput(root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return GitIdentity{}, "", err
	}
	head, err := gitOutput(root, "rev-parse", "HEAD")
	if err != nil {
		return GitIdentity{}, "", err
	}
	tree, err := gitOutput(root, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return GitIdentity{}, "", err
	}
	indexPath, err := gitOutput(root, "rev-parse", "--git-path", "index")
	if err != nil {
		return GitIdentity{}, "", err
	}
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(root, indexPath)
	}
	indexDigest, err := hashFile(indexPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return GitIdentity{}, "", err
	}
	status, err := gitOutputBytes(root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return GitIdentity{}, "", err
	}
	refsOutput, err := gitOutput(root, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads", "refs/tags")
	if err != nil {
		return GitIdentity{}, "", err
	}
	refs := make(map[string]string)
	for _, line := range strings.Split(refsOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			refs[fields[0]] = fields[1]
		}
	}
	remote, remoteErr := gitOutputBytesAllowExitOne(root, "config", "--get-regexp", `^remote\..*\.url$`)
	if remoteErr != nil {
		return GitIdentity{}, "", remoteErr
	}
	commonDir, err := gitOutput(root, "rev-parse", "--git-common-dir")
	if err != nil {
		return GitIdentity{}, "", err
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(root, commonDir)
	}
	commonDir, _ = filepath.Abs(commonDir)
	return GitIdentity{
		Branch: branch, Head: head, Tree: tree, IndexDigest: indexDigest,
		WorktreeDigest: hashObserverBytes(status), Refs: refs, RemoteDigest: hashObserverBytes(remote),
	}, hashObserverBytes([]byte(commonDir)), nil
}

func deltaEntry(baseline Baseline, identity FileIdentity, path, from string, mutation MutationClass) DeltaEntry {
	entry := DeltaEntry{Path: path, FromPath: from, Mutation: mutation, Kind: identity.Kind, SHA256: identity.SHA256, LinkTarget: identity.LinkTarget}
	entry.ExternalTarget = identity.Kind == "symlink" && symlinkEscapes(baseline.Workspace, path, identity.LinkTarget)
	if expected, ok := baseline.EngineWrites[path]; ok && expected == identity.SHA256 {
		entry.EngineOwned = true
	}
	return entry
}

func symlinkEscapes(root, path, target string) bool {
	if target == "" {
		return false
	}
	resolved := target
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(root, filepath.Dir(filepath.FromSlash(path)), target)
	}
	resolved = filepath.Clean(resolved)
	rel, err := filepath.Rel(root, resolved)
	return err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func baselineDigest(baseline Baseline) (string, error) {
	paths := make([]string, 0, len(baseline.Entries))
	for path := range baseline.Entries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	entries := make([]FileIdentity, 0, len(paths))
	for _, path := range paths {
		entries = append(entries, baseline.Entries[path])
	}
	encoded, err := json.Marshal(struct {
		Workspace string         `json:"workspace"`
		Entries   []FileIdentity `json:"entries"`
		Git       GitIdentity    `json:"git"`
	}{baseline.Workspace, entries, baseline.Git})
	if err != nil {
		return "", err
	}
	return hashObserverBytes(encoded), nil
}

func canonicalObservedPath(value string) (string, error) {
	if value == "" || filepath.IsAbs(value) || strings.ContainsRune(value, '\x00') || strings.Contains(value, "\\") {
		return "", fmt.Errorf("unsafe observed path")
	}
	clean := filepath.ToSlash(filepath.Clean(value))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe observed path")
	}
	return clean, nil
}

func fileKind(mode os.FileMode) string {
	switch {
	case mode.IsRegular():
		return "regular"
	case mode&os.ModeSymlink != 0:
		return "symlink"
	default:
		return "special"
	}
}

func statIdentity(value any) (device, inode, links uint64) {
	if value == nil {
		return 0, 0, 0
	}
	v := reflect.Indirect(reflect.ValueOf(value))
	field := func(name string) uint64 {
		item := v.FieldByName(name)
		if !item.IsValid() {
			return 0
		}
		switch item.Kind() {
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return item.Uint()
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return uint64(item.Int())
		default:
			return 0
		}
	}
	return field("Dev"), field("Ino"), field("Nlink")
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func hashObserverBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func gitOutput(root string, args ...string) (string, error) {
	value, err := gitOutputBytes(root, args...)
	return strings.TrimSpace(string(value)), err
}

func gitOutputBytes(root string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return output, nil
}

func gitOutputBytesAllowExitOne(root string, args ...string) ([]byte, error) {
	value, err := gitOutputBytes(root, args...)
	if err == nil {
		return value, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return nil, nil
	}
	return nil, err
}
