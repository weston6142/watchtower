package decisionpage

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("golden missing (run with -update): %v", err)
	}
	if string(want) != string(got) {
		t.Errorf("golden mismatch for %s", name)
	}
}

func fixtureBriefing() *Briefing {
	return &Briefing{
		Question:   "Apply the schema migration now, or gate it behind a version check?",
		AgentLabel: "⚙ builder-agent",
		Importance: 0.8,
		Reversible: "reversible",
		Options: []Option{
			{Key: "1", Label: "Gate behind version check", OneLiner: "Old daemons ignore the new column.", Recommended: true},
			{Key: "2", Label: "Apply immediately", OneLiner: "Daemons older than 0.9 crash on restart."},
			{Key: "f", Label: "Freeform", OneLiner: "Type your own instruction back to the agent."},
		},
		Wins:         []string{"Migration written — migrate.go +96", "Engine reads it — tests pass (14/14)"},
		Excerpts:     []Excerpt{{Text: "Any schema change must be invisible to version N−1.", Cite: `spec.md §2.1 "Compatibility contract"`}},
		OverrideNote: "Option 2 contradicts spec.md §2.1 — picking it overrides the spec.",
		NextAction:   "Press 1 in the TUI to accept the gate, or 2 to override the spec.",
		EvidenceDocs: []string{"plan.md", "spec.md", "diff.patch"},
	}
}

func fixturePage(b *Briefing) PageData {
	return PageData{
		IssueID: "GH-42", Title: "Live config migration",
		StageIndex: 4, StageTotal: 8, CurrentStage: "execute", DoneCount: 3,
		BlockedFor: "6 min", HeldSlots: "holding 1 heavy slot",
		Floors: []Floor{
			{Name: "brainstorm", Status: FloorDone, Note: "brainstorm.md · 12.4k tok",
				Artifacts: []FloorLink{{Name: "brainstorm.md", Href: "artifacts/brainstorm.md"}},
				Decisions: []FloorDecision{{Question: "Approve this brainstorm design?", Answer: "option 1", Href: "decisions/198.html"}}},
			{Name: "spec", Status: FloorDone, Note: "spec.md · 8.1k tok",
				Artifacts: []FloorLink{{Name: "spec.md", Href: "artifacts/spec.md"}}},
			{Name: "plan", Status: FloorDone, Note: "plan.md, touchset.json · 15.7k tok",
				Artifacts: []FloorLink{
					{Name: "plan.md", Href: "artifacts/plan.md"},
					{Name: "touchset.json", Href: "artifacts/touchset.json"}},
				Decisions: []FloorDecision{{Question: "Ship the plan as written?", Answer: "auto: option 1"}}},
			{Name: "execute", Status: FloorCurrent},
			{Name: "correctness-review", Status: FloorPending, Note: "will review the same touchset"},
			{Name: "merge-verification", Status: FloorPending, Note: "merge barrier"},
		},
		Briefing:      b,
		TouchsetGlobs: []string{"internal/engine/**", "internal/store/**"},
		Files: []FileRow{
			{Path: "internal/engine/engine.go", Added: 142, Removed: 18, InBounds: true},
			{Path: "internal/store/migrate.go", Added: 96, InBounds: true},
		},
	}
}

func TestRenderDecision(t *testing.T) {
	got, err := Render(fixturePage(fixtureBriefing()))
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "decision", got)
}

func TestRenderProgressOnly(t *testing.T) {
	d := fixturePage(nil)
	d.BlockedFor, d.HeldSlots = "", ""
	got, err := Render(d)
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "progress", got)
}

func TestRenderMissingEvidence(t *testing.T) {
	d := fixturePage(fixtureBriefing())
	d.Files, d.TouchsetGlobs = nil, nil
	d.TouchsetMissing, d.EvidenceMissing = true, true
	got, err := Render(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "not produced by this flow") {
		t.Error("missing sections must say so explicitly")
	}
	checkGolden(t, "missing-evidence", got)
}

func TestEscaping(t *testing.T) {
	b := fixtureBriefing()
	b.Question = `<script>alert(1)</script> & "quotes"`
	b.Options[0].Label = `<img src=x onerror=alert(1)>`
	got, err := Render(fixturePage(b))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if strings.Contains(s, "<script>alert") || strings.Contains(s, "<img src=x") {
		t.Fatal("unescaped hostile input in output")
	}
}
