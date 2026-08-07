// Package verificationcache provides repository-neutral, interruption-safe
// cache leases for verification commands.
package verificationcache

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	formatVersion       = 1
	managedScopeVersion = "verification-cache-v1"
	manifestName        = "manifest.json"
)

// State is the durable state of a cache lease.
type State string

const (
	StateActive      State = "active"
	StateComplete    State = "complete"
	StateQuarantined State = "quarantined"
)

var (
	// ErrLeaseBusy means a live lease owns the repository cache lock.
	ErrLeaseBusy = errors.New("verification cache lease is busy")
	// ErrLeaseInvalid means a lease can no longer be used for completion.
	ErrLeaseInvalid = errors.New("verification cache lease is invalid")
)

// Config identifies one repository verification attempt.
//
// CacheRoot is normally the per-repository daemon data directory. Root is a
// compatibility alias for callers that already use that name. Repository is
// the canonical Git common-directory identity; RepoDir is used to resolve it
// when Repository is empty.
type Config struct {
	CacheRoot     string
	Root          string
	Repository    string
	RepoDir       string
	RepositoryDir string
	BaseSHA       string
	BranchSHA     string
	TreeSHA       string
	Argv          []string
}

// Evidence is the strict durable cache evidence associated with a completed
// lease. It intentionally contains managed scope and digests, not inherited
// environment values.
type Evidence struct {
	Version       int                     `json:"version"`
	LeaseID       string                  `json:"lease_id"`
	State         State                   `json:"state"`
	Repository    string                  `json:"repository"`
	BaseSHA       string                  `json:"base_sha"`
	BranchSHA     string                  `json:"branch_sha"`
	TreeSHA       string                  `json:"tree_sha"`
	CommandDigest string                  `json:"command_argv_digest"`
	ManagedScope  string                  `json:"managed_scope"`
	SeedLeaseID   string                  `json:"seed_lease_id"`
	Quarantines   []QuarantineDisposition `json:"quarantines,omitempty"`
}

// QuarantineDisposition records why an active lease or complete candidate was
// made ineligible. Records are retained for diagnosis and retry evidence.
type QuarantineDisposition struct {
	Version       int       `json:"version"`
	LeaseID       string    `json:"lease_id"`
	CandidateID   string    `json:"candidate_id,omitempty"`
	Reason        string    `json:"reason"`
	Repository    string    `json:"repository"`
	BaseSHA       string    `json:"base_sha"`
	BranchSHA     string    `json:"branch_sha"`
	TreeSHA       string    `json:"tree_sha"`
	CommandDigest string    `json:"command_argv_digest"`
	OwnerPID      int       `json:"owner_pid,omitempty"`
	RecordedAt    time.Time `json:"recorded_at"`
}

// ManagedVariables returns the cache variables controlled by every lease.
func ManagedVariables() []string {
	return []string{"GOCACHE", "GOMODCACHE", "GOPATH"}
}

// CommandDigest hashes the canonical JSON representation of exact argv.
func CommandDigest(argv []string) string {
	encoded, _ := json.Marshal(argv)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

// CanonicalRepositoryIdentity resolves the absolute Git common directory.
func CanonicalRepositoryIdentity(repoDir string) (string, error) {
	if strings.TrimSpace(repoDir) == "" {
		return "", fmt.Errorf("repository directory is required")
	}
	abs, err := filepath.Abs(repoDir)
	if err != nil {
		return "", fmt.Errorf("resolve repository directory: %w", err)
	}
	output, err := exec.Command("git", "-C", abs, "rev-parse", "--path-format=absolute", "--git-common-dir").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("resolve Git common directory: %v: %s", err, strings.TrimSpace(string(output)))
	}
	identity := strings.TrimSpace(string(output))
	if identity == "" {
		return "", fmt.Errorf("Git returned an empty common directory")
	}
	if !filepath.IsAbs(identity) {
		identity = filepath.Join(abs, identity)
	}
	identity, err = filepath.Abs(identity)
	if err != nil {
		return "", fmt.Errorf("resolve Git common directory path: %w", err)
	}
	if identity, err = filepath.EvalSymlinks(identity); err != nil {
		return "", fmt.Errorf("resolve Git common directory symlinks: %w", err)
	}
	return filepath.Clean(identity), nil
}

