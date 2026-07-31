package core

import (
	"testing"
	"time"
)

func TestStartOfDayReturnsLocalMidnight(t *testing.T) {
	zone := time.FixedZone("TST", -5*3600)
	now := time.Date(2026, 7, 31, 14, 37, 12, 500, zone)
	got := StartOfDay(now)

	want := time.Date(2026, 7, 31, 0, 0, 0, 0, zone)
	if !got.Equal(want) {
		t.Fatalf("StartOfDay(%v) = %v, want %v", now, got, want)
	}
	if got.Location() != zone {
		t.Fatalf("location = %v, want %v", got.Location(), zone)
	}
	if again := StartOfDay(got); !again.Equal(got) {
		t.Fatalf("not idempotent: StartOfDay(%v) = %v", got, again)
	}
}
