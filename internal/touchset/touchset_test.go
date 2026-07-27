package touchset

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAndOverlap(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "touchset.json")
	if err := os.WriteFile(p, []byte(`{"globs":["internal/pay/**","go.mod"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Load(p)
	if err != nil || len(a.Globs) != 2 {
		t.Fatalf("load: %+v %v", a, err)
	}
	cases := []struct {
		b    []string
		want bool
	}{
		{[]string{"internal/pay/refund.go"}, true},
		{[]string{"internal/payments/**"}, false},
		{[]string{"docs/**"}, false},
		{[]string{"go.mod"}, true},
		{[]string{"internal/**"}, true},
	}
	for _, c := range cases {
		if got := Overlap(a, Set{Globs: c.b}); got != c.want {
			t.Errorf("overlap(%v)=%v want %v", c.b, got, c.want)
		}
	}
}
