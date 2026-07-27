// Package transcript stores a bounded, human-readable view of agent output.
package transcript

import (
	"sort"
	"sync"
)

type key struct {
	issue string
	stage string
}

type entry struct {
	seq   uint64
	stage string
	line  string
}

type ring struct {
	items []entry
	next  int
}

func (r *ring) add(e entry, limit int) {
	if len(r.items) < limit {
		r.items = append(r.items, e)
		return
	}
	r.items[r.next] = e
	r.next = (r.next + 1) % limit
}

func (r *ring) snapshot() []entry {
	if len(r.items) == 0 {
		return nil
	}
	if r.next == 0 {
		return append([]entry(nil), r.items...)
	}
	out := make([]entry, 0, len(r.items))
	out = append(out, r.items[r.next:]...)
	out = append(out, r.items[:r.next]...)
	return out
}

// Buffer keeps the last N lines per (issue, stage) key. It is safe for
// concurrent use, and retains a global sequence to rebuild cross-stage order.
type Buffer struct {
	mu     sync.Mutex
	perKey int
	seq    uint64
	byKey  map[key]*ring
}

func NewBuffer(perKey int) *Buffer {
	if perKey < 1 {
		perKey = 1
	}
	return &Buffer{perKey: perKey, byKey: map[key]*ring{}}
}

func (b *Buffer) Add(issueID, stage, line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	k := key{issue: issueID, stage: stage}
	r := b.byKey[k]
	if r == nil {
		r = &ring{}
		b.byKey[k] = r
	}
	r.add(entry{seq: b.seq, stage: stage, line: line}, b.perKey)
}

// Tail returns the most recent n lines across an issue's stages, oldest first.
// Each line is prefixed with its stage for display in the control plane.
func (b *Buffer) Tail(issueID string, n int) []string {
	if n <= 0 {
		return nil
	}
	b.mu.Lock()
	var entries []entry
	for k, r := range b.byKey {
		if k.issue == issueID {
			entries = append(entries, r.snapshot()...)
		}
	}
	b.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].seq < entries[j].seq })
	if len(entries) > n {
		entries = entries[len(entries)-n:]
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.stage + " │ " + e.line
	}
	return out
}
