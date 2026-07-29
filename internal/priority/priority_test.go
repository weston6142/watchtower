package priority

import "testing"

func TestLabelNamed(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{-1, "low"}, {0, "normal"}, {1, "high"}, {2, "urgent"}} {
		if got := Label(tc.n); got != tc.want {
			t.Errorf("Label(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestLabelOutOfSet(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{3, "3"}, {-5, "-5"}, {42, "42"}} {
		if got := Label(tc.n); got != tc.want {
			t.Errorf("Label(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestCycleWraps(t *testing.T) {
	if got := Cycle(2, 1); got != -1 {
		t.Errorf("Cycle(2, 1) = %d, want -1", got)
	}
	if got := Cycle(-1, -1); got != 2 {
		t.Errorf("Cycle(-1, -1) = %d, want 2", got)
	}
}

func TestCycleInSetSteps(t *testing.T) {
	for _, tc := range []struct{ n, delta, want int }{
		{-1, 1, 0}, {0, -1, -1}, {0, 1, 1}, {1, -1, 0}, {1, 1, 2}, {2, -1, 1},
	} {
		if got := Cycle(tc.n, tc.delta); got != tc.want {
			t.Errorf("Cycle(%d, %d) = %d, want %d", tc.n, tc.delta, got, tc.want)
		}
	}
}

func TestCycleFromOutOfSet(t *testing.T) {
	for _, tc := range []struct{ n, delta, want int }{
		{3, -1, 2}, {3, 1, 2}, {42, -1, 2}, {42, 1, 2},
		{-5, -1, -1}, {-5, 1, -1},
	} {
		got := Cycle(tc.n, tc.delta)
		if got != tc.want {
			t.Errorf("Cycle(%d, %d) = %d, want %d", tc.n, tc.delta, got, tc.want)
		}
		// A second move can never return to the raw value.
		for _, d := range []int{-1, 1} {
			if back := Cycle(got, d); back == tc.n {
				t.Errorf("Cycle(%d, %d) returned to out-of-set %d", got, d, tc.n)
			}
		}
	}
}

func TestCycleAlwaysInSet(t *testing.T) {
	for n := -10; n <= 10; n++ {
		for _, delta := range []int{-1, 1} {
			got := Cycle(n, delta)
			if _, ok := indexOf(got); !ok {
				t.Errorf("Cycle(%d, %d) = %d, not a member of Levels", n, delta, got)
			}
		}
	}
}
