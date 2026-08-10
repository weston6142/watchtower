package engine

import (
	"fmt"
	"path/filepath"

	"github.com/weston6142/watchtower/internal/plannerartifact"
)

func plannerBindingKey(binding plannerartifact.Binding) string {
	return fmt.Sprintf("%s\x00%s\x00%d\x00%s", binding.IssueID, binding.Stage, binding.Attempt, binding.Worktree)
}

func canonicalPlannerWorktree(worktree string) (string, error) {
	abs, err := filepath.Abs(worktree)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func (e *Engine) RegisterPlannerAuthority(authority *plannerartifact.Authority) {
	if authority == nil {
		return
	}
	binding := authority.Binding()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.plannerAuthorities[plannerBindingKey(binding)] = authority
}

func (e *Engine) UnregisterPlannerAuthority(binding plannerartifact.Binding) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.plannerAuthorities, plannerBindingKey(binding))
}

func (e *Engine) plannerAuthorityForScope(asserted plannerartifact.Binding) (*plannerartifact.Authority, plannerartifact.Binding, error) {
	if asserted.Worktree == "" {
		return nil, plannerartifact.Binding{}, plannerartifact.NewAuthorityError(plannerartifact.ErrorAuthorityUninitialized, "", "no active planner scope")
	}
	canonical, err := canonicalPlannerWorktree(asserted.Worktree)
	if err != nil {
		return nil, plannerartifact.Binding{}, plannerartifact.NewAuthorityError(plannerartifact.ErrorScopeMismatch, "", "worktree is unavailable")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var match *plannerartifact.Authority
	var binding plannerartifact.Binding
	for _, authority := range e.plannerAuthorities {
		candidate := authority.Binding()
		if candidate.Worktree != canonical {
			continue
		}
		if match != nil {
			return nil, plannerartifact.Binding{}, plannerartifact.NewAuthorityError(plannerartifact.ErrorAuthorityState, "", "planner scope is ambiguous")
		}
		match, binding = authority, candidate
	}
	if match == nil {
		return nil, plannerartifact.Binding{}, plannerartifact.NewAuthorityError(plannerartifact.ErrorAuthorityUninitialized, "", "no active planner authority")
	}
	if (asserted.IssueID != "" && asserted.IssueID != binding.IssueID) ||
		(asserted.Stage != "" && asserted.Stage != binding.Stage) ||
		(asserted.Attempt != 0 && asserted.Attempt != binding.Attempt) {
		return nil, plannerartifact.Binding{}, plannerartifact.NewAuthorityError(plannerartifact.ErrorScopeMismatch, "", "planner scope does not match active attempt")
	}
	return match, binding, nil
}

// IssuePlannerAuthority presents the live engine-issued capability to the
// daemon route. A client may assert scope fields, but cannot create one.
func (e *Engine) IssuePlannerAuthority(asserted plannerartifact.Binding) (string, plannerartifact.Binding, error) {
	authority, binding, err := e.plannerAuthorityForScope(asserted)
	if err != nil {
		return "", plannerartifact.Binding{}, err
	}
	return authority.CapabilityHandle(), binding, nil
}

func (e *Engine) ApplyPlannerArtifact(scope plannerartifact.Binding, handle string, request plannerartifact.WriteRequest) (string, error) {
	authority, _, err := e.plannerAuthorityForScope(scope)
	if err != nil {
		return "", err
	}
	if err := authority.ApplyWithCapability(handle, request); err != nil {
		return "", err
	}
	return request.Key, nil
}

func (e *Engine) RetryPlannerAuthority(scope plannerartifact.Binding) (string, error) {
	authority, binding, err := e.plannerAuthorityForScope(scope)
	if err != nil {
		return "", err
	}
	retry, err := plannerartifact.CreateOrLoad(e.cfg.Store, binding)
	if err != nil {
		return "", err
	}
	e.mu.Lock()
	e.plannerAuthorities[plannerBindingKey(binding)] = retry
	e.mu.Unlock()
	_ = authority.Close()
	return retry.CapabilityHandle(), nil
}

func (e *Engine) ValidatePlannerAuthority(scope plannerartifact.Binding, handle string) error {
	authority, _, err := e.plannerAuthorityForScope(scope)
	if err != nil {
		return err
	}
	if err := authority.VerifyCapability(handle); err != nil {
		return err
	}
	return authority.ValidateComplete()
}

func (e *Engine) ExpirePlannerAuthority(scope plannerartifact.Binding) error {
	authority, binding, err := e.plannerAuthorityForScope(scope)
	if err != nil {
		return err
	}
	if err := authority.Expire(); err != nil {
		return err
	}
	e.UnregisterPlannerAuthority(binding)
	return nil
}