// Runtime owns the repository-scoped lock, snapshots, and lease records.
type Runtime struct {
	root       string
	repository string
	config     Config
	repoRoot   string
}

// New creates a runtime below the supplied repository daemon data root.
func New(config Config) (*Runtime, error) {
	cacheRoot := config.CacheRoot
	if cacheRoot == "" {
		cacheRoot = config.Root
	}
	if strings.TrimSpace(cacheRoot) == "" {
		return nil, fmt.Errorf("cache root is required")
	}
	cacheRoot, err := filepath.Abs(cacheRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve cache root: %w", err)
	}
	repository := config.Repository
	if repository == "" {
		repoDir := config.RepoDir
		if repoDir == "" {
			repoDir = config.RepositoryDir
		}
		repository, err = CanonicalRepositoryIdentity(repoDir)
		if err != nil {
			return nil, err
		}
	} else {
		repository, err = filepath.Abs(repository)
		if err != nil {
			return nil, fmt.Errorf("resolve repository identity: %w", err)
		}
		repository = filepath.Clean(repository)
	}
	if strings.TrimSpace(repository) == "" {
		return nil, fmt.Errorf("repository identity is required")
	}
	keyDigest := sha256.Sum256([]byte(repository))
	repoRoot := filepath.Join(cacheRoot, "verification-cache", hex.EncodeToString(keyDigest[:]))
	if err := os.MkdirAll(repoRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create verification cache root: %w", err)
	}
	if err := os.Chmod(repoRoot, 0o700); err != nil {
		return nil, fmt.Errorf("protect verification cache root: %w", err)
	}
	for _, dir := range []string{"active", "complete", "quarantine"} {
		if err := os.MkdirAll(filepath.Join(repoRoot, dir), 0o700); err != nil {
			return nil, fmt.Errorf("create verification cache %s directory: %w", dir, err)
		}
	}
	config.Repository = repository
	return &Runtime{root: cacheRoot, repository: repository, config: config, repoRoot: repoRoot}, nil
}

// Acquire obtains the repository lock without stealing a live owner.
func (r *Runtime) Acquire(ctx context.Context, configs ...Config) (*Lease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	lock, err := os.OpenFile(filepath.Join(r.repoRoot, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open verification cache lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrLeaseBusy
		}
		return nil, fmt.Errorf("acquire verification cache lock: %w", err)
	}

	leaseID, err := newLeaseID()
	if err != nil {
		_ = unlock(lock)
		return nil, err
	}
	config := r.config
	if len(configs) > 0 {
		config = configs[0]
		config.Repository = r.repository
	}
	quarantines, err := r.reconcileActive(config)
	if err != nil {
		_ = unlock(lock)
		return nil, err
	}
	seed, candidates, err := r.selectSeed(config)
	if err != nil {
		_ = unlock(lock)
		return nil, err
	}
	quarantines = append(quarantines, candidates...)
	activeDir := filepath.Join(r.repoRoot, "active", leaseID)
	activeRoot := filepath.Join(activeDir, "cache")
	if err := os.MkdirAll(activeRoot, 0o700); err != nil {
		_ = unlock(lock)
		return nil, fmt.Errorf("create active verification cache: %w", err)
	}
	if seed != "" {
		if err := copyTree(seed, activeRoot); err != nil {
			_ = unlock(lock)
			return nil, fmt.Errorf("seed active verification cache: %w", err)
		}
	}
	lease := &Lease{
		runtime:     r,
		lock:        lock,
		id:          leaseID,
		config:      config,
		state:       StateActive,
		activeDir:   activeDir,
		activeRoot:  activeRoot,
		seedLeaseID: seedLeaseID(seed),
		quarantines: append([]QuarantineDisposition(nil), quarantines...),
		ownerPID:    os.Getpid(),
	}
	manifest := lease.manifest()
	if err := writeJSONAtomic(filepath.Join(activeDir, manifestName), manifest); err != nil {
		_ = unlock(lock)
		return nil, fmt.Errorf("publish active verification lease: %w", err)
	}
	return lease, nil
}

