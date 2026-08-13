package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/contextpack"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/stagelifecycle"
)

func TestCapabilityAttemptEvidenceIsAppendOnlyAndBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capability.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	attempt, record := capabilityFixture(t)
	if err := s.CreateCapabilityAttempt(record); err != nil {
		t.Fatal(err)
	}
	plan := capability.EnforcementPlan{ContractID: record.Contract.ContractID, Provider: "fake", Implementation: "test", Version: "1", PlanID: strings.Repeat("b", 64)}
	if err := s.RecordCapabilityPreflight(attempt, plan); err != nil {
		t.Fatal(err)
	}
	baseline := capability.BaselineIdentity{Digest: strings.Repeat("c", 64)}
	if err := s.RecordCapabilityBaseline(attempt, baseline); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendCapabilityAudit(capability.AuditRecord{Attempt: attempt, ContractID: record.Contract.ContractID, Phase: "runtime", Outcome: "allowed", Operation: capability.OpWorkspaceRead, Paths: []string{"ISSUE.md"}}); err != nil {
		t.Fatal(err)
	}
	resultSHA := strings.Repeat("d", 64)
	validation := capability.ValidationResult{Passed: true, ResultDigest: resultSHA, DeltaDigest: strings.Repeat("e", 64)}
	if err := s.BindCapabilityValidation(attempt, resultSHA, validation); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.CreateCapabilityAttempt(record); err != nil {
		t.Fatalf("exact create replay: %v", err)
	}
	if err := s.RecordCapabilityPreflight(attempt, plan); err != nil {
		t.Fatalf("exact preflight replay: %v", err)
	}
	got, found, err := s.CapabilityAttempt(attempt.IssueID, attempt.Stage, attempt.AttemptID)
	if err != nil || !found {
		t.Fatalf("attempt found=%v err=%v", found, err)
	}
	if got.Plan.PlanID != plan.PlanID || got.Baseline != baseline || got.Validation != validation || got.ImmutableResultID != resultSHA {
		t.Fatalf("reopened attempt = %+v", got)
	}
	audit, err := s.CapabilityAudit(attempt.IssueID, attempt.Stage, attempt.AttemptID)
	if err != nil || len(audit) != 1 || audit[0].Operation != capability.OpWorkspaceRead {
		t.Fatalf("audit = %+v, %v", audit, err)
	}

	conflict := plan
	conflict.PlanID = strings.Repeat("f", 64)
	if err := s.RecordCapabilityPreflight(attempt, conflict); diagnosticCode(err) != CodeConflict {
		t.Fatalf("conflicting preflight error = %v", err)
	}
}

func TestCapabilityAttemptRejectsConflictingIdentity(t *testing.T) {
	s := openLifecycleStore(t)
	_, record := capabilityFixture(t)
	if err := s.CreateCapabilityAttempt(record); err != nil {
		t.Fatal(err)
	}
	conflictContract, err := capability.Compile(capability.CompileInput{
		IssueID: "GH-68", Stage: "execute", AttemptID: "attempt-1", Profile: flow.ProfileArtifact,
		WorkspaceRoot: "/tmp/worktree", MaterializedInputs: []string{"STAGE.md"},
		Repository: capability.RepositoryIdentity{Branch: "issue/GH-68", BaseCommit: "base", StartCommit: "head", Tree: "tree"},
	})
	if err != nil {
		t.Fatal(err)
	}
	conflict := record
	conflict.Contract = conflictContract
	if err := s.CreateCapabilityAttempt(conflict); diagnosticCode(err) != CodeConflict {
		t.Fatalf("conflicting create error = %v", err)
	}
}

func TestCapabilityAuditRedactsSensitivePayloads(t *testing.T) {
	s := openLifecycleStore(t)
	attempt, record := capabilityFixture(t)
	if err := s.CreateCapabilityAttempt(record); err != nil {
		t.Fatal(err)
	}
	for _, diagnostic := range []string{
		"TOKEN=secret", "https://user:secret@example.com/repo", "command: git push origin main", "prompt contents are secret",
	} {
		err := s.AppendCapabilityAudit(capability.AuditRecord{Attempt: attempt, Phase: "runtime", Outcome: "denied", Diagnostic: diagnostic})
		if err == nil {
			t.Errorf("sensitive diagnostic %q was accepted", diagnostic)
		}
	}
	if audit, err := s.CapabilityAudit(attempt.IssueID, attempt.Stage, attempt.AttemptID); err != nil || len(audit) != 0 {
		t.Fatalf("audit after rejection = %+v, %v", audit, err)
	}
}

