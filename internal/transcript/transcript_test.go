package transcript

import (
	"strings"
	"testing"
)

func TestBufferTailBoundedAndOrdered(t *testing.T) {
	b := NewBuffer(3)
	for i := 1; i <= 5; i++ {
		b.Add("GH-1", "execute", strings.Repeat("x", i)) // lengths 1..5
	}
	b.Add("GH-1", "review", "rev line")
	b.Add("GH-2", "spec", "other issue")

	got := b.Tail("GH-1", 10)
	if len(got) != 4 { // 3 kept from execute + 1 review
		t.Fatalf("tail: %v", got)
	}
	if !strings.HasPrefix(got[0], "execute │ xxx") || !strings.HasPrefix(got[3], "review │ rev") {
		t.Fatalf("order/prefix wrong: %v", got)
	}
	if len(b.Tail("GH-1", 2)) != 2 {
		t.Fatal("n limit ignored")
	}
	if len(b.Tail("GH-3", 5)) != 0 {
		t.Fatal("unknown issue should be empty")
	}
}
