package marshal

import (
	"context"
	"sync"

	"github.com/wbushyeager/guildhall/internal/core"
	"github.com/wbushyeager/guildhall/internal/touchset"
)

type Emit func(t core.EventType, issueID string, payload any)

type entry struct {
	ts     touchset.Set
	behind string
	gone   chan struct{}
}

// Marshal sequences merges of issues whose plans touch overlapping files.
type Marshal struct {
	mu    sync.Mutex
	emit  Emit
	inFly map[string]*entry
	order []string
}

func New(emit Emit) *Marshal {
	return &Marshal{emit: emit, inFly: map[string]*entry{}}
}

func (m *Marshal) PlanApproved(issueID string, ts touchset.Set) {
	m.mu.Lock()
	if _, exists := m.inFly[issueID]; exists {
		m.mu.Unlock()
		return
	}
	e := &entry{ts: ts, gone: make(chan struct{})}
	for _, prior := range m.order {
		pe, ok := m.inFly[prior]
		if !ok {
			continue
		}
		if touchset.Overlap(ts, pe.ts) {
			e.behind = prior
			break
		}
	}
	m.inFly[issueID] = e
	m.order = append(m.order, issueID)
	behind := e.behind
	m.mu.Unlock()
	if behind != "" && m.emit != nil {
		m.emit(core.EvMergeSequenced, issueID, map[string]string{"behind": behind})
	}
}

func (m *Marshal) ReadyToMerge(ctx context.Context, issueID string) error {
	for {
		m.mu.Lock()
		e, ok := m.inFly[issueID]
		if !ok || e.behind == "" {
			m.mu.Unlock()
			return nil
		}
		pe, ok := m.inFly[e.behind]
		if !ok {
			e.behind = ""
			m.mu.Unlock()
			return nil
		}
		gone := pe.gone
		m.mu.Unlock()
		select {
		case <-gone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (m *Marshal) clear(issueID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.inFly[issueID]; ok {
		close(e.gone)
		delete(m.inFly, issueID)
	}
	for i, id := range m.order {
		if id == issueID {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
}

func (m *Marshal) Merged(issueID string)  { m.clear(issueID) }
func (m *Marshal) Aborted(issueID string) { m.clear(issueID) }

func (m *Marshal) BlockedBehind(issueID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	frontier := []string{issueID}
	for len(frontier) > 0 {
		next := []string{}
		for id, e := range m.inFly {
			for _, f := range frontier {
				if e.behind == f {
					n++
					next = append(next, id)
				}
			}
		}
		frontier = next
	}
	return n
}
