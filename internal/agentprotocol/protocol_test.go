package agentprotocol

import (
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/stageresult"
)

func TestExtractStageResultEvidence(t *testing.T) {
	text := "finished\n" + validExecuteStageResultMarker()
	evidence, found, err := ExtractStageResult(text)
	if err != nil || !found {
		t.Fatalf("ExtractStageResult() = (%#v, %v, %v)", evidence, found, err)
	}
	if evidence.StageKind != stageresult.KindExecute || evidence.Execute == nil {
		t.Fatalf("evidence = %#v", evidence)
	}
	if got := evidence.Execute.PlanTasks[0].ID; got != "task-0001" {
		t.Fatalf("task ID = %q", got)
	}
	if got := []stageresult.CheckResult{evidence.Execute.Checks[0].Result, evidence.Execute.Checks[1].Result}; got[0] != stageresult.CheckRed || got[1] != stageresult.CheckGreen {
		t.Fatalf("check results = %#v", got)
	}
	if got := evidence.Execute.Skips[0].Explanation; got != "the final repository gate belongs to merge verification" {
		t.Fatalf("skip explanation = %q", got)
	}
}

func TestExtractStageResultDistinguishesMalformedAndMissingMarkers(t *testing.T) {
	valid := validExecuteStageResultMarker()
	tests := []struct {
		name      string
		text      string
		wantFound bool
		wantErr   bool
	}{
		{name: "invalid JSON", text: `{"watchtower_stage_result":`, wantFound: true, wantErr: true},
		{name: "unknown top-level field", text: strings.TrimSuffix(valid, "}") + `,"extra":true}`, wantFound: true, wantErr: true},
		{name: "unknown evidence field", text: strings.Replace(valid, `"schema_version":1`, `"schema_version":1,"extra":true`, 1), wantFound: true, wantErr: true},
		{name: "missing marker value", text: `{"watchtower_stage_result":null}`, wantFound: true, wantErr: true},
		{name: "no marker", text: "ordinary final prose"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, found, err := ExtractStageResult(tt.text)
			if found != tt.wantFound || (err != nil) != tt.wantErr {
				t.Fatalf("ExtractStageResult() found=%v err=%v", found, err)
			}
			if tt.wantErr && strings.TrimSpace(err.Error()) == "" {
				t.Fatal("decode error is blank")
			}
		})
	}
}

func TestExtractStageResultRejectsConflictingMarkersInOneMessage(t *testing.T) {
	first := validExecuteStageResultMarker()
	second := strings.Replace(first, `"task-0001"`, `"task-0002"`, 1)

	_, found, err := ExtractStageResult(first + "\n" + second)
	if !found || err == nil {
		t.Fatalf("ExtractStageResult() found=%v err=%v, want a conflicting-marker error", found, err)
	}
}

func validExecuteStageResultMarker() string {
	return `{"watchtower_stage_result":{"schema_version":1,"stage_kind":"execute","outcome":"completed","remaining_work":[],"remaining_concerns":[],"execute":{"plan_tasks":[{"id":"task-0001","outcome":"completed","summary":"implemented"}],"commits":[{"sha":"abc123","message":"feat: implement task","task_ids":["task-0001"]}],"checks":[{"name":"focused red","command":"go test ./internal/example -run TestBehavior","result":"red","affected":true},{"name":"focused green","command":"go test ./internal/example -run TestBehavior","result":"green","affected":true}],"skips":[{"activity":"repository gate","explanation":"the final repository gate belongs to merge verification"}]}}}`
}

func TestTaskMessagePointsToCompactStageBrief(t *testing.T) {
	message := TaskMessage("spec", "GH-1")
	if !strings.Contains(message, "STAGE.md") || !strings.Contains(message, "ISSUE.md") {
		t.Fatalf("task message: %q", message)
	}
}

