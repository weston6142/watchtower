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
	read, charged := c.ledger.read(source)
	if !charged {
		return ReadResult{Source: read, Content: read.Content, Snapshot: c.meter.Snapshot()}
	}
	reservation := source.Reservation
	if reservation <= 0 {
		reservation = 1
	}
	lease, _, err := c.meter.Admit(source.ID, reservation)
	if err != nil {
		return ReadResult{Source: read, Content: read.Content, Charged: true, Snapshot: c.meter.Snapshot(), Err: err}
	}
	actual := reservation
	snapshot, err := c.meter.Reconcile(lease, &actual, nil)
	return ReadResult{Source: read, Content: read.Content, Charged: true, Snapshot: snapshot, Err: err}
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
