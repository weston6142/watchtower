// Package stageusage provides concurrency-safe, stage-neutral usage accounting.
package stageusage

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

type Dimension string

const (
	DimensionCalls   Dimension = "calls"
	DimensionTokens  Dimension = "tokens"
	DimensionElapsed Dimension = "elapsed"
)

type Status string

const (
	StatusRunning          Status = "running"
	StatusWarning          Status = "warning"
	StatusNormal           Status = "normal"
	StatusBudgetLimited    Status = "budget_limited"
	StatusConfigurationErr Status = "configuration_error"
	StatusToolError        Status = "tool_error"
)

var (
	ErrAdmissionClosed = errors.New("stage usage admission closed")
	ErrLeaseUsed       = errors.New("stage usage lease already reconciled")
	ErrUnknownLease    = errors.New("stage usage lease is unknown")
	ErrInvalidLimits   = errors.New("stage usage limits are invalid")
	ErrInvalidReserve  = errors.New("stage usage reservation is invalid")
	ErrInvalidActual   = errors.New("stage usage actual usage is invalid")
)

type DimensionLimit struct {
	Warning int64 `yaml:"warn" json:"warn"`
	Hard    int64 `yaml:"hard" json:"hard"`
}

type ElapsedLimit struct {
	Warning time.Duration `yaml:"warn" json:"warn"`
	Hard    time.Duration `yaml:"hard" json:"hard"`
}

type Limits struct {
	Calls   DimensionLimit
	Tokens  DimensionLimit
	Elapsed ElapsedLimit
}

type Snapshot struct {
	Stage                  string      `json:"stage"`
	Attempt                int         `json:"attempt"`
	CallsUsed              int64       `json:"calls_used"`
	CallsLimit             int64       `json:"calls_limit"`
	ChargedTokens          int64       `json:"charged_tokens"`
	TokensLimit            int64       `json:"tokens_limit"`
	TokensEstimated        bool        `json:"tokens_estimated"`
	ElapsedMillis          int64       `json:"elapsed_millis"`
	ElapsedLimitMillis     int64       `json:"elapsed_limit_millis"`
	Warnings               []Dimension `json:"warnings,omitempty"`
	Stopped                bool        `json:"stopped"`
	StopDimension          Dimension   `json:"stop_dimension,omitempty"`
	Status                 Status      `json:"status"`
	LastSource             string      `json:"last_source,omitempty"`
	InFlightReconciliation bool        `json:"in_flight_reconciliation"`
}

type Lease struct {
	id          uint64
	source      string
	reservation int64
}

type AdmissionError struct {
	Dimension Dimension
	Snapshot  Snapshot
}

func (e *AdmissionError) Error() string {
	if e.Dimension == "" {
		return ErrAdmissionClosed.Error()
	}
	return fmt.Sprintf("planner budget exhausted: %s", e.Dimension)
}

func (e *AdmissionError) Unwrap() error { return ErrAdmissionClosed }

type Meter struct {
	mu       sync.Mutex
	stage    string
	attempt  int
	limits   Limits
	start    time.Time
	now      func() time.Time
	nextID   uint64
	leases   map[uint64]Lease
	used     map[uint64]bool
	warned   map[Dimension]bool
	snapshot Snapshot
}

func New(stage string, attempt int, limits Limits, now func() time.Time) (*Meter, error) {
	if err := validateLimits(limits); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	start := now()
	m := &Meter{
		stage:   stage,
		attempt: attempt,
		limits:  limits,
		start:   start,
		now:     now,
		leases:  make(map[uint64]Lease),
		used:    make(map[uint64]bool),
		warned:  make(map[Dimension]bool),
		snapshot: Snapshot{
			Stage:              stage,
			Attempt:            attempt,
			CallsLimit:         limits.Calls.Hard,
			TokensLimit:        limits.Tokens.Hard,
			ElapsedLimitMillis: limits.Elapsed.Hard.Milliseconds(),
			Status:             StatusRunning,
		},
	}
	m.mu.Lock()
	m.updateLocked(nil, false)
	m.mu.Unlock()
	return m, nil
}

func validateLimits(limits Limits) error {
	if !validDimensionLimit(limits.Calls) || !validDimensionLimit(limits.Tokens) {
		return ErrInvalidLimits
	}
	if limits.Elapsed.Warning <= 0 || limits.Elapsed.Hard <= 0 || limits.Elapsed.Warning >= limits.Elapsed.Hard {
		return ErrInvalidLimits
	}
	return nil
}

func validDimensionLimit(limit DimensionLimit) bool {
	return limit.Warning > 0 && limit.Hard > 0 &&
		limit.Warning < limit.Hard &&
		limit.Warning != math.MaxInt64 && limit.Hard != math.MaxInt64
}