func TestCoachMessageExplainsRequiredDecisionFields(t *testing.T) {
	for _, field := range []string{"why", "consequences", "reversible", "proof", "claim", "cite"} {
		if !strings.Contains(CoachMessage, field) {
			t.Fatalf("coach message does not mention %q: %q", field, CoachMessage)
		}
	}
}

func TestUnstructuredDecisionRequestNeedsCoaching(t *testing.T) {
	text := "I found an architecture mismatch. Reply with `Human decision: choose one`, and I'll continue."
	if !UnstructuredDecisionRequestNeedsCoaching(text) {
		t.Fatalf("unstructured decision request was not detected: %q", text)
	}
	if UnstructuredDecisionRequestNeedsCoaching("The accepted Human decision: keep the existing behavior.") {
		t.Fatal("an accepted decision reply was mistaken for a new decision request")
	}
	if UnstructuredDecisionRequestNeedsCoaching(`{"watchtower_decision":{"kind":"choice","question":"Choose?","options":["A","B"],"recommended":0}}`) {
		t.Fatal("a structured decision marker was mistaken for an unstructured request")
	}
}

func TestDecisionNeedsCoaching(t *testing.T) {
	validChoice := levers.Decision{
		Kind: levers.DecisionChoice, Question: "Ship it?",
		Options: []string{"Ship", "Hold"}, Recommended: 0,
		Why:          "The verified change is ready.",
		Consequences: []string{"The next stage starts.", "The current stage remains blocked."},
		Reversible:   "The deployment can be rolled back.",
		Briefing: &levers.Briefing{Proof: []levers.BriefingProof{{
			Claim: "Focused tests pass.", Cite: "go test ./internal/decisionpage",
		}}},
	}
	tests := []struct {
		name string
		edit func(*levers.Decision)
		want bool
	}{
		{name: "complete", edit: func(*levers.Decision) {}, want: false},
		{name: "blank why", edit: func(d *levers.Decision) { d.Why = "  " }, want: true},
		{name: "blank reversible", edit: func(d *levers.Decision) { d.Reversible = "  " }, want: true},
		{name: "wrong consequence count", edit: func(d *levers.Decision) { d.Consequences = d.Consequences[:1] }, want: true},
		{name: "blank consequence", edit: func(d *levers.Decision) { d.Consequences[0] = "" }, want: true},
		{name: "blank proof claim", edit: func(d *levers.Decision) { d.Briefing.Proof[0].Claim = "" }, want: true},
		{name: "blank proof citation", edit: func(d *levers.Decision) { d.Briefing.Proof[0].Cite = "" }, want: true},
		{name: "uncited excerpt", edit: func(d *levers.Decision) {
			d.Briefing.Excerpts = []levers.BriefingExcerpt{{Text: "Approved requirement."}}
		}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := validChoice
			d.Options = append([]string(nil), validChoice.Options...)
			d.Consequences = append([]string(nil), validChoice.Consequences...)
			briefing := *validChoice.Briefing
			briefing.Proof = append([]levers.BriefingProof(nil), validChoice.Briefing.Proof...)
			d.Briefing = &briefing
			tt.edit(&d)
			if got := DecisionNeedsCoaching(d); got != tt.want {
				t.Fatalf("DecisionNeedsCoaching() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractDecisionPreservesStructuredProof(t *testing.T) {
	text := `{"watchtower_decision":{"kind":"choice","question":"Ship?","options":["Ship","Hold"],"recommended":0,"why":"Ready.","consequences":["Advances.","Waits."],"briefing":{"proof":[{"claim":"Tests pass.","cite":"go test ./..."}]}}}`
	d, ok := ExtractDecision(text)
	if !ok || d.Briefing == nil || len(d.Briefing.Proof) != 1 ||
		d.Briefing.Proof[0].Claim != "Tests pass." || d.Briefing.Proof[0].Cite != "go test ./..." {
		t.Fatalf("structured proof was not preserved: %#v, ok=%v", d, ok)
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

func TestExtractDecisionBriefing(t *testing.T) {
	text := "preamble\n" + `{"watchtower_decision":{"kind":"choice","question":"Gate it?","options":["Gate","Apply"],"recommended":0,"consequences":["safe","crash"],"briefing":{"option_details":["Old daemons ignore the column.","Old daemons crash."],"wins":["migration written"],"excerpts":[{"text":"must be invisible to N−1","cite":"spec.md §2.1"}],"override_note":"Option 2 overrides the spec.","next_action":"Press 1.","diagram_svg":"<svg viewBox=\"0 0 10 10\"></svg>","diagram_caption":"the gate"}}}`
	d, ok := ExtractDecision(text)
	if !ok {
		t.Fatal("decision not extracted")
	}
	if d.Briefing == nil {
		t.Fatal("briefing not extracted")
	}
	if len(d.Briefing.OptionDetails) != 2 || d.Briefing.Excerpts[0].Cite != "spec.md §2.1" {
		t.Fatalf("briefing mis-parsed: %+v", d.Briefing)
	}
	if d.Briefing.NextAction != "Press 1." || d.Briefing.DiagramCaption != "the gate" {
		t.Fatalf("briefing details mis-parsed: %+v", d.Briefing)
	}
	if len(d.Briefing.Wins) != 0 {
		t.Fatalf("new decision preserved deprecated wins: %+v", d.Briefing)
	}
}

func TestExtractDecisionNoBriefing(t *testing.T) {
	text := `{"watchtower_decision":{"kind":"choice","question":"Q","options":["a","b"],"consequences":["x","y"]}}`
	d, ok := ExtractDecision(text)
	if !ok || d.Briefing != nil {
		t.Fatalf("want ok with nil briefing, got ok=%v briefing=%+v", ok, d.Briefing)
	}
}

func TestExtractDecisionBriefingClampsLists(t *testing.T) {
	text := `{"watchtower_decision":{"kind":"choice","question":"Q","options":["a","b"],"recommended":0,"briefing":{"option_details":["only one"],"wins":["w1","w2","w3","w4","w5","w6"],"proof":[{"claim":"1"},{"claim":"2"},{"claim":"3"},{"claim":"4"},{"claim":"5"},{"claim":"6"}],"excerpts":[{"text":"1"},{"text":"2"},{"text":"3"},{"text":"4"}]}}}`
	d, ok := ExtractDecision(text)
	if !ok || d.Briefing == nil {
		t.Fatalf("decision = %#v, ok=%v", d, ok)
	}
	if len(d.Briefing.OptionDetails) != 0 || len(d.Briefing.Wins) != 0 ||
		len(d.Briefing.Proof) != 5 || len(d.Briefing.Excerpts) != 3 {
		t.Fatalf("briefing limits not applied: %+v", d.Briefing)
	}
}

func TestExtractLegacyDecisionPreservesBriefingWins(t *testing.T) {
	text := `{"guildhall_decision":{"kind":"choice","question":"Q","options":["a","b"],"recommended":0,"briefing":{"wins":["w1","w2","w3","w4","w5","w6"]}}}`
	d, ok := ExtractDecision(text)
	if !ok || d.Briefing == nil || len(d.Briefing.Wins) != levers.MaxBriefingWins {
		t.Fatalf("legacy briefing wins = %+v, ok=%v", d.Briefing, ok)
	}
}

func TestExtractDecisionIgnoresAgentSuppliedContext(t *testing.T) {
	text := `{"watchtower_decision":{"kind":"choice","question":"Proceed?","options":["yes","no"],"recommended":0,"context":{"task_summary":"untrusted","agent_name":"spoof","agent_color":"red","agent_symbol":"!"}}}`
	d, ok := ExtractDecision(text)
	if !ok || d.Question != "Proceed?" || d.Options[0] != "yes" {
		t.Fatalf("decision = %#v, ok = %v", d, ok)
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
