// Package priority names the urgency levels an issue can carry. Storage, the
// wire protocol, and both queue sorts keep priority as a plain int; the names
// exist only so the TUI and the CLI can render one shared vocabulary instead of
// asking the operator to remember that a larger number is more urgent.
package priority

import "strconv"

// Level pairs a name with the int that persists.
type Level struct {
	Name  string
	Value int
}

// Levels is the cycle order, lowest urgency first. normal is anchored at 0 so
// every row already stored at the old default reads as normal and the CLI's
// -priority default still agrees with the modal default.
var Levels = []Level{{"low", -1}, {"normal", 0}, {"high", 1}, {"urgent", 2}}

// Label names n, or renders it as a decimal when n is outside Levels. Total: an
// out-of-set value is shown, never rejected or silently renumbered.
func Label(n int) string {
	for _, l := range Levels {
		if l.Value == n {
			return l.Name
		}
	}
	return strconv.Itoa(n)
}

// Cycle moves n by delta and always returns a member of Levels. For an in-set n
// the move is modulo, matching the lever editor's cycler. For an out-of-set n
// the first move lands on the nearest named level in the direction travelled —
// deliberately not a modulo cycle over n itself, which would let urgent wrap
// back onto a stale raw number.
func Cycle(n, delta int) int {
	if i, ok := indexOf(n); ok {
		return Levels[(i+delta+len(Levels))%len(Levels)].Value
	}
	if delta < 0 {
		// Highest named level below n; low if none is.
		for i := len(Levels) - 1; i >= 0; i-- {
			if Levels[i].Value < n {
				return Levels[i].Value
			}
		}
		return Levels[0].Value
	}
	// Lowest named level above n; urgent if none is.
	for _, l := range Levels {
		if l.Value > n {
			return l.Value
		}
	}
	return Levels[len(Levels)-1].Value
}

func indexOf(n int) (int, bool) {
	for i, l := range Levels {
		if l.Value == n {
			return i, true
		}
	}
	return 0, false
}
