package levers

import (
	"testing"

	"github.com/weston6142/watchtower/internal/flow"
)

func TestRouteMatrix(t *testing.T) {
	cases := []struct {
		name  string
		d     Decision
		lever flow.Lever
		rules Rules
		want  bool
	}{
		{"strict always asks", Decision{Importance: 0.1}, flow.LeverStrict, Rules{}, true},
		{"yolo skips minor", Decision{Importance: 0.5}, flow.LeverYolo, Rules{}, false},
		{"yolo floor still asks", Decision{Importance: 1.0}, flow.LeverYolo, Rules{}, true},
		{"regular mid asks", Decision{Importance: 0.6}, flow.LeverRegular, Rules{}, true},
		{"regular minor skips", Decision{Importance: 0.2}, flow.LeverRegular, Rules{}, false},
		{"user rule overrides yolo", Decision{Importance: 0.1, Paths: []string{"payments/charge.go"}},
			flow.LeverYolo, Rules{AlwaysEscalate: []string{"payments/**"}}, true},
	}
	for _, c := range cases {
		if got := Route(c.d, c.lever, c.rules); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestPresetFillsAllStages(t *testing.T) {
	f := flow.Flow{Stages: []flow.Stage{{Name: "a"}, {Name: "b"}}}
	m := Preset(f, flow.LeverYolo)
	if m["a"] != flow.LeverYolo || m["b"] != flow.LeverYolo {
		t.Fatalf("preset wrong: %v", m)
	}
}

func TestRecommendedAnswerMatchesDecisionKind(t *testing.T) {
	choice := Decision{Kind: DecisionChoice, Recommended: 1}
	choiceAnswer := choice.RecommendedAnswer()
	if choiceAnswer.Kind != DecisionChoice || choiceAnswer.Option == nil || *choiceAnswer.Option != 1 {
		t.Fatalf("choice answer = %#v", choiceAnswer)
	}

	freeform := Decision{
		Kind:                DecisionFreeform,
		RecommendedResponse: "Approve spec.md as written.",
	}
	freeformAnswer := freeform.RecommendedAnswer()
	if freeformAnswer.Kind != DecisionFreeform ||
		freeformAnswer.Text != "Approve spec.md as written." ||
		freeformAnswer.Option != nil {
		t.Fatalf("freeform answer = %#v", freeformAnswer)
	}
}

func TestDecisionAcceptsFreeformForChoice(t *testing.T) {
	cases := []struct {
		name     string
		decision Decision
		response Response
		want     bool
	}{
		{name: "choice without legacy flag", decision: Decision{
			Kind: DecisionChoice, Options: []string{"approve", "hold"},
		}, response: FreeformResponse("Clarify the rollout."), want: true},
		{name: "choice with legacy flag false", decision: Decision{
			Kind: DecisionChoice, Options: []string{"approve", "hold"}, AllowFreeform: false,
		}, response: FreeformResponse("Clarify the rollout."), want: true},
		{name: "choice with legacy flag true", decision: Decision{
			Kind: DecisionChoice, Options: []string{"approve", "hold"}, AllowFreeform: true,
		}, response: FreeformResponse("Clarify the rollout."), want: true},
		{name: "legacy zero-value choice", decision: Decision{
			Options: []string{"approve", "hold"},
		}, response: FreeformResponse("Clarify the rollout."), want: true},
		{name: "freeform", decision: Decision{
			Kind: DecisionFreeform,
		}, response: FreeformResponse("Clarify the rollout."), want: true},
		{name: "empty text", decision: Decision{
			Kind: DecisionChoice, Options: []string{"approve"},
		}, response: FreeformResponse(""), want: false},
		{name: "invalid option", decision: Decision{
			Kind: DecisionChoice, Options: []string{"approve"},
		}, response: ChoiceResponse(1), want: false},
		{name: "nil option", decision: Decision{
			Kind: DecisionChoice, Options: []string{"approve"},
		}, response: Response{Kind: DecisionChoice}, want: false},
		{name: "unsupported response kind", decision: Decision{
			Kind: DecisionChoice, Options: []string{"approve"},
		}, response: Response{Kind: DecisionKind("unsupported"), Text: "Clarify"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.decision.Accepts(tc.response); got != tc.want {
				t.Fatalf("Accepts(%#v) = %v, want %v", tc.response, got, tc.want)
			}
		})
	}
}
