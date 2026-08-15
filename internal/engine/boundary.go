package engine

import (
	"context"
	"errors"

	"github.com/weston6142/watchtower/internal/store"
)

// BoundaryKind identifies the durable production subsystem that committed a
// checkpoint before execution was interrupted.
type BoundaryKind string

const (
	BoundaryStageLifecycle BoundaryKind = "stage_lifecycle"
	BoundaryFinalization   BoundaryKind = "finalization"
)

// DurableBoundary is an observable checkpoint whose state is already durable
// when an observer is called.
type DurableBoundary struct {
	Kind    BoundaryKind
	ID      string
	IssueID string
	Stage   string
}

// BoundaryObserver observes a committed checkpoint. Returning an error stops
// the active operation after persistence without changing the durable state.
type BoundaryObserver interface {
	AfterCommit(context.Context, DurableBoundary) error
}

type committedBoundaryError struct {
	boundary DurableBoundary
	err      error
}

type committedInterruption struct{ err error }

func (e *committedInterruption) Error() string { return e.err.Error() }
func (e *committedInterruption) Unwrap() error { return e.err }

func InterruptAfterCommit(err error) error {
	if err == nil {
		return nil
	}
	return &committedInterruption{err: err}
}

func isCommittedInterruption(err error) bool {
	var interruption *committedInterruption
	return errors.As(err, &interruption)
}

func (e *Engine) InterruptPendingDecision(issueID string) error {
	e.mu.Lock()
	is, ok := e.issues[issueID]
	if !ok {
		e.mu.Unlock()
		return errors.New("unknown issue " + issueID)
	}
	if is.stageCancel == nil {
		e.mu.Unlock()
		return errors.New("issue " + issueID + " has no running stage")
	}
	interrupted := false
	for id, pending := range e.pend {
		if pending.IssueID != issueID {
			continue
		}
		delete(e.pend, id)
		if pending.reply != nil {
			close(pending.reply)
		}
		interrupted = true
	}
	if !interrupted {
		e.mu.Unlock()
		return errors.New("issue " + issueID + " has no pending decision")
	}
	is.decisionInterrupted = true
	cancel := is.stageCancel
	e.mu.Unlock()
	cancel()
	return nil
}

func (e *Engine) consumeDecisionInterruption(is *issueState) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	interrupted := is.decisionInterrupted
	is.decisionInterrupted = false
	return interrupted
}

func (e *committedBoundaryError) Error() string { return e.err.Error() }
func (e *committedBoundaryError) Unwrap() error { return e.err }

func (e *Engine) notifyBoundary(ctx context.Context, boundary DurableBoundary) error {
	if e.cfg.BoundaryObserver == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := e.cfg.BoundaryObserver.AfterCommit(ctx, boundary); err != nil {
		return &committedBoundaryError{boundary: boundary, err: err}
	}
	return nil
}

func isFinalizationBoundary(state string) bool {
	for _, candidate := range store.FinalizationBoundaries() {
		if candidate == state {
			return true
		}
	}
	return false
}

func (e *Engine) notifyFinalizationBoundary(ctx context.Context, issueID, stage, state string) error {
	if !isFinalizationBoundary(state) {
		return nil
	}
	return e.notifyBoundary(ctx, DurableBoundary{
		Kind: BoundaryFinalization, ID: state, IssueID: issueID, Stage: stage,
	})
}
