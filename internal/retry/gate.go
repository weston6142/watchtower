package retry

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/weston6142/watchtower/internal/failure"
)

const ContextSchemaVersion = 1

const (
	ContextActive      = "active"
	ContextClosed      = "closed"
	ContextUnavailable = "unavailable"
)

var (
	ErrContextNotFound       = errors.New("retry context not found")
	ErrContextUnavailable    = errors.New("retry context unavailable")
	ErrInvalidContext        = errors.New("invalid retry context")
	ErrAuthorizationConflict = errors.New("retry authorization conflict")
)

// Context is the aggregate durable retry state for the latest matching
// failure occurrence. Equal failure fingerprints share this record while the
// append-only failure history retains every occurrence.
type Context struct {
	ContextKey         string                   `json:"context_key"`
	SchemaVersion      int                      `json:"schema_version"`
	LatestRecordID     int64                    `json:"latest_record_id"`
	IssueID            string                   `json:"issue_id"`
	Stage              string                   `json:"stage"`
	FailureSite        failure.Site             `json:"failure_site"`
	FailureClass       failure.Class            `json:"failure_class"`
	RetryDisposition   failure.RetryDisposition `json:"retry_disposition"`
	FailureFingerprint string                   `json:"failure_fingerprint"`
	State              StateVector              `json:"state"`
	SharedUsed         int                      `json:"shared_used"`
	SharedCap          int                      `json:"shared_cap"`
	ModelResampleUsed  int                      `json:"model_resample_used"`
	ModelResampleCap   int                      `json:"model_resample_cap"`
	DecisionIdentity   string                   `json:"decision_identity,omitempty"`
	Policy             PolicyEvidence           `json:"policy"`
	Lifecycle          string                   `json:"lifecycle"`
	Version            int64                    `json:"version"`
	UpdatedAt          string                   `json:"updated_at"`
}

type AuthorizationUpdate struct {
	ContextKey       string
	ExpectedVersion  int64
	Kind             Kind
	Current          StateVector
	SharedCap        int
	ModelResampleCap int
	DecisionIdentity string
	Policy           PolicyEvidence
}

type ContextStore interface {
	LoadRetryContext(context.Context, string, string) (Context, error)
	AuthorizeRetry(context.Context, AuthorizationUpdate) (Context, error)
}

type Decision struct {
	Authorized    bool           `json:"authorized"`
	Authorization *Authorization `json:"authorization,omitempty"`
	Rejection     *Rejection     `json:"rejection,omitempty"`
}

type Gate struct {
	store  ContextStore
	policy Policy
}

