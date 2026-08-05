// Package decisionpage renders a self-contained HTML briefing for a pending
// watchtower decision, or a progress-only view when no decision is pending.
package decisionpage

import (
	"bytes"
	"embed"
	"html/template"
)

type FloorStatus string

const (
	FloorDone    FloorStatus = "done"
	FloorCurrent FloorStatus = "current"
	FloorPending FloorStatus = "pending"
	FloorFailed  FloorStatus = "failed"
)

type Floor struct {
	Name   string
	Status FloorStatus
	Note   string
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

type FileRow struct {
	Path     string
	Added    int
	Removed  int
	InBounds bool
}

type Briefing struct {
	Question       string
	AgentLabel     string
	Importance     float64
	Reversible     string
	Options        []Option
	Wins           []string
	DiagramSVG     template.HTML
	DiagramCaption string
	DiagramMissing bool
	Excerpts       []Excerpt
	OverrideNote   string
	NextAction     string
	EvidenceDocs   []string
}

type PageData struct {
	IssueID         string
	Title           string
	StageIndex      int
	StageTotal      int
	CurrentStage    string
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
}

//go:embed page.tmpl.html
var tmplFS embed.FS

var page = template.Must(template.ParseFS(tmplFS, "page.tmpl.html"))

func Render(d PageData) ([]byte, error) {
	var buf bytes.Buffer
	if err := page.Execute(&buf, d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
