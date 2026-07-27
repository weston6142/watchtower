package slots

import (
	"context"
	"sort"
	"sync"
)

type waiter struct {
	issueID  string
	priority int
	order    int64
	ready    chan struct{}
}

type Pool struct {
	mu      sync.Mutex
	free    int
	held    map[string]int // issueID -> count held
	queue   []*waiter
	counter int64
}

func NewPool(n int) *Pool {
	return &Pool{free: n, held: map[string]int{}}
}

func (p *Pool) Acquire(ctx context.Context, issueID string, priority int) (func(), error) {
	p.mu.Lock()
	if p.free > 0 && len(p.queue) == 0 {
		p.free--
		p.held[issueID]++
		p.mu.Unlock()
		return p.releaseFunc(issueID), nil
	}
	w := &waiter{issueID: issueID, priority: priority, order: p.counter, ready: make(chan struct{})}
	p.counter++
	p.queue = append(p.queue, w)
	p.sortQueue()
	p.mu.Unlock()

	select {
	case <-w.ready:
		return p.releaseFunc(issueID), nil
	case <-ctx.Done():
		p.mu.Lock()
		for i, q := range p.queue {
			if q == w {
				p.queue = append(p.queue[:i], p.queue[i+1:]...)
				break
			}
		}
		p.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (p *Pool) releaseFunc(issueID string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.held[issueID]--
			if p.held[issueID] <= 0 {
				delete(p.held, issueID)
			}
			if len(p.queue) > 0 {
				w := p.queue[0]
				p.queue = p.queue[1:]
				p.held[w.issueID]++
				close(w.ready)
			} else {
				p.free++
			}
		})
	}
}

func (p *Pool) sortQueue() {
	sort.SliceStable(p.queue, func(i, j int) bool {
		if p.queue[i].priority != p.queue[j].priority {
			return p.queue[i].priority > p.queue[j].priority
		}
		return p.queue[i].order < p.queue[j].order
	})
}

func (p *Pool) Snapshot() (held []string, queued []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id := range p.held {
		held = append(held, id)
	}
	sort.Strings(held)
	for _, w := range p.queue {
		queued = append(queued, w.issueID)
	}
	return
}
