package core

import (
	"testing"
	"time"
)

func TestClockFuncReturnsUTC(t *testing.T) {
	local := time.Date(2026, 8, 13, 9, 30, 0, 123, time.FixedZone("EDT", -4*60*60))
	clock := ClockFunc(func() time.Time { return local })

	got := clock.Now()
	if !got.Equal(local) || got.Location() != time.UTC {
		t.Fatalf("ClockFunc.Now() = %v (%v), want %v (UTC)", got, got.Location(), local.UTC())
	}
}

func TestSystemClockReturnsUTC(t *testing.T) {
	if got := SystemClock.Now(); got.Location() != time.UTC {
		t.Fatalf("SystemClock.Now() location = %v, want UTC", got.Location())
	}
}
