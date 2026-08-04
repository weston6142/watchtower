package stageusage

import (
	"errors"
	"testing"
	"time"
)

type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func testLimits() Limits {
	return Limits{
		Calls:   DimensionLimit{Warning: 1, Hard: 3},
		Tokens:  DimensionLimit{Warning: 10, Hard: 30},
		Elapsed: ElapsedLimit{Warning: time.Second, Hard: 3 * time.Second},
	}
}

func TestMeterStartsRunningAndReconcilesExactUsage(t *testing.T) {
	clock := &fakeClock{}
	m, err := New("plan", 1, testLimits(), clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	initial := m.Snapshot()
	if initial.Status != StatusRunning || initial.CallsUsed != 0 || initial.ChargedTokens != 0 {
		t.Fatalf("initial snapshot = %+v", initial)
	}

	lease, admitted, err := m.Admit("spec.md", 10)
	if err != nil {
		t.Fatal(err)
	}
	if admitted.CallsUsed != 1 || admitted.ChargedTokens != 10 {
		t.Fatalf("admission snapshot = %+v", admitted)
	}
	actual := int64(7)
	snapshot, err := m.Reconcile(lease, &actual, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ChargedTokens != 7 || snapshot.TokensEstimated {
		t.Fatalf("reconciled snapshot = %+v", snapshot)
	}
}

func TestMeterWarningsAreEmittedOnce(t *testing.T) {
	clock := &fakeClock{}
	m, err := New("plan", 1, testLimits(), clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	lease, _, err := m.Admit("spec.md", 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reconcile(lease, nil, nil); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	warned := m.Snapshot()
	if len(warned.Warnings) != 3 || warned.Status != StatusWarning {
		t.Fatalf("warning snapshot = %+v", warned)
	}
	again := m.Snapshot()
	if len(again.Warnings) != len(warned.Warnings) {
		t.Fatalf("warnings repeated: first=%+v second=%+v", warned, again)
	}
}

func TestMeterStopsAtFirstHardDimensionAndReconcilesOnce(t *testing.T) {
	clock := &fakeClock{}
	m, err := New("plan", 1, Limits{
		Calls:   DimensionLimit{Warning: 1, Hard: 2},
		Tokens:  DimensionLimit{Warning: 10, Hard: 20},
		Elapsed: ElapsedLimit{Warning: time.Second, Hard: 2 * time.Second},
	}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	lease, _, err := m.Admit("spec.md", 10)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Second)
	snapshot, err := m.Reconcile(lease, nil, errors.New("provider failed"))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != StatusBudgetLimited || snapshot.StopDimension != DimensionElapsed {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if _, _, err := m.Admit("engine.go", 1); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatalf("second admission error = %v", err)
	}
	actual := int64(10)
	if _, err := m.Reconcile(lease, &actual, nil); !errors.Is(err, ErrLeaseUsed) {
		t.Fatalf("duplicate reconciliation error = %v", err)
	}
}

func TestMeterStopsIndependentlyOnCallsAndTokens(t *testing.T) {
	clock := &fakeClock{}
	callMeter, err := New("plan", 1, Limits{
		Calls:   DimensionLimit{Warning: 1, Hard: 2},
		Tokens:  DimensionLimit{Warning: 99, Hard: 100},
		Elapsed: ElapsedLimit{Warning: time.Minute, Hard: 2 * time.Minute},
	}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	lease, _, err := callMeter.Admit("ISSUE.md", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := callMeter.Reconcile(lease, nil, nil); err != nil {
		t.Fatal(err)
	}
	lease, _, err = callMeter.Admit("STAGE.md", 1)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := callMeter.Reconcile(lease, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.StopDimension != DimensionCalls {
		t.Fatalf("call stop snapshot = %+v", snapshot)
	}

	tokenMeter, err := New("plan", 1, Limits{
		Calls:   DimensionLimit{Warning: 99, Hard: 100},
		Tokens:  DimensionLimit{Warning: 1, Hard: 5},
		Elapsed: ElapsedLimit{Warning: time.Minute, Hard: 2 * time.Minute},
	}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	lease, _, err = tokenMeter.Admit("ISSUE.md", 5)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = tokenMeter.Reconcile(lease, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.StopDimension != DimensionTokens {
		t.Fatalf("token stop snapshot = %+v", snapshot)
	}
}

func TestMeterChargesUnavailableUsageAndOverageAndFailedCalls(t *testing.T) {
	clock := &fakeClock{}
	m, err := New("plan", 1, Limits{
		Calls:   DimensionLimit{Warning: 10, Hard: 20},
		Tokens:  DimensionLimit{Warning: 10, Hard: 50},
		Elapsed: ElapsedLimit{Warning: time.Minute, Hard: 2 * time.Minute},
	}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	lease, _, err := m.Admit("unknown.md", 10)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Reconcile(lease, nil, errors.New("failed call"))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ChargedTokens != 10 || !snapshot.TokensEstimated || snapshot.Status != StatusToolError {
		t.Fatalf("failed estimated snapshot = %+v", snapshot)
	}

	lease, _, err = m.Admit("overage.md", 5)
	if err != nil {
		t.Fatal(err)
	}
	actual := int64(12)
	snapshot, err = m.Reconcile(lease, &actual, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ChargedTokens != 22 || !snapshot.TokensEstimated {
		t.Fatalf("overage snapshot = %+v", snapshot)
	}
}
