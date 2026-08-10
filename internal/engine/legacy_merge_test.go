package engine

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/store"
)

func TestFoldLegacyMergeEvidence(t *testing.T) {
	validEvents := func(t *testing.T) []core.Event {
		t.Helper()
		return orderedLegacyEvents(t,
			legacyEventSpec{typ: core.EvIssueMerged, payload: map[string]any{
				"commit": "landed-a", "branch": "main",
			}},
			legacyEventSpec{typ: core.EvIssueCompleted, payload: map[string]any{}},
		)
	}

	cases := []struct {
		name   string
		issue  store.IssueRow
		events func(*testing.T) []core.Event
		want   legacyMergeEvidence
		valid  bool
	}{
		{
			name:   "valid",
			issue:  store.IssueRow{ID: "GH-101", State: "done"},
			events: validEvents,
			want:   legacyMergeEvidence{IssueID: "GH-101", BaseBranch: "main", LandedSHA: "landed-a"},
			valid:  true,
		},
		{
			name:  "missing issue merged",
			issue: store.IssueRow{ID: "GH-101", State: "done"},
			events: func(t *testing.T) []core.Event {
				return orderedLegacyEvents(t, legacyEventSpec{typ: core.EvIssueCompleted, payload: map[string]any{}})
			},
		},
		{
			name:  "missing issue completed",
			issue: store.IssueRow{ID: "GH-101", State: "done"},
			events: func(t *testing.T) []core.Event {
				return orderedLegacyEvents(t, legacyEventSpec{typ: core.EvIssueMerged, payload: map[string]any{
					"commit": "landed-a", "branch": "main",
				}})
			},
		},
		{
			name:  "missing commit",
			issue: store.IssueRow{ID: "GH-101", State: "done"},
			events: func(t *testing.T) []core.Event {
				return orderedLegacyEvents(t,
					legacyEventSpec{typ: core.EvIssueMerged, payload: map[string]any{"branch": "main"}},
					legacyEventSpec{typ: core.EvIssueCompleted, payload: map[string]any{}},
				)
			},
		},
		{
			name:  "left unmerged",
			issue: store.IssueRow{ID: "GH-101", State: "done (unmerged)"},
			events: func(t *testing.T) []core.Event {
				return orderedLegacyEvents(t,
					legacyEventSpec{typ: core.EvIssueMerged, payload: map[string]any{
						"commit": "landed-a", "branch": "main",
					}},
					legacyEventSpec{typ: core.EvIssueCompleted, payload: map[string]any{"merge": "left-unmerged"}},
				)
			},
		},
		{
			name:  "conflicting commits",
			issue: store.IssueRow{ID: "GH-101", State: "done"},
			events: func(t *testing.T) []core.Event {
				return orderedLegacyEvents(t,
					legacyEventSpec{typ: core.EvIssueMerged, payload: map[string]any{
						"commit": "landed-a", "branch": "main",
					}},
					legacyEventSpec{typ: core.EvPublishSucceeded, payload: map[string]any{
						"commit": "landed-b", "branch": "main",
					}},
					legacyEventSpec{typ: core.EvIssueCompleted, payload: map[string]any{}},
				)
			},
		},
		{
			name:  "conflicting base branches",
			issue: store.IssueRow{ID: "GH-101", State: "done"},
			events: func(t *testing.T) []core.Event {
				return orderedLegacyEvents(t,
					legacyEventSpec{typ: core.EvIssueMerged, payload: map[string]any{
						"commit": "landed-a", "base_branch": "main",
					}},
					legacyEventSpec{typ: core.EvPublishSucceeded, payload: map[string]any{
						"commit": "landed-a", "branch": "develop",
					}},
					legacyEventSpec{typ: core.EvIssueCompleted, payload: map[string]any{}},
				)
			},
		},
		{
			name:  "reversed lifecycle order",
			issue: store.IssueRow{ID: "GH-101", State: "done"},
			events: func(t *testing.T) []core.Event {
				return orderedLegacyEvents(t,
					legacyEventSpec{typ: core.EvIssueCompleted, payload: map[string]any{}},
					legacyEventSpec{typ: core.EvIssueMerged, payload: map[string]any{
						"commit": "landed-a", "branch": "main",
					}},
				)
			},
		},
		{
			name:  "malformed relevant payload",
			issue: store.IssueRow{ID: "GH-101", State: "done"},
			events: func(t *testing.T) []core.Event {
				return orderedLegacyEvents(t,
					legacyEventSpec{typ: core.EvIssueMerged, raw: json.RawMessage(`{"commit":`)},
					legacyEventSpec{typ: core.EvIssueCompleted, payload: map[string]any{}},
				)
			},
		},
		{
			name:   "abandoned issue",
			issue:  store.IssueRow{ID: "GH-101", State: "abandoned"},
			events: validEvents,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := foldLegacyMergeEvidence(tc.issue, tc.events(t), "main")
			if tc.valid {
				if err != nil {
					t.Fatalf("foldLegacyMergeEvidence() error = %v", err)
				}
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("foldLegacyMergeEvidence() = %+v, want %+v", got, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("foldLegacyMergeEvidence() succeeded with %+v", got)
			}
			var rejection *legacyEvidenceError
			if !asLegacyEvidenceError(err, &rejection) {
				t.Fatalf("foldLegacyMergeEvidence() error = %T %v, want candidate rejection", err, err)
			}
		})
	}
}

type legacyEventSpec struct {
	typ     core.EventType
	payload any
	raw     json.RawMessage
}

func orderedLegacyEvents(t *testing.T, specs ...legacyEventSpec) []core.Event {
	t.Helper()
	events := make([]core.Event, 0, len(specs))
	for i, spec := range specs {
		var event core.Event
		if spec.raw != nil {
			event = core.Event{Type: spec.typ, IssueID: "GH-101", Payload: spec.raw}
		} else {
			var err error
			event, err = core.NewEvent(spec.typ, "GH-101", spec.payload)
			if err != nil {
				t.Fatal(err)
			}
		}
		event.Seq = int64(i + 1)
		events = append(events, event)
	}
	return events
}

func asLegacyEvidenceError(err error, target **legacyEvidenceError) bool {
	if err == nil {
		return false
	}
	value, ok := err.(*legacyEvidenceError)
	if !ok {
		return false
	}
	*target = value
	return true
}
