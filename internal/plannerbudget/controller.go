package plannerbudget

import (
	"time"

	"github.com/weston6142/watchtower/internal/stageusage"
)

type Controller struct {
	meter  *stageusage.Meter
	ledger *Ledger
}

func NewController(stage string, attempt int, profile Profile, now func() time.Time) (*Controller, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	meter, err := stageusage.New(stage, attempt, stageusage.Limits{
		Calls:   profile.Calls,
		Tokens:  profile.Tokens,
		Elapsed: profile.Elapsed,
	}, now)
	if err != nil {
		return nil, err
	}
	return &Controller{meter: meter, ledger: NewLedger()}, nil
}

func (c *Controller) Add(source Source) { c.ledger.Add(source) }

func (c *Controller) Next() Candidate { return c.ledger.Next() }

func (c *Controller) Read(source Source) ReadResult {
	lease, result := c.AdmitSource(source)
	if result.Err != nil || !result.Charged {
		return result
	}
	reservation := source.Reservation
	if reservation <= 0 {
		reservation = 1
	}
	actual := reservation
	snapshot, err := c.meter.Reconcile(lease, &actual, nil)
	result.Snapshot = snapshot
	result.Err = err
	return result
}

// AdmitSource reserves a new source read without reconciling it. Unchanged
// sources are returned from the attempt ledger without consuming budget.
func (c *Controller) AdmitSource(source Source) (stageusage.Lease, ReadResult) {
	if cached, ok := c.ledger.cached(source); ok {
		return stageusage.Lease{}, ReadResult{
			Source: cached, Content: cached.Content, Snapshot: c.meter.Snapshot(),
		}
	}
	reservation := source.Reservation
	if reservation <= 0 {
		reservation = 1
	}
	lease, snapshot, err := c.meter.Admit(source.ID, reservation)
	if err != nil {
		return stageusage.Lease{}, ReadResult{Source: source, Content: source.Content, Snapshot: snapshot, Err: err}
	}
	c.ledger.observe(source)
	return lease, ReadResult{Source: source, Content: source.Content, Charged: true, Snapshot: snapshot}
}

func (c *Controller) Admit(source string, reservation int64) (stageusage.Lease, stageusage.Snapshot, error) {
	return c.meter.Admit(source, reservation)
}

func (c *Controller) Reconcile(lease stageusage.Lease, actual *int64, operationErr error) (stageusage.Snapshot, error) {
	return c.meter.Reconcile(lease, actual, operationErr)
}

func (c *Controller) Snapshot() stageusage.Snapshot { return c.meter.Snapshot() }

func (c *Controller) Finish(operationErr error) Outcome {
	snapshot := c.meter.Finish(operationErr)
	return outcomeFor(snapshot)
}

func (c *Controller) Outcome() Outcome { return outcomeFor(c.meter.Snapshot()) }

func outcomeFor(snapshot stageusage.Snapshot) Outcome {
	switch snapshot.Status {
	case stageusage.StatusBudgetLimited:
		return OutcomeBudgetLimited
	case stageusage.StatusConfigurationErr:
		return OutcomeConfigurationError
	case stageusage.StatusToolError:
		return OutcomeToolError
	default:
		return OutcomeNormal
	}
}

type Outcome string

const (
	OutcomeNormal             Outcome = "normal"
	OutcomeBudgetLimited      Outcome = "budget_limited"
	OutcomeConfigurationError Outcome = "configuration_error"
	OutcomeToolError          Outcome = "tool_error"
)
