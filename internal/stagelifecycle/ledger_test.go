package stagelifecycle

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestSixSubstatesAreOrdered(t *testing.T) {
	got := []Substate{}
	current := Substate("")
	for {
		next, ok := Next(current)
		if !ok {
			break
		}
		got = append(got, next)
		current = next
	}
	want := []Substate{
		RunnerSucceeded, ArtifactsValidated, ArtifactsArchived,
		GateResolved, VerificationPassed, FinalizationReady,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("substates = %v, want %v", got, want)
	}
}

func TestExactTransitionReplayIsHarmlessButConflictsFailClosed(t *testing.T) {
	first := Record{SchemaVersion: 1, Version: 2, Substate: ArtifactsValidated,
		AttemptID: "attempt-7", PredecessorVersion: 1, TransitionID: "attempt-7:artifacts_validated",
		ResultRef: "artifacts/attempts/attempt-7/result/manifest.json", ResultDigest: strings.Repeat("a", 64), PayloadDigest: strings.Repeat("a", 64)}
	predecessor := &Record{SchemaVersion: 1, Version: 1, Substate: RunnerSucceeded,
		AttemptID: "attempt-7", TransitionID: "attempt-7:runner_succeeded",
		ResultRef: "artifacts/attempts/attempt-7/result/manifest.json", ResultDigest: strings.Repeat("a", 64), PayloadDigest: strings.Repeat("a", 64)}
	if err := ValidateTransition(predecessor, first); err != nil {
		t.Fatal(err)
	}
	if !SameTransition(first, first) {
		t.Fatal("exact replay was not identical")
	}
	conflicting := first
	conflicting.PayloadDigest = strings.Repeat("b", 64)
	if err := ValidateReplay(first, conflicting); err == nil {
		t.Fatal("conflicting replay was accepted")
	}
}

func TestTransitionDiagnosticsRejectUnsafeOrUnknownState(t *testing.T) {
	valid := Record{SchemaVersion: SchemaVersion, Version: 1, Substate: RunnerSucceeded,
		AttemptID: "attempt-7", TransitionID: "attempt-7:runner_succeeded", ResultRef: "artifacts/attempts/attempt-7/result/manifest.json",
		ResultDigest: strings.Repeat("a", 64), PayloadDigest: strings.Repeat("a", 64)}
	cases := []struct {
		name   string
		record Record
		code   DiagnosticCode
	}{
		{name: "unknown version", record: Record{SchemaVersion: 2, Version: 1,
			Substate: RunnerSucceeded, TransitionID: "id", ResultRef: "result.json",
			PayloadDigest: strings.Repeat("a", 64)}, code: CodeUnknownVersion},
		{name: "unknown substate", record: Record{SchemaVersion: SchemaVersion, Version: 1,
			Substate: Substate("future"), TransitionID: "id", ResultRef: "result.json",
			PayloadDigest: strings.Repeat("a", 64)}, code: CodeInvalidState},
		{name: "invalid digest", record: Record{SchemaVersion: SchemaVersion, Version: 1,
			Substate: RunnerSucceeded, TransitionID: "id", ResultRef: "result.json",
			PayloadDigest: "not-a-digest"}, code: CodeIntegrity},
		{name: "missing result", record: Record{SchemaVersion: SchemaVersion, Version: 1,
			Substate: RunnerSucceeded, TransitionID: "id", PayloadDigest: strings.Repeat("a", 64)}, code: CodeMissingResult},
		{name: "missing result digest", record: Record{SchemaVersion: SchemaVersion, Version: 1,
			Substate: RunnerSucceeded, TransitionID: "id", ResultRef: "result.json",
			PayloadDigest: strings.Repeat("a", 64)}, code: CodeIntegrity},
		{name: "result outside attempt slot", record: Record{SchemaVersion: SchemaVersion, Version: 1,
			AttemptID: "attempt-7", Substate: RunnerSucceeded, TransitionID: "id", ResultRef: "result.json",
			ResultDigest: strings.Repeat("a", 64), PayloadDigest: strings.Repeat("a", 64)}, code: CodeIntegrity},
		{name: "escaping artifact", record: Record{SchemaVersion: SchemaVersion, Version: 1,
			Substate: RunnerSucceeded, TransitionID: "id", ResultRef: "result.json",
			PayloadDigest: strings.Repeat("a", 64), Artifacts: []ArtifactRef{{
				Name: "plan.md", Path: "attempts/attempt-7/../../plan.md", SHA256: strings.Repeat("a", 64),
			}}}, code: CodeIntegrity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRecord(tc.record)
			var diagnostic *DiagnosticError
			if !errors.As(err, &diagnostic) {
				t.Fatalf("error = %v, want diagnostic %s", err, tc.code)
			}
			if diagnostic.Code != tc.code {
				t.Fatalf("diagnostic code = %q, want %q", diagnostic.Code, tc.code)
			}
		})
	}
	if err := ValidateTransition(nil, valid); err != nil {
		t.Fatalf("empty predecessor should allow runner_succeeded: %v", err)
	}
	if err := ValidateTransition(&valid, Record{
		SchemaVersion: SchemaVersion, Version: 2, Substate: ArtifactsValidated,
		AttemptID: "attempt-7", PredecessorVersion: 1, TransitionID: "id-2", ResultRef: "artifacts/attempts/attempt-7/result/manifest.json",
		ResultDigest: strings.Repeat("a", 64), PayloadDigest: strings.Repeat("a", 64),
	}); err != nil {
		t.Fatalf("valid next transition rejected: %v", err)
	}
}