// Acquire is a package-level convenience for callers that do not need to
// retain the Runtime separately.
func Acquire(ctx context.Context, config Config) (*Lease, error) {
	runtime, err := New(config)
	if err != nil {
		return nil, err
	}
	return runtime.Acquire(ctx)
}

// CompleteRoot exposes the durable complete-candidate directory for runtime
// diagnostics and behavior-level tests.
func (r *Runtime) CompleteRoot() string { return filepath.Join(r.repoRoot, "complete") }

// ActiveRoot exposes the durable active-lease parent for diagnostics.
func (r *Runtime) ActiveRoot() string { return filepath.Join(r.repoRoot, "active") }

// QuarantineRoot exposes retained recovery records for diagnostics.
func (r *Runtime) QuarantineRoot() string { return filepath.Join(r.repoRoot, "quarantine") }

func (r *Runtime) reconcileActive(config Config) ([]QuarantineDisposition, error) {
	entries, err := os.ReadDir(r.ActiveRoot())
	if err != nil {
		return nil, fmt.Errorf("scan active verification leases: %w", err)
	}
	var records []QuarantineDisposition
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		leaseDir := filepath.Join(r.ActiveRoot(), entry.Name())
		manifest, err := readManifest(filepath.Join(leaseDir, manifestName))
		if err != nil {
			// An unpublished or malformed active manifest cannot be selected.
			record := r.newDisposition(config, entry.Name(), "malformed active lease manifest", 0)
			if writeErr := r.writeDisposition(record); writeErr != nil {
				return nil, writeErr
			}
			records = append(records, record)
			continue
		}
		if manifest.State != StateActive {
			continue
		}
		if manifest.OwnerPID != 0 && manifest.OwnerPID != os.Getpid() && ownerAlive(manifest.OwnerPID) {
			return nil, ErrLeaseBusy
		}
		record := r.newDisposition(config, manifest.LeaseID, "interrupted active lease", manifest.OwnerPID)
		if writeErr := r.writeDisposition(record); writeErr != nil {
			return nil, writeErr
		}
		manifest.State = StateQuarantined
		manifest.OwnerPID = 0
		manifest.Quarantine = append(manifest.Quarantine, record)
		if writeErr := writeJSONAtomic(filepath.Join(leaseDir, manifestName), manifest); writeErr != nil {
			return nil, fmt.Errorf("quarantine active lease %s: %w", manifest.LeaseID, writeErr)
		}
		records = append(records, record)
	}
	return records, nil
}

func (r *Runtime) selectSeed(config Config) (string, []QuarantineDisposition, error) {
	entries, err := os.ReadDir(r.CompleteRoot())
	if err != nil {
		return "", nil, fmt.Errorf("scan complete verification snapshots: %w", err)
	}
	type candidate struct {
		path string
		name string
	}
	var candidates []candidate
	for _, entry := range entries {
		if entry.IsDir() {
			candidates = append(candidates, candidate{path: filepath.Join(r.CompleteRoot(), entry.Name()), name: entry.Name()})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].name > candidates[j].name })
	var dispositions []QuarantineDisposition
	for _, candidate := range candidates {
		manifest, err := readManifest(filepath.Join(candidate.path, manifestName))
		if err == nil {
			err = r.validateCandidate(candidate.path, manifest, config)
		}
		if err == nil {
			return filepath.Join(candidate.path, "cache"), dispositions, nil
		}
		record := r.newDisposition(config, manifest.LeaseID, "invalid complete candidate: "+err.Error(), 0)
		if record.CandidateID == "" {
			record.CandidateID = candidate.name
		}
		if writeErr := r.writeDisposition(record); writeErr != nil {
			return "", nil, writeErr
		}
		dispositions = append(dispositions, record)
	}
	return "", dispositions, nil
}