func (m *Meter) Admit(source string, reservation int64) (Lease, Snapshot, error) {
	if reservation <= 0 || reservation == math.MaxInt64 {
		return Lease{}, m.Snapshot(), ErrInvalidReserve
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateLocked(nil, false)
	if m.snapshot.Stopped {
		return Lease{}, m.snapshotLocked(), &AdmissionError{Dimension: m.snapshot.StopDimension, Snapshot: m.snapshotLocked()}
	}
	if m.snapshot.CallsUsed >= m.limits.Calls.Hard {
		m.stopLocked(DimensionCalls)
		return Lease{}, m.snapshotLocked(), &AdmissionError{Dimension: DimensionCalls, Snapshot: m.snapshotLocked()}
	}
	if reservation > m.limits.Tokens.Hard-m.snapshot.ChargedTokens {
		m.stopLocked(DimensionTokens)
		return Lease{}, m.snapshotLocked(), &AdmissionError{Dimension: DimensionTokens, Snapshot: m.snapshotLocked()}
	}
	if m.elapsedLocked() >= m.limits.Elapsed.Hard {
		m.stopLocked(DimensionElapsed)
		return Lease{}, m.snapshotLocked(), &AdmissionError{Dimension: DimensionElapsed, Snapshot: m.snapshotLocked()}
	}

	m.nextID++
	lease := Lease{id: m.nextID, source: source, reservation: reservation}
	m.leases[lease.id] = lease
	m.snapshot.CallsUsed++
	m.snapshot.ChargedTokens += reservation
	m.snapshot.LastSource = source
	m.updateLocked(nil, false)
	return lease, m.snapshotLocked(), nil
}

func (m *Meter) Reconcile(lease Lease, actual *int64, operationErr error) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.leases[lease.id]
	if !ok {
		return Snapshot{}, ErrUnknownLease
	}
	if m.used[lease.id] {
		return Snapshot{}, ErrLeaseUsed
	}
	if actual != nil && *actual < 0 {
		return Snapshot{}, ErrInvalidActual
	}
	m.used[lease.id] = true
	m.snapshot.InFlightReconciliation = true
	m.snapshot.ChargedTokens -= stored.reservation
	if actual == nil {
		m.snapshot.ChargedTokens += stored.reservation
		m.snapshot.TokensEstimated = true
	} else {
		m.snapshot.ChargedTokens += *actual
	}
	m.snapshot.LastSource = stored.source
	m.updateLocked(operationErr, true)
	m.snapshot.InFlightReconciliation = false
	m.updateLocked(operationErr, false)
	return m.snapshotLocked(), nil
}

// Finish marks a meter's synthesis as normally complete or as a tool error.
// A budget stop always takes precedence over the supplied error.
func (m *Meter) Finish(operationErr error) Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateLocked(operationErr, false)
	if m.snapshot.Stopped {
		return m.snapshotLocked()
	}
	if operationErr != nil {
		m.snapshot.Status = StatusToolError
	} else {
		m.snapshot.Status = StatusNormal
	}
	return m.snapshotLocked()
}

func (m *Meter) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateLocked(nil, false)
	return m.snapshotLocked()
}

func (m *Meter) elapsedLocked() time.Duration {
	elapsed := m.now().Sub(m.start)
	if elapsed < 0 {
		return 0
	}
	return elapsed
}

func (m *Meter) updateLocked(operationErr error, completed bool) {
	elapsed := m.elapsedLocked()
	m.snapshot.ElapsedMillis = elapsed.Milliseconds()
	m.addWarningLocked(DimensionCalls, m.snapshot.CallsUsed >= m.limits.Calls.Warning)
	m.addWarningLocked(DimensionTokens, m.snapshot.ChargedTokens >= m.limits.Tokens.Warning)
	m.addWarningLocked(DimensionElapsed, elapsed >= m.limits.Elapsed.Warning)
	if !m.snapshot.Stopped {
		if m.snapshot.CallsUsed >= m.limits.Calls.Hard {
			m.stopLocked(DimensionCalls)
		} else if m.snapshot.ChargedTokens >= m.limits.Tokens.Hard {
			m.stopLocked(DimensionTokens)
		} else if elapsed >= m.limits.Elapsed.Hard {
			m.stopLocked(DimensionElapsed)
		}
	}
	if m.snapshot.Stopped {
		return
	}
	if operationErr != nil && completed {
		m.snapshot.Status = StatusToolError
		return
	}
	if len(m.snapshot.Warnings) > 0 && m.snapshot.Status == StatusRunning {
		m.snapshot.Status = StatusWarning
	}
}

func (m *Meter) addWarningLocked(dimension Dimension, crossed bool) {
	if !crossed || m.warned[dimension] {
		return
	}
	m.warned[dimension] = true
	m.snapshot.Warnings = append(m.snapshot.Warnings, dimension)
}

func (m *Meter) stopLocked(dimension Dimension) {
	if m.snapshot.Stopped {
		return
	}
	m.snapshot.Stopped = true
	m.snapshot.StopDimension = dimension
	m.snapshot.Status = StatusBudgetLimited
}

func (m *Meter) snapshotLocked() Snapshot {
	snapshot := m.snapshot
	snapshot.Warnings = append([]Dimension(nil), m.snapshot.Warnings...)
	return snapshot
}
