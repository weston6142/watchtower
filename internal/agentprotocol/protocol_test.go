package agentprotocol

import (
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/levers"
)

func TestTaskMessagePointsToCompactStageBrief(t *testing.T) {
	message := TaskMessage("spec", "GH-1")
	if !strings.Contains(message, "STAGE.md") || !strings.Contains(message, "ISSUE.md") {
		t.Fatalf("task message: %q", message)
	}
}

func TestCoachMessageExplainsRequiredDecisionFields(t *testing.T) {
	for _, field := range []string{"why", "consequences"} {
		if !strings.Contains(CoachMessage, field) {
			t.Fatalf("coach message does not mention %q: %q", field, CoachMessage)
		}
	}
}

func TestExtractDecision(t *testing.T) {
	text := "I need input.\n{\"watchtower_decision\": {\"question\": \"REST or GraphQL?\", \"options\": [\"REST\", \"GraphQL\"], \"recommended\": 0, \"importance\": 0.6, \"paths\": [\"api/routes.go\"]}}\n"
	d, ok := ExtractDecision(text)
	if !ok || d.Question != "REST or GraphQL?" || len(d.Options) != 2 || d.Importance != 0.6 || d.Paths[0] != "api/routes.go" {
		t.Fatalf("decision: %+v ok=%v", d, ok)
	}
	if _, ok := ExtractDecision("no marker here"); ok {
		t.Fatal("false positive")
	}
}

func TestExtractDecisionV2Fields(t *testing.T) {
	text := `{"watchtower_decision": {"question": "Q?", "options": ["a","b"], "recommended": 1, "importance": 0.5, "paths": [], "why": "b is safer", "consequences": ["fast but risky", "slower, safe"], "reversible": "until execute"}}`
	d, ok := ExtractDecision(text)
	if !ok || d.Kind != levers.DecisionChoice || d.Why != "b is safer" ||
		len(d.Consequences) != 2 || d.Reversible != "until execute" {
		t.Fatalf("v2 fields: %+v ok=%v", d, ok)
	}
}

func TestExtractChoiceDecisionPreservesLegacyFreeformFlag(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		allow bool
	}{
		{name: "omitted"},
		{name: "false", field: `,"allow_freeform":false`},
		{name: "true", field: `,"allow_freeform":true`, allow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := `{"watchtower_decision":{"kind":"choice","question":"Fix it?","options":["Apply fix","Hold"],"recommended":0` + tc.field + `,"importance":0.8,"why":"The fix is scoped.","consequences":["Tests rerun.","Branch is preserved."],"reversible":"yes"}}`
			d, ok := ExtractDecision(text)
			if !ok || d.Kind != levers.DecisionChoice || len(d.Options) != 2 || d.Options[0] != "Apply fix" || d.AllowFreeform != tc.allow {
				t.Fatalf("decision = %#v, %v", d, ok)
			}
		})
	}
}

func TestExtractFreeformDecision(t *testing.T) {
	text := `{"watchtower_decision":{"kind":"freeform","question":"Review spec.md","recommended_response":"Approve spec.md as written.","importance":0.8,"why":"It matches the design.","consequences":["Planning begins."],"reversible":"yes"}}`
	d, ok := ExtractDecision(text)
	if !ok || d.Kind != levers.DecisionFreeform ||
		d.RecommendedResponse != "Approve spec.md as written." {
		t.Fatalf("decision = %#v, %v", d, ok)
	}
}

func TestExtractDecisionRejectsInvalidKindPayload(t *testing.T) {
	cases := []string{
		`{"watchtower_decision":{"kind":"choice","question":"Q?","recommended":0}}`,
		`{"watchtower_decision":{"kind":"choice","question":"Q?","options":["a"],"recommended":1}}`,
		`{"watchtower_decision":{"kind":"freeform","question":"Q?"}}`,
		`{"watchtower_decision":{"kind":"unknown","question":"Q?","options":["a"]}}`,
	}
	for _, text := range cases {
		if d, ok := ExtractDecision(text); ok {
			t.Errorf("ExtractDecision(%q) = %#v, true", text, d)
		}
	}
}