func (r *Runtime) validateCandidate(path string, manifest leaseManifest, config Config) error {
	if manifest.State != StateComplete {
		return fmt.Errorf("state is %q", manifest.State)
	}
	if manifest.Version != formatVersion || manifest.Repository != r.repository {
		return fmt.Errorf("repository identity or format mismatch")
	}
	if manifest.BaseSHA != config.BaseSHA || manifest.BranchSHA != config.BranchSHA || manifest.TreeSHA != config.TreeSHA {
		return fmt.Errorf("verification identity mismatch")
	}
	if manifest.CommandDigest != CommandDigest(config.Argv) || manifest.ManagedScopeVersion != managedScopeVersion {
		return fmt.Errorf("command or managed scope mismatch")
	}
	if manifest.LeaseID == "" || manifest.Files == nil {
		return fmt.Errorf("complete candidate is incomplete")
	}
	return verifyFiles(filepath.Join(path, "cache"), manifest.Files)
}

func (r *Runtime) newDisposition(config Config, leaseID, reason string, ownerPID int) QuarantineDisposition {
	return QuarantineDisposition{
		Version: formatVersion, LeaseID: leaseID, CandidateID: leaseID, Reason: reason,
		Repository: r.repository, BaseSHA: config.BaseSHA, BranchSHA: config.BranchSHA,
		TreeSHA: config.TreeSHA, CommandDigest: CommandDigest(config.Argv),
		OwnerPID: ownerPID, RecordedAt: time.Now().UTC(),
	}
}

