package stagelifecycle

import (
	"reflect"
	"testing"
)

func TestDurableBoundaryInventory(t *testing.T) {
	want := []Substate{
		RunnerSucceeded, ArtifactsValidated, ArtifactsArchived,
		GateResolved, VerificationPassed, FinalizationReady,
	}

	got := DurableBoundaries()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("durable boundaries = %v, want %v", got, want)
	}
	got[0] = FinalizationReady
	if again := DurableBoundaries(); !reflect.DeepEqual(again, want) {
		t.Fatalf("durable boundaries after caller mutation = %v, want %v", again, want)
	}
}
