package stagelifecycle

import (
	"reflect"
	"testing"
)

func TestDurableBoundaryInventory(t *testing.T) {
	want := []Substate{
		"runner_succeeded", "artifacts_validated", "artifacts_archived",
		"gate_resolved", "verification_passed", "finalization_ready",
	}

	got := DurableBoundaries()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("durable boundaries = %v, want %v", got, want)
	}
	got[0] = Substate("excluded")
	if again := DurableBoundaries(); !reflect.DeepEqual(again, want) {
		t.Fatalf("durable boundaries after caller mutation = %v, want %v", again, want)
	}
}
