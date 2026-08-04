package plannerbudget

import (
	"sort"

	"github.com/weston6142/watchtower/internal/stageusage"
)

type SourcePriority int

const (
	IssueArtifact SourcePriority = iota
	TouchsetCandidate
	DirectCode
	BroadSource
)

type Source struct {
	ID          string
	Priority    SourcePriority
	Fingerprint string
	Content     string
	Reservation int64
}

type Candidate struct {
	Source Source
}

type ReadResult struct {
	Source   Source
	Content  string
	Charged  bool
	Snapshot stageusage.Snapshot
	Err      error
}

type Ledger struct {
	sources  []Source
	observed map[string]map[string]Source
	position int
}

func NewLedger() *Ledger {
	return &Ledger{observed: make(map[string]map[string]Source)}
}

func (l *Ledger) Add(source Source) {
	l.sources = append(l.sources, source)
	fingerprints := l.observed[source.ID]
	if fingerprints == nil {
		fingerprints = make(map[string]Source)
		l.observed[source.ID] = fingerprints
	}
	if _, exists := fingerprints[source.Fingerprint]; !exists {
		fingerprints[source.Fingerprint] = source
	}
	sort.SliceStable(l.sources, func(i, j int) bool {
		if l.sources[i].Priority != l.sources[j].Priority {
			return l.sources[i].Priority < l.sources[j].Priority
		}
		return l.sources[i].ID < l.sources[j].ID
	})
}

func (l *Ledger) Next() Candidate {
	if l.position >= len(l.sources) {
		return Candidate{}
	}
	source := l.sources[l.position]
	l.position++
	return Candidate{Source: source}
}

func (l *Ledger) cached(source Source) (Source, bool) {
	fingerprints := l.observed[source.ID]
	if cached, ok := fingerprints[source.Fingerprint]; ok {
		return cached, true
	}
	return Source{}, false
}

func (l *Ledger) observe(source Source) {
	fingerprints := l.observed[source.ID]
	if fingerprints == nil {
		fingerprints = make(map[string]Source)
		l.observed[source.ID] = fingerprints
	}
	fingerprints[source.Fingerprint] = source
}