func TestRunnerSucceededRequiresValidatedCapabilityEvidence(t *testing.T) {
	for _, test := range []struct {
		name       string
		validation *capability.ValidationResult
		bindSHA    string
		wantOK     bool
	}{
		{name: "missing"},
		{name: "failed", validation: &capability.ValidationResult{Passed: false}},
		{name: "mismatched", validation: &capability.ValidationResult{Passed: true, ResultDigest: strings.Repeat("b", 64), DeltaDigest: strings.Repeat("d", 64)}, bindSHA: strings.Repeat("b", 64)},
		{name: "matching", validation: &capability.ValidationResult{Passed: true, ResultDigest: strings.Repeat("a", 64), DeltaDigest: strings.Repeat("d", 64)}, bindSHA: strings.Repeat("a", 64), wantOK: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := openLifecycleStore(t)
			identity, capRecord := capabilityFixture(t)
			attempt := BeginAttempt(identity.IssueID, identity.Stage, identity.AttemptID)
			if err := s.CreateStageLifecycleAttempt(attempt); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateCapabilityAttempt(capRecord); err != nil {
				t.Fatal(err)
			}
			if test.validation != nil {
				if err := s.BindCapabilityValidation(identity, test.bindSHA, *test.validation); err != nil && test.name != "failed" {
					t.Fatal(err)
				}
			}
			result := contextpack.AttemptResult{AttemptID: attempt.AttemptID, ResultPath: "artifacts/attempts/attempt-1/result/manifest.json", ResultSHA256: strings.Repeat("a", 64)}
			err := s.PutStageLifecycleResult(attempt, result)
			if !test.wantOK {
				if err == nil {
					t.Fatal("unvalidated result was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			record := lifecycleRecord(attempt, stagelifecycle.RunnerSucceeded, 1, 0, "attempt-1:runner_succeeded")
			if err := s.PrepareStageLifecycle(record); err != nil {
				t.Fatal(err)
			}
			if err := s.CommitPreparedStageLifecycle(record); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHistoricalCompletedLifecycleRemainsReadable(t *testing.T) {
	s := openLifecycleStore(t)
	attempt := BeginAttempt("GH-old", "execute", "legacy-1")
	attempt.CapabilitySchemaVersion = 0
	attempt.CapabilityContractSHA256 = ""
	if err := s.CreateStageLifecycleAttempt(attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE stage_lifecycle_attempts SET result_path=?,result_sha256=? WHERE issue_id=? AND stage=? AND attempt_id=?`,
		"artifacts/attempts/legacy-1/result/manifest.json", strings.Repeat("a", 64), attempt.IssueID, attempt.Stage, attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	record := lifecycleRecord(attempt, stagelifecycle.RunnerSucceeded, 1, 0, "legacy-1:runner_succeeded")
	if err := s.PrepareStageLifecycle(record); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE stage_lifecycle_checkpoints SET status='committed' WHERE issue_id=? AND stage=? AND attempt_id=?`, attempt.IssueID, attempt.Stage, attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.LatestCommittedStageLifecycle(attempt)
	if err != nil || !found || got.Substate != stagelifecycle.RunnerSucceeded {
		t.Fatalf("historical lifecycle = %+v found=%v err=%v", got, found, err)
	}
}

func capabilityFixture(t *testing.T) (capability.AttemptIdentity, capability.AttemptRecord) {
	t.Helper()
	compiled, err := capability.Compile(capability.CompileInput{
		IssueID: "GH-68", Stage: "execute", AttemptID: "attempt-1", Profile: flow.ProfileArtifact,
		WorkspaceRoot: "/tmp/worktree", MaterializedInputs: []string{"ISSUE.md"},
		Repository: capability.RepositoryIdentity{Branch: "issue/GH-68", BaseCommit: "base", StartCommit: "head", Tree: "tree"},
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := capability.AttemptIdentity{IssueID: "GH-68", Stage: "execute", AttemptID: "attempt-1"}
	return identity, capability.AttemptRecord{Identity: identity, SchemaVersion: capability.ContractVersion, Contract: compiled}
}

func requireLifecycleCode(t *testing.T, err error, want DiagnosticCode) {
	t.Helper()
	var diagnostic *DiagnosticError
	if !errors.As(err, &diagnostic) || diagnostic.Code != want {
		t.Fatalf("error = %v, want %s", err, want)
	}
}