func TestExtractProposal(t *testing.T) {
	text := "found something\n{\"watchtower_proposal\": {\"title\": \"Refactor refunds\", \"body\": \"3 call sites entangled\", \"depends_on\": [\" GH-1 \", \"GH-2\"]}}"
	p, ok := ExtractProposal(text)
	if !ok || p.Title != "Refactor refunds" || p.Body != "3 call sites entangled" ||
		len(p.DependsOn) != 2 || p.DependsOn[0] != "GH-1" || p.DependsOn[1] != "GH-2" {
		t.Fatalf("proposal: %+v ok=%v", p, ok)
	}
	if _, ok := ExtractProposal("nothing"); ok {
		t.Fatal("false positive")
	}
}

func TestExtractProposalBatch(t *testing.T) {
	text := `{"watchtower_proposal_batch":{"tasks":[` +
		`{"key":"api","title":"Add API","body":"endpoint","depends_on":[]},` +
		`{"key":"consumer","title":"Use API","body":"client","depends_on":["api"]}` +
		`]}}`
	batch, ok := ExtractProposalBatch(text)
	if !ok || len(batch) != 2 || batch[0].Key != "api" ||
		batch[1].Key != "consumer" || len(batch[1].DependsOn) != 1 ||
		batch[1].DependsOn[0] != "api" {
		t.Fatalf("batch: %+v ok=%v", batch, ok)
	}
}

func TestExtractDependency(t *testing.T) {
	dependsOn, ok := ExtractDependency(
		`{"watchtower_dependency":{"depends_on":[" GH-1 ","GH-2","GH-1"]}}`)
	if !ok || len(dependsOn) != 2 || dependsOn[0] != "GH-1" || dependsOn[1] != "GH-2" {
		t.Fatalf("dependency: %#v ok=%v", dependsOn, ok)
	}
}

func TestExtractDecisionLegacyKey(t *testing.T) {
	text := `{"guildhall_decision": {"question": "Q?", "options": ["a","b"], "recommended": 1, "importance": 0.5, "paths": ["api/routes.go"], "why": "b is safer", "consequences": ["fast but risky", "slower, safe"], "reversible": "until execute"}}`
	d, ok := ExtractDecision(text)
	if !ok || d.Question != "Q?" || len(d.Options) != 2 || d.Recommended != 1 ||
		d.Importance != 0.5 || d.Paths[0] != "api/routes.go" || d.Why != "b is safer" ||
		len(d.Consequences) != 2 || d.Reversible != "until execute" {
		t.Fatalf("legacy decision: %+v ok=%v", d, ok)
	}
}

func TestExtractProposalLegacyKey(t *testing.T) {
	text := "found something\n{\"guildhall_proposal\": {\"title\": \"Refactor refunds\", \"body\": \"3 call sites entangled\"}}"
	p, ok := ExtractProposal(text)
	if !ok || p.Title != "Refactor refunds" || p.Body != "3 call sites entangled" {
		t.Fatalf("legacy proposal: %+v ok=%v", p, ok)
	}
}

func TestExtractMarkerNewKeyWinsOverLegacy(t *testing.T) {
	text := `{"watchtower_decision": {"question": "new?", "options": ["a","b"]}, "guildhall_decision": {"question": "old?", "options": ["c","d"]}}`
	d, ok := ExtractDecision(text)
	if !ok || d.Question != "new?" || d.Options[0] != "a" {
		t.Fatalf("precedence: %+v ok=%v", d, ok)
	}

	ptext := `{"watchtower_proposal": {"title": "new", "body": "n"}, "guildhall_proposal": {"title": "old", "body": "o"}}`
	p, ok := ExtractProposal(ptext)
	if !ok || p.Title != "new" || p.Body != "n" {
		t.Fatalf("proposal precedence: %+v ok=%v", p, ok)
	}
}
