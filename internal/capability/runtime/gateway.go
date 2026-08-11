package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/runner"
)

type StartRequest struct {
	Contract        capability.CompiledContract
	Plan            capability.EnforcementPlan
	Worktree        string
	Backend         Backend
	EnginePaths     []string
	EngineSockets   []string
	Environment     []string
	PlannerArtifact func(any) error
	Audit           func(capability.AuditRecord)
}

type Session struct {
	contract        capability.CompiledContract
	plan            capability.EnforcementPlan
	worktree        string
	root            *os.Root
	backend         Backend
	scratch         string
	environment     []string
	plannerArtifact func(any) error
	audit           func(capability.AuditRecord)

	mu        sync.Mutex
	processes map[*runner.ProcessTree]bool
	closed    bool
}

func Start(_ context.Context, request StartRequest) (*Session, error) {
	if request.Backend == nil {
		return nil, unsupported("containment backend is unavailable")
	}
	backendPlan, err := request.Backend.Preflight(request.Contract)
	if err != nil {
		return nil, err
	}
	if request.Plan.PlanID == "" {
		request.Plan = backendPlan
	}
	if request.Plan.PlanID != backendPlan.PlanID || request.Plan.ContractID != request.Contract.ContractID {
		return nil, unsupported("containment plan identity mismatch")
	}
	if err := runner.ValidateEnforcementPlan(request.Contract, request.Plan); err != nil {
		return nil, err
	}
	rootPath, err := filepath.EvalSymlinks(request.Worktree)
	if err != nil {
		return nil, unsupported("workspace identity is unavailable")
	}
	rootPath, err = filepath.Abs(rootPath)
	contractRoot, contractErr := filepath.EvalSymlinks(request.Contract.Contract.WorkspaceRoot)
	if contractErr == nil {
		contractRoot, contractErr = filepath.Abs(contractRoot)
	}
	if err != nil || contractErr != nil || rootPath != contractRoot {
		return nil, unsupported("workspace identity mismatch")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	scratch, err := os.MkdirTemp("", "watchtower-capability-")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if err := os.Chmod(scratch, 0o700); err != nil {
		_ = root.Close()
		_ = os.RemoveAll(scratch)
		return nil, err
	}
	session := &Session{
		contract: request.Contract, plan: request.Plan, worktree: rootPath, root: root,
		backend: request.Backend, scratch: scratch, environment: scrubEnvironment(request.Environment),
		plannerArtifact: request.PlannerArtifact, audit: request.Audit, processes: make(map[*runner.ProcessTree]bool),
	}
	session.environment = append(session.environment, "TMPDIR="+scratch)
	session.record("launch", "passed", "", "", nil)
	return session, nil
}

func (s *Session) ScratchRoot() string {
	if s == nil {
		return ""
	}
	return s.scratch
}

func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	processes := make([]*runner.ProcessTree, 0, len(s.processes))
	for process := range s.processes {
		processes = append(processes, process)
	}
	s.mu.Unlock()
	for _, process := range processes {
		_ = process.TerminateAndWait(250 * time.Millisecond)
	}
	rootErr := s.root.Close()
	removeErr := os.RemoveAll(s.scratch)
	s.record("reap", "passed", "", "", nil)
	if rootErr != nil {
		return rootErr
	}
	return removeErr
}

func (s *Session) ensureOpen() error {
	if s == nil || s.root == nil {
		return unsupported("runtime session is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return unsupported("runtime session is closed")
	}
	return nil
}

func (s *Session) deny(operation capability.OperationClass, paths ...string) error {
	canonical := make([]string, 0, len(paths))
	for _, path := range paths {
		if path != "" {
			canonical = append(canonical, filepath.ToSlash(path))
		}
	}
	s.record("runtime", "denied", capability.ReasonRuntimeDenied, operation, canonical)
	return &capability.PolicyError{Phase: "runtime", Reason: capability.ReasonRuntimeDenied, Operation: operation, Paths: canonical, Diagnostic: "operation is outside compiled contract"}
}

func (s *Session) record(phase, outcome string, reason capability.FailureReason, operation capability.OperationClass, paths []string) {
	if s == nil || s.audit == nil {
		return
	}
	s.audit(capability.AuditRecord{
		Attempt: capability.AttemptIdentity{
			IssueID: s.contract.Contract.IssueID, Stage: s.contract.Contract.Stage, AttemptID: s.contract.Contract.AttemptID,
		},
		ContractID: s.contract.ContractID, Phase: phase, Outcome: outcome, Reason: reason,
		Provider: s.plan.Provider, Implementation: s.plan.Implementation, Operation: operation, Paths: append([]string(nil), paths...),
	})
}

func unsupported(diagnostic string) error {
	return &capability.PolicyError{Phase: "preflight", Reason: capability.ReasonProviderUnsupported, Diagnostic: diagnostic}
}

func scrubEnvironment(values []string) []string {
	result := make([]string, 0, len(values)+1)
	hasPath := false
	for _, value := range values {
		key, _, ok := strings.Cut(value, "=")
		if !ok || key == "" {
			continue
		}
		lower := strings.ToLower(key)
		if key == "TMPDIR" {
			continue
		}
		if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") ||
			strings.Contains(lower, "credential") || strings.Contains(lower, "ssh") || strings.Contains(lower, "gpg") || strings.HasPrefix(lower, "watchtower_") {
			continue
		}
		hasPath = hasPath || key == "PATH"
		result = append(result, value)
	}
	if !hasPath {
		result = append(result, "PATH=/usr/bin:/bin")
	}
	return result
}

func (s *Session) PlannerApply(value any) error {
	if !s.hasOperation(capability.OpPlannerArtifactApply) || s.plannerArtifact == nil {
		return s.deny(capability.OpPlannerArtifactApply)
	}
	if err := s.plannerArtifact(value); err != nil {
		return fmt.Errorf("planner artifact apply: %w", err)
	}
	s.record("runtime", "passed", "", capability.OpPlannerArtifactApply, nil)
	return nil
}

func (s *Session) hasOperation(want capability.OperationClass) bool {
	for _, operation := range s.contract.Contract.Operations {
		if operation == want {
			return true
		}
	}
	return false
}
