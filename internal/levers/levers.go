package levers

import (
	"path"
	"strings"

	"github.com/weston6142/watchtower/internal/flow"
)

type Matrix map[string]flow.Lever

func Preset(f flow.Flow, l flow.Lever) Matrix {
	m := Matrix{}
	for _, st := range f.Stages {
		m[st.Name] = l
	}
	return m
}

type DecisionKind string

const (
	DecisionChoice   DecisionKind = "choice"
	DecisionFreeform DecisionKind = "freeform"
)

type Decision struct {
	Kind                DecisionKind
	Question            string
	Options             []string
	Recommended         int
	RecommendedResponse string
	// AllowFreeform is retained wire/storage compatibility metadata. It is not
	// a current capability switch for decision responses.
	AllowFreeform bool
	Importance    float64
	Paths         []string
	Why           string
	Consequences  []string
	Reversible    string
}

type Response struct {
	Kind   DecisionKind `json:"kind"`
	Option *int         `json:"option,omitempty"`
	Text   string       `json:"text,omitempty"`
}

func ChoiceResponse(option int) Response {
	return Response{Kind: DecisionChoice, Option: &option}
}

func FreeformResponse(text string) Response {
	return Response{Kind: DecisionFreeform, Text: text}
}

func (d Decision) RecommendedAnswer() Response {
	if d.Kind == DecisionFreeform {
		return FreeformResponse(d.RecommendedResponse)
	}
	return ChoiceResponse(d.Recommended)
}

func (d Decision) Accepts(response Response) bool {
	if response.Kind == DecisionFreeform {
		return response.Text != "" && (d.Kind == "" || d.Kind == DecisionChoice || d.Kind == DecisionFreeform)
	}
	if response.Kind != DecisionChoice || response.Option == nil {
		return false
	}
	return *response.Option >= 0 && *response.Option < len(d.Options)
}

type Rules struct {
	AlwaysEscalate []string
}

// Importance thresholds for routing decisions to a human.
const (
	// escalationFloor: decisions at or above this always escalate,
	// regardless of lever.
	escalationFloor = 1.0
	// regularThreshold: minimum importance escalated under the regular lever.
	regularThreshold = 0.5
	// yoloThreshold: minimum importance escalated under the yolo lever.
	yoloThreshold = 0.9
)

func matches(pattern, p string) bool {
	if strings.HasSuffix(pattern, "/**") {
		return strings.HasPrefix(p, strings.TrimSuffix(pattern, "**"))
	}
	ok, _ := path.Match(pattern, p)
	return ok
}

func Route(d Decision, lever flow.Lever, rules Rules) bool {
	if d.Importance >= escalationFloor {
		return true
	}
	for _, pat := range rules.AlwaysEscalate {
		for _, p := range d.Paths {
			if matches(pat, p) {
				return true
			}
		}
	}
	switch lever {
	case flow.LeverStrict:
		return true
	case flow.LeverRegular:
		return d.Importance >= regularThreshold
	default: // yolo
		return d.Importance >= yoloThreshold
	}
}
