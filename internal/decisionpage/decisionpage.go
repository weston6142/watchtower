// Package decisionpage renders a self-contained HTML briefing for a pending or
// resolved watchtower decision, or a progress-only view when no decision is
// selected.
package decisionpage

import (
	"bytes"
	"embed"
	"html/template"
)

// FileName is the on-disk name of the rendered page inside an issue's data
// directory; it is also the artifact name emitted when the page is written.
const FileName = "decision.html"

type FloorStatus string

const (
	FloorDone    FloorStatus = "done"
	FloorCurrent FloorStatus = "current"
	FloorPending FloorStatus = "pending"
	FloorFailed  FloorStatus = "failed"
)

// FloorLink is a relative link to an archived stage artifact
// (e.g. "artifacts/spec.md"), resolvable from the page's own directory.
type FloorLink struct {
	Name string
	Href string
}

// FloorDecision summarizes a decision made at a completed stage, with an
// optional relative link to its archived briefing page.
type FloorDecision struct {
	Question string
	Answer   string
	Href     string
}

type Floor struct {
	Name      string
	Status    FloorStatus
	Note      string
	Artifacts []FloorLink
	Decisions []FloorDecision
}

// Expandable reports whether a completed floor has archived context worth
// rendering inside a <details> disclosure.
func (f Floor) Expandable() bool {
	return len(f.Artifacts) > 0 || len(f.Decisions) > 0
}

type Option struct {
	Key         string
	Label       string
	OneLiner    string
	Recommended bool
}

type Excerpt struct {
	Text string
	Cite string
}

// Proof pairs one verified result with the source that demonstrates it.
type Proof struct {
	Claim string
	Cite  string
}

type FileRow struct {
	Path     string
	Added    int
	Removed  int
	InBounds bool
}

// Briefing is the normalized actionable breakdown rendered for one decision.
// Missing proof or a rejected diagram is represented explicitly instead of
// being inferred from other prose.
type Briefing struct {
	Question          string
	AgentLabel        string
	Importance        float64
	Reversible        string
	Action            string
	Recommendation    string
	RecommendationWhy string
	Options           []Option
	Proof             []Proof
	ProofMissing      bool
	AfterAnswer       string
	DiagramSVG        template.HTML
	DiagramCaption    string
	DiagramMissing    bool
	Excerpts          []Excerpt
	OverrideNote      string
	EvidenceDocs      []string
}

// PageData is the complete decision-page render model. DecisionStage pins an
// archived briefing to the floor that raised it even after CurrentStage moves;
// PolicyApproved and AutoResolved distinguish resolved outcomes.
type PageData struct {
	IssueID         string
	Title           string
	StageIndex      int
	StageTotal      int
	CurrentStage    string
	DecisionStage   string
	DoneCount       int
	BlockedFor      string
	HeldSlots       string
	Floors          []Floor
	Briefing        *Briefing
	TouchsetGlobs   []string
	Files           []FileRow
	TouchsetMissing bool
	EvidenceMissing bool
	Answered        string
	PolicyApproved  bool
	AutoResolved    bool
}

//go:embed page.tmpl.html
var tmplFS embed.FS

var page = template.Must(template.ParseFS(tmplFS, "page.tmpl.html"))

// Render executes the embedded page template. A briefing without an explicit
// decision stage is placed on the current stage for legacy snapshots.
func Render(d PageData) ([]byte, error) {
	if d.Briefing != nil && d.DecisionStage == "" {
		d.DecisionStage = d.CurrentStage
	}
	var buf bytes.Buffer
	if err := page.Execute(&buf, d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