func NewGate(store ContextStore, policy Policy) (*Gate, error) {
	if store == nil {
		return nil, fmt.Errorf("retry context store is required")
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return &Gate{store: store, policy: policy}, nil
}

func MustNewGate(store ContextStore, policy Policy) *Gate {
	gate, err := NewGate(store, policy)
	if err != nil {
		panic(err)
	}
	return gate
}

type evaluatedRetry struct {
	context   Context
	rule      Rule
	changed   []string
	unchanged []string
}

func (g *Gate) Authorize(ctx context.Context, request Request) (Decision, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(request.IssueID) == "" || strings.TrimSpace(request.Stage) == "" {
		return rejectionDecision(request, Context{}, Rule{}, ReasonInvalidContext, nil, nil,
			"record a fresh durable failure context before retrying"), nil
	}
	if request.Kind != KindAutomatic && request.Kind != KindExplicit && request.Kind != KindModelResample {
		return rejectionDecision(request, Context{}, Rule{}, ReasonKindNotAllowed, nil, nil,
			"request automatic, explicit, or model-resample retry"), nil
	}
	var last evaluatedRetry
	for conflict := 0; conflict < 2; conflict++ {
		stored, err := g.store.LoadRetryContext(ctx, request.IssueID, request.Stage)
		if err != nil {
			switch {
			case errors.Is(err, ErrContextNotFound), errors.Is(err, ErrContextUnavailable), errors.Is(err, ErrInvalidContext):
				return rejectionDecision(request, Context{}, Rule{}, ReasonInvalidContext, nil, nil,
					"record a fresh durable failure context before retrying"), nil
			default:
				return rejectionDecision(request, Context{}, Rule{}, ReasonPersistenceUnavailable, nil, nil,
					"restore durable retry storage before retrying"), nil
			}
		}
		evaluated, rejected := g.evaluate(request, stored)
		if rejected != nil {
			return *rejected, nil
		}
		last = evaluated
		updated, err := g.store.AuthorizeRetry(ctx, AuthorizationUpdate{
			ContextKey: evaluated.context.ContextKey, ExpectedVersion: evaluated.context.Version,
			Kind: request.Kind, Current: request.Current, SharedCap: evaluated.rule.SharedCap,
			ModelResampleCap: evaluated.rule.ModelResampleCap, DecisionIdentity: request.DecisionIdentity,
			Policy: evaluated.rule.Evidence,
		})
		if err == nil {
			authorization := &Authorization{
				ContextKey: updated.ContextKey, IssueID: updated.IssueID, Stage: updated.Stage, Kind: request.Kind,
				FailureSite: updated.FailureSite, FailureClass: updated.FailureClass,
				FailureFingerprint: updated.FailureFingerprint,
				Attempt:            updated.SharedUsed, SharedUsed: updated.SharedUsed, SharedCap: evaluated.rule.SharedCap,
				ModelResampleUsed: updated.ModelResampleUsed, ModelResampleCap: evaluated.rule.ModelResampleCap,
				DecisionIdentity: updated.DecisionIdentity,
				Current:          updated.State, ChangedDimensions: evaluated.changed, Policy: evaluated.rule.Evidence,
			}
			return Decision{Authorized: true, Authorization: authorization}, nil
		}
		if !errors.Is(err, ErrAuthorizationConflict) {
			return rejectionDecision(request, stored, evaluated.rule, ReasonPersistenceUnavailable,
				evaluated.changed, evaluated.unchanged, "restore durable retry storage before retrying"), nil
		}
	}
	return rejectionDecision(request, last.context, last.rule, ReasonPersistenceConflict, last.changed, last.unchanged,
		"reload the issue after the concurrent retry completes"), nil
}

func (g *Gate) evaluate(request Request, stored Context) (evaluatedRetry, *Decision) {
	if stored.SchemaVersion != ContextSchemaVersion || stored.ContextKey == "" || stored.Lifecycle != ContextActive ||
		stored.IssueID != request.IssueID || stored.Stage != request.Stage {
		rejected := rejectionDecision(request, stored, Rule{}, ReasonInvalidContext, nil, nil,
			"record a fresh durable failure context before retrying")
		return evaluatedRetry{}, &rejected
	}
	rule, err := g.policy.Rule(stored.FailureClass, stored.RetryDisposition)
	if err != nil {
		rejected := rejectionDecision(request, stored, Rule{}, ReasonInvalidContext, nil, nil,
			"record a supported durable failure classification before retrying")
		return evaluatedRetry{}, &rejected
	}
	if err := availabilityError(stored.State); err != nil {
		dimensions := UnavailableDimensions(stored.State)
		rejected := rejectionDecision(request, stored, rule, ReasonFingerprintUnavailable, nil, nil,
			"record fresh failure evidence with available "+strings.Join(dimensions, ", ")+" state")
		rejected.Rejection.UnavailableDimensions = append([]string(nil), dimensions...)
		return evaluatedRetry{}, &rejected
	}
	if err := availabilityError(request.Current); err != nil {
		dimensions := UnavailableDimensions(request.Current)
		rejected := rejectionDecision(request, stored, rule, ReasonFingerprintUnavailable, nil, nil,
			"restore the unavailable "+strings.Join(dimensions, ", ")+" state evidence")
		rejected.Rejection.UnavailableDimensions = append([]string(nil), dimensions...)
		return evaluatedRetry{}, &rejected
	}
	changed, unchanged := ChangedDimensions(stored.State, request.Current)
	if rule.Behavior == BehaviorNonRetryable {
		rejected := rejectionDecision(request, stored, rule, ReasonNonRetryable, changed, unchanged,
			"follow operator recovery guidance instead of repeating verification")
		return evaluatedRetry{}, &rejected
	}
	if request.Kind == KindModelResample {
		if !rule.ModelResampleEligible || !stateDimensionAvailable("decision", request.DecisionIdentity) ||
			request.DecisionIdentity == stored.DecisionIdentity {
			rejected := rejectionDecision(request, stored, rule, ReasonKindNotAllowed, changed, unchanged,
				"select a newer durable decision for an eligible explicit model resample")
			return evaluatedRetry{}, &rejected
		}
		if !containsDimension(changed, "decision") {
			rejected := rejectionDecision(request, stored, rule, ReasonStateUnchanged, changed, unchanged,
				"record a newer durable model decision before retrying verification")
			return evaluatedRetry{}, &rejected
		}
	}
	if rule.RequiresStateChange && len(changed) == 0 {
		rejected := rejectionDecision(request, stored, rule, ReasonStateUnchanged, changed, unchanged,
			"change tree, config, environment, or decision state before retrying verification")
		return evaluatedRetry{}, &rejected
	}
	if stored.SharedUsed >= rule.SharedCap {
		rejected := rejectionDecision(request, stored, rule, ReasonCapExhausted, changed, unchanged,
			"establish a new failure fingerprint or use specialized lifecycle recovery")
		return evaluatedRetry{}, &rejected
	}
	if request.Kind == KindModelResample && stored.ModelResampleUsed >= rule.ModelResampleCap {
		rejected := rejectionDecision(request, stored, rule, ReasonModelResampleExhausted, changed, unchanged,
			"change non-model state or follow operator recovery guidance")
		return evaluatedRetry{}, &rejected
	}
	if request.Kind == KindModelResample {
		rule.Evidence.Behavior = BehaviorModelResample
	}
	return evaluatedRetry{context: stored, rule: rule, changed: changed, unchanged: unchanged}, nil
}

func containsDimension(dimensions []string, want string) bool {
	for _, dimension := range dimensions {
		if dimension == want {
			return true
		}
	}
	return false
}

func rejectionDecision(request Request, stored Context, rule Rule, reason Reason,
	changed, unchanged []string, nextAction string,
) Decision {
	rejection := &Rejection{
		Reason: reason, IssueID: request.IssueID, Stage: request.Stage, Kind: request.Kind,
		FailureClass: stored.FailureClass, FailureFingerprint: stored.FailureFingerprint,
		SharedUsed: stored.SharedUsed, SharedCap: rule.SharedCap,
		ModelResampleUsed: stored.ModelResampleUsed, ModelResampleCap: rule.ModelResampleCap,
		ChangedDimensions: append([]string(nil), changed...), UnchangedDimensions: append([]string(nil), unchanged...),
		Policy: rule.Evidence, NextAction: nextAction,
	}
	return Decision{Rejection: rejection}
}
