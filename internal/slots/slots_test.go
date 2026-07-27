package slots

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestPriorityOrderAndSnapshot(t *testing.T) {
	p := NewPool(1)
	rel1, err := p.Acquire(context.Background(), "GH-1", 0)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	grab := func(id string, prio int) {
		defer wg.Done()
		rel, err := p.Acquire(context.Background(), id, prio)
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		order = append(order, id)
		mu.Unlock()
		rel()
	}
	wg.Add(2)
	go grab("GH-low", 0)
	time.Sleep(20 * time.Millisecond) // low enqueues first
	go grab("GH-high", 5)
	time.Sleep(20 * time.Millisecond)

	held, queued := p.Snapshot()
	if len(held) != 1 || held[0] != "GH-1" || len(queued) != 2 {
		t.Fatalf("snapshot wrong: held=%v queued=%v", held, queued)
	}

	rel1()
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if order[0] != "GH-high" {
		t.Fatalf("priority ignored: %v", order)
	}
}
