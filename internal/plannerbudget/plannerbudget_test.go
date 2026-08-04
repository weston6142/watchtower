package plannerbudget

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/stageusage"
)

type fixtureClock struct{ now time.Time }

func (c *fixtureClock) Now() time.Time { return c.now }

func (c *fixtureClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func fixtureProfile() Profile {
	return Profile{
		Calls:  stageusage.DimensionLimit{Warning: 8, Hard: 10},
		Tokens: stageusage.DimensionLimit{Warning: 80, Hard: 100},
		Elapsed: stageusage.ElapsedLimit{
			Warning: time.Minute,
			Hard:    2 * time.Minute,
		},
	}
}

func newFixtureController(t *testing.T, profile Profile) (*Controller, *fixtureClock) {
	t.Helper()
	clock := &fixtureClock{}
	controller, err := NewController("plan", 1, profile, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	return controller, clock
}

func TestProfileDefaultsAndFiniteOverrides(t *testing.T) {
	defaults := DefaultProfile()
	if defaults.Calls.Warning != 24 || defaults.Calls.Hard != 32 ||
		defaults.Tokens.Warning != 200000 || defaults.Tokens.Hard != 250000 ||
		defaults.Elapsed.Warning != 8*time.Minute || defaults.Elapsed.Hard != 10*time.Minute {
		t.Fatalf("defaults = %+v", defaults)
	}
	warn, hard := int64(4), int64(5)
	override := &Override{Calls: &DimensionOverride{Warning: &warn, Hard: &hard}}
	resolved, err := Resolve(defaults, override)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Calls.Warning != 4 || resolved.Calls.Hard != 5 || resolved.Tokens != defaults.Tokens {
		t.Fatalf("resolved profile = %+v", resolved)
	}
}

func TestProfileRejectsUnboundedAndInvalidLimits(t *testing.T) {
	cases := []Profile{
		{Calls: stageusage.DimensionLimit{Warning: 0, Hard: 1}, Tokens: fixtureProfile().Tokens, Elapsed: fixtureProfile().Elapsed},
		{Calls: stageusage.DimensionLimit{Warning: 2, Hard: 2}, Tokens: fixtureProfile().Tokens, Elapsed: fixtureProfile().Elapsed},
		{Calls: stageusage.DimensionLimit{Warning: 1, Hard: math.MaxInt64}, Tokens: fixtureProfile().Tokens, Elapsed: fixtureProfile().Elapsed},
		{Calls: fixtureProfile().Calls, Tokens: fixtureProfile().Tokens, Elapsed: stageusage.ElapsedLimit{Warning: 0, Hard: time.Minute}},
		{Calls: fixtureProfile().Calls, Tokens: fixtureProfile().Tokens, Elapsed: stageusage.ElapsedLimit{Warning: 2 * time.Minute, Hard: time.Minute}},
	}
	for i, profile := range cases {
		if err := profile.Validate(); err == nil {
			t.Errorf("case %d unexpectedly validated: %+v", i, profile)
		}
	}
}

func TestLedgerPrioritizesAndReusesUnchangedSources(t *testing.T) {
	controller, _ := newFixtureController(t, fixtureProfile())
	controller.Add(Source{ID: "code.go", Priority: DirectCode, Fingerprint: "v1", Content: "package code"})
	controller.Add(Source{ID: "ISSUE.md", Priority: IssueArtifact, Fingerprint: "v1", Content: "issue"})
	controller.Add(Source{ID: "touchset:internal/**", Priority: TouchsetCandidate, Fingerprint: "v1", Content: "touchset"})
	first := controller.Next()
	if first.Source.ID != "ISSUE.md" {
		t.Fatalf("first source = %+v", first.Source)
	}
	if got := controller.Read(Source{ID: "ISSUE.md", Priority: IssueArtifact, Fingerprint: "v1"}); got.Source.Content != "issue" || got.Charged {
		t.Fatalf("cached read = %+v", got)
	}
	changed := controller.Read(Source{ID: "ISSUE.md", Priority: IssueArtifact, Fingerprint: "v2", Content: "changed issue"})
	if !changed.Charged || changed.Source.Content != "changed issue" {
		t.Fatalf("changed read = %+v", changed)
	}
	if controller.Snapshot().ChargedTokens != 1 {
		t.Fatalf("cached read charged unexpectedly: %+v", controller.Snapshot())
	}
}

func TestControllerStopsOnCallsTokensAndElapsed(t *testing.T) {
	calls, _ := newFixtureController(t, Profile{
		Calls:   stageusage.DimensionLimit{Warning: 1, Hard: 2},
		Tokens:  stageusage.DimensionLimit{Warning: 80, Hard: 100},
		Elapsed: stageusage.ElapsedLimit{Warning: time.Minute, Hard: 2 * time.Minute},
	})
	lease, _, err := calls.Admit("ISSUE.md", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := calls.Reconcile(lease, nil, nil); err != nil {
		t.Fatal(err)
	}
	lease, _, err = calls.Admit("STAGE.md", 1)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := calls.Reconcile(lease, nil, nil)
	if err != nil || snapshot.StopDimension != stageusage.DimensionCalls {
		t.Fatalf("call stop snapshot=%+v err=%v", snapshot, err)
	}

	tokens, _ := newFixtureController(t, Profile{
		Calls:   stageusage.DimensionLimit{Warning: 8, Hard: 10},
		Tokens:  stageusage.DimensionLimit{Warning: 4, Hard: 5},
		Elapsed: stageusage.ElapsedLimit{Warning: time.Minute, Hard: 2 * time.Minute},
	})
	lease, _, err = tokens.Admit("ISSUE.md", 5)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = tokens.Reconcile(lease, nil, nil)
	if err != nil || snapshot.StopDimension != stageusage.DimensionTokens {
		t.Fatalf("token stop snapshot=%+v err=%v", snapshot, err)
	}

	elapsed, clock := newFixtureController(t, Profile{
		Calls:   stageusage.DimensionLimit{Warning: 8, Hard: 10},
		Tokens:  stageusage.DimensionLimit{Warning: 80, Hard: 100},
		Elapsed: stageusage.ElapsedLimit{Warning: time.Second, Hard: 2 * time.Second},
	})
	lease, _, err = elapsed.Admit("ISSUE.md", 1)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Second)
	snapshot, err = elapsed.Reconcile(lease, nil, nil)
	if err != nil || snapshot.StopDimension != stageusage.DimensionElapsed {
		t.Fatalf("elapsed stop snapshot=%+v err=%v", snapshot, err)
	}
	if _, _, err := elapsed.Admit("code.go", 1); !errors.Is(err, stageusage.ErrAdmissionClosed) {
		t.Fatalf("admission after elapsed stop = %v", err)
	}
}

func TestControllerDistinguishesNormalAndBudgetLimitedOutcomes(t *testing.T) {
	normal, _ := newFixtureController(t, fixtureProfile())
	if got := normal.Finish(nil); got != OutcomeNormal {
		t.Fatalf("normal outcome = %v", got)
	}

	limited, _ := newFixtureController(t, Profile{
		Calls:   stageusage.DimensionLimit{Warning: 1, Hard: 2},
		Tokens:  stageusage.DimensionLimit{Warning: 4, Hard: 5},
		Elapsed: stageusage.ElapsedLimit{Warning: time.Minute, Hard: 2 * time.Minute},
	})
	lease, _, err := limited.Admit("ISSUE.md", 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limited.Reconcile(lease, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := limited.Finish(nil); got != OutcomeBudgetLimited {
		t.Fatalf("limited outcome = %v", got)
	}
}