func (r *Runtime) writeDisposition(record QuarantineDisposition) error {
	name := record.LeaseID
	if name == "" {
		name = "candidate-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	path := filepath.Join(r.QuarantineRoot(), name+".json")
	return writeJSONAtomic(path, record)
}

// Lease is one immutable-identity active cache attempt.
type Lease struct {
	runtime     *Runtime
	lock        *os.File
	id          string
	config      Config
	state       State
	activeDir   string
	activeRoot  string
	seedLeaseID string
	quarantines []QuarantineDisposition
	ownerPID    int
	closed      bool
}

func (l *Lease) ID() string                  { return l.id }
func (l *Lease) State() State                { return l.state }
func (l *Lease) Repository() string          { return l.runtime.repository }
func (l *Lease) BaseSHA() string             { return l.config.BaseSHA }
func (l *Lease) BranchSHA() string           { return l.config.BranchSHA }
func (l *Lease) TreeSHA() string             { return l.config.TreeSHA }
func (l *Lease) CommandDigest() string       { return CommandDigest(l.config.Argv) }
func (l *Lease) SeedLeaseID() string         { return l.seedLeaseID }
func (l *Lease) ActiveRoot() string          { return l.activeRoot }
func (l *Lease) CompleteRoot() string        { return filepath.Join(l.runtime.CompleteRoot(), l.id, "cache") }
func (l *Lease) ManagedScope() string        { return l.activeRoot }
func (l *Lease) ManagedScopeVersion() string { return managedScopeVersion }

// Quarantines returns retained recovery dispositions associated with this
// attempt. Unlike strict Evidence, this diagnostic view is available while a
// retry is active and cannot be used as proof of a passing verification.
func (l *Lease) Quarantines() []QuarantineDisposition {
	return append([]QuarantineDisposition(nil), l.quarantines...)
}

// ManagedEnvironment returns the replacement overlay for child processes.
func (l *Lease) ManagedEnvironment() []string {
	return []string{
		"GOCACHE=" + filepath.Join(l.activeRoot, "gocache"),
		"GOMODCACHE=" + filepath.Join(l.activeRoot, "gomodcache"),
		"GOPATH=" + filepath.Join(l.activeRoot, "gopath"),
	}
}

// Environment is an observable map form of ManagedEnvironment.
func (l *Lease) Environment() map[string]string {
	values := make(map[string]string, len(l.ManagedEnvironment()))
	for _, entry := range l.ManagedEnvironment() {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	return values
}

func (l *Lease) manifest() leaseManifest {
	return leaseManifest{
		Version: formatVersion, LeaseID: l.id, State: l.state, Repository: l.Repository(),
		BaseSHA: l.BaseSHA(), BranchSHA: l.BranchSHA(), TreeSHA: l.TreeSHA(),
		Argv: append([]string(nil), l.config.Argv...), CommandDigest: l.CommandDigest(),
		ManagedScope: l.ManagedScope(), ManagedScopeVersion: managedScopeVersion,
		SeedLeaseID: l.seedLeaseID, OwnerPID: l.ownerPID, OwnerStartedAt: time.Now().UTC(),
		Quarantine: append([]QuarantineDisposition(nil), l.quarantines...),
	}
}

// Seal publishes an immutable complete snapshot after the caller's command
// and receipt checks have succeeded.
func (l *Lease) Seal() error {
	if l.state != StateActive || l.closed {
		return ErrLeaseInvalid
	}
	files, err := collectFiles(l.activeRoot)
	if err != nil {
		return fmt.Errorf("probe active verification cache: %w", err)
	}
	temp, err := os.MkdirTemp(l.runtime.CompleteRoot(), ".seal-")
	if err != nil {
		return fmt.Errorf("create complete snapshot staging: %w", err)
	}
	defer os.RemoveAll(temp)
	if err := copyTree(l.activeRoot, filepath.Join(temp, "cache")); err != nil {
		return fmt.Errorf("copy complete verification snapshot: %w", err)
	}
	manifest := l.manifest()
	manifest.State = StateComplete
	manifest.OwnerPID = 0
	manifest.Files = files
	if err := writeJSONAtomic(filepath.Join(temp, manifestName), manifest); err != nil {
		return fmt.Errorf("publish complete verification manifest: %w", err)
	}
	final := filepath.Join(l.runtime.CompleteRoot(), l.id)
	if err := os.Rename(temp, final); err != nil {
		return fmt.Errorf("publish complete verification snapshot: %w", err)
	}
	l.state = StateComplete
	l.ownerPID = 0
	if err := writeJSONAtomic(filepath.Join(l.activeDir, manifestName), l.manifest()); err != nil {
		return fmt.Errorf("publish completed lease state: %w", err)
	}
	return nil
}

// Quarantine makes an active attempt permanently ineligible while retaining
// its files and a machine-readable reason.
func (l *Lease) Quarantine(reason string) error {
	if l.state == StateQuarantined {
		return nil
	}
	if l.state == StateComplete || l.closed {
		return ErrLeaseInvalid
	}
	if strings.TrimSpace(reason) == "" {
		reason = "verification attempt failed"
	}
	record := QuarantineDisposition{
		Version: formatVersion, LeaseID: l.id, Reason: reason, Repository: l.Repository(),
		BaseSHA: l.BaseSHA(), BranchSHA: l.BranchSHA(), TreeSHA: l.TreeSHA(),
		CommandDigest: l.CommandDigest(), OwnerPID: l.ownerPID, RecordedAt: time.Now().UTC(),
	}
	if err := l.runtime.writeDisposition(record); err != nil {
		return err
	}
	l.state = StateQuarantined
	l.ownerPID = 0
	l.quarantines = append(l.quarantines, record)
	manifest := l.manifest()
	if err := writeJSONAtomic(filepath.Join(l.activeDir, manifestName), manifest); err != nil {
		return fmt.Errorf("publish quarantined lease: %w", err)
	}
	return nil
}

// Evidence returns strict proof only after the lease has completed.
func (l *Lease) Evidence() (Evidence, error) {
	if l.state != StateComplete || l.closed {
		return Evidence{}, ErrLeaseInvalid
	}
	return Evidence{
		Version: formatVersion, LeaseID: l.id, State: StateComplete,
		Repository: l.Repository(), BaseSHA: l.BaseSHA(), BranchSHA: l.BranchSHA(),
		TreeSHA: l.TreeSHA(), CommandDigest: l.CommandDigest(), ManagedScope: l.ManagedScope(),
		SeedLeaseID: l.seedLeaseID, Quarantines: append([]QuarantineDisposition(nil), l.quarantines...),
	}, nil
}

// Close releases ownership. An unfinished lease remains active and is
// reconciled into quarantine by the next owner; this models interruption
// without deleting its partial state.
func (l *Lease) Close() error {
	if l.closed {
		return nil
	}
	if l.state == StateActive {
		l.ownerPID = 0
		if err := writeJSONAtomic(filepath.Join(l.activeDir, manifestName), l.manifest()); err != nil {
			return err
		}
	}
	l.closed = true
	return unlock(l.lock)
}

type leaseManifest struct {
	Version             int                     `json:"version"`
	LeaseID             string                  `json:"lease_id"`
	State               State                   `json:"state"`
	Repository          string                  `json:"repository"`
	BaseSHA             string                  `json:"base_sha"`
	BranchSHA           string                  `json:"branch_sha"`
	TreeSHA             string                  `json:"tree_sha"`
	Argv                []string                `json:"argv"`
	CommandDigest       string                  `json:"command_argv_digest"`
	ManagedScope        string                  `json:"managed_scope"`
	ManagedScopeVersion string                  `json:"managed_scope_version"`
	SeedLeaseID         string                  `json:"seed_lease_id"`
	OwnerPID            int                     `json:"owner_pid"`
	OwnerStartedAt      time.Time               `json:"owner_started_at"`
	Files               map[string]FileEvidence `json:"files,omitempty"`
	Quarantine          []QuarantineDisposition `json:"quarantines,omitempty"`
}

// FileEvidence is the stable integrity record for one snapshot file.
type FileEvidence struct {
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
	SHA256 string `json:"sha256"`
}

func readManifest(path string) (leaseManifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return leaseManifest{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var manifest leaseManifest
	if err := decoder.Decode(&manifest); err != nil {
		return leaseManifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return leaseManifest{}, fmt.Errorf("decode manifest: trailing JSON value")
		}
		return leaseManifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	return manifest, nil
}

func collectFiles(root string) (map[string]FileEvidence, error) {
	files := make(map[string]FileEvidence)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink is not valid cache content: %s", path)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(body)
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = FileEvidence{Size: info.Size(), Mode: uint32(info.Mode().Perm()), SHA256: hex.EncodeToString(digest[:])}
		return nil
	})
	return files, err
}

func verifyFiles(root string, expected map[string]FileEvidence) error {
	actual, err := collectFiles(root)
	if err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return fmt.Errorf("snapshot file set changed")
	}
	for path, want := range expected {
		got, ok := actual[path]
		if !ok || got != want {
			return fmt.Errorf("snapshot integrity mismatch for %s", path)
		}
	}
	return nil
}

func copyTree(source, destination string) error {
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink is not valid cache content: %s", path)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o600)
	})
}

func writeJSONAtomic(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func newLeaseID() (string, error) {
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate verification lease identity: %w", err)
	}
	return time.Now().UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(random), nil
}

func seedLeaseID(seed string) string {
	if seed == "" {
		return "no-seed"
	}
	return filepath.Base(filepath.Dir(seed))
}

func ownerAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

func unlock(file *os.File) error {
	if file == nil {
		return nil
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
