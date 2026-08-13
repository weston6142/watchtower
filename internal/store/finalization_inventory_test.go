package store

import (
	"reflect"
	"testing"
)

func TestFinalizationBoundaryInventory(t *testing.T) {
	want := []string{
		"verification_ready",
		"pending_reverification",
		"reverification_failed",
		"publish_pending",
		"cleanup_needed",
		"merged",
	}

	got := FinalizationBoundaries()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("finalization boundaries = %v, want %v", got, want)
	}
	got[0] = "claimed"
	if again := FinalizationBoundaries(); !reflect.DeepEqual(again, want) {
		t.Fatalf("finalization boundaries after caller mutation = %v, want %v", again, want)
	}
}
