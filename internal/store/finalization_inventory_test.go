package store

import (
	"reflect"
	"testing"
)

func TestFinalizationBoundaryInventory(t *testing.T) {
	want := []string{
		IntegrationVerificationReady,
		IntegrationPendingReverification,
		IntegrationReverificationFailed,
		IntegrationPublishPending,
		IntegrationCleanupNeeded,
		IntegrationMerged,
	}

	got := FinalizationBoundaries()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("finalization boundaries = %v, want %v", got, want)
	}
	got[0] = IntegrationClaimed
	if again := FinalizationBoundaries(); !reflect.DeepEqual(again, want) {
		t.Fatalf("finalization boundaries after caller mutation = %v, want %v", again, want)
	}
}
