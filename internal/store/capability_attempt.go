package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/weston6142/watchtower/internal/capability"
)

const capabilitySchemaVersion = 1

func (s *Store) CreateCapabilityAttempt(record capability.AttemptRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	contractJSON, err := validateCapabilityAttempt(record)
	if err != nil {
		return err
	}
	var existingJSON, existingContractID, existingAuthority string
	err = s.db.QueryRow(`SELECT contract_json,contract_digest,authority_digest FROM capability_attempts
		WHERE issue_id=? AND stage=? AND attempt_id=?`, record.Identity.IssueID, record.Identity.Stage, record.Identity.AttemptID).
		Scan(&existingJSON, &existingContractID, &existingAuthority)
	if err == nil {
		if existingJSON == contractJSON && existingContractID == record.Contract.ContractID && existingAuthority == record.Contract.AuthorityDigest {
			return nil
		}
		return lifecycleDiagnostic(CodeConflict, "capability attempt identity already has different authority")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.db.Exec(`INSERT INTO capability_attempts(
		issue_id,stage,attempt_id,schema_version,contract_json,contract_digest,authority_digest,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, record.Identity.IssueID, record.Identity.Stage, record.Identity.AttemptID,
		record.SchemaVersion, contractJSON, record.Contract.ContractID, record.Contract.AuthorityDigest, now, now)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE stage_lifecycle_attempts SET capability_contract_sha256=?
		WHERE issue_id=? AND stage=? AND attempt_id=? AND capability_schema_version=? AND capability_contract_sha256=''`,
		record.Contract.ContractID, record.Identity.IssueID, record.Identity.Stage, record.Identity.AttemptID, capabilitySchemaVersion)
	return err
}

func (s *Store) RecordCapabilityPreflight(identity capability.AttemptIdentity, plan capability.EnforcementPlan) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCapabilityIdentity(identity); err != nil {
		return err
	}
	if !validDigest(plan.PlanID) || plan.ContractID == "" || plan.Provider == "" || plan.Implementation == "" || plan.Version == "" {
		return lifecycleDiagnostic(CodeIntegrity, "capability enforcement plan identity is incomplete")
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	var contractID, existing string
	err = s.db.QueryRow(`SELECT contract_digest,enforcement_plan_json FROM capability_attempts
		WHERE issue_id=? AND stage=? AND attempt_id=?`, identity.IssueID, identity.Stage, identity.AttemptID).Scan(&contractID, &existing)
	if errors.Is(err, sql.ErrNoRows) {
		return lifecycleDiagnostic(CodeInvalidState, "capability attempt is missing")
	}
	if err != nil {
		return err
	}
	if plan.ContractID != contractID {
		return lifecycleDiagnostic(CodeConflict, "enforcement plan contract identity conflicts")
	}
	if existing != "" {
		if existing == string(encoded) {
			return nil
		}
		return lifecycleDiagnostic(CodeConflict, "capability preflight already has different data")
	}
	_, err = s.db.Exec(`UPDATE capability_attempts SET enforcement_plan_json=?,enforcement_plan_digest=?,updated_at=?
		WHERE issue_id=? AND stage=? AND attempt_id=? AND enforcement_plan_json=''`, string(encoded), plan.PlanID,
		time.Now().UTC().Format(time.RFC3339Nano), identity.IssueID, identity.Stage, identity.AttemptID)
	return err
}

func (s *Store) RecordCapabilityBaseline(identity capability.AttemptIdentity, baseline capability.BaselineIdentity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCapabilityIdentity(identity); err != nil {
		return err
	}
	if !validDigest(baseline.Digest) {
		return lifecycleDiagnostic(CodeIntegrity, "capability baseline digest is invalid")
	}
	encoded, err := json.Marshal(baseline)
	if err != nil {
		return err
	}
	return s.fillCapabilitySlotLocked(identity, "baseline_json", "baseline_digest", string(encoded), baseline.Digest)
}

func (s *Store) AppendCapabilityAudit(record capability.AuditRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCapabilityIdentity(record.Attempt); err != nil {
		return err
	}
	if strings.TrimSpace(record.Phase) == "" || strings.TrimSpace(record.Outcome) == "" {
		return lifecycleDiagnostic(CodeIntegrity, "capability audit phase and outcome are required")
	}
	if !safeCapabilityDiagnostic(record.Diagnostic) {
		return lifecycleDiagnostic(CodeIntegrity, "capability audit diagnostic contains sensitive payload")
	}
	paths := append([]string(nil), record.Paths...)
	sort.Strings(paths)
	encoded, err := json.Marshal(paths)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO capability_audit_records(
		issue_id,stage,attempt_id,contract_id,phase,outcome,reason,provider,implementation,operation,paths_json,diagnostic,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, record.Attempt.IssueID, record.Attempt.Stage, record.Attempt.AttemptID,
		record.ContractID, record.Phase, record.Outcome, record.Reason, record.Provider, record.Implementation,
		record.Operation, string(encoded), record.Diagnostic, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) BindCapabilityValidation(identity capability.AttemptIdentity, resultSHA string, result capability.ValidationResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCapabilityIdentity(identity); err != nil {
		return err
	}
	if result.Passed {
		if !validDigest(resultSHA) || result.ResultDigest != resultSHA || !validDigest(result.DeltaDigest) {
			return lifecycleDiagnostic(CodeIntegrity, "successful capability validation identity is incomplete")
		}
	} else if result.ResultDigest != "" && result.ResultDigest != resultSHA {
		return lifecycleDiagnostic(CodeConflict, "failed capability validation result identity conflicts")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	status := "failed"
	if result.Passed {
		status = "passed"
	}
	var existing, existingStatus, existingResult string
	err = s.db.QueryRow(`SELECT validation_json,final_status,immutable_result_digest FROM capability_attempts
		WHERE issue_id=? AND stage=? AND attempt_id=?`, identity.IssueID, identity.Stage, identity.AttemptID).
		Scan(&existing, &existingStatus, &existingResult)
	if errors.Is(err, sql.ErrNoRows) {
		return lifecycleDiagnostic(CodeInvalidState, "capability attempt is missing")
	}
	if err != nil {
		return err
	}
	if existing != "" {
		if existing == string(encoded) && existingStatus == status && existingResult == resultSHA {
			return nil
		}
		return lifecycleDiagnostic(CodeConflict, "capability validation already has different data")
	}
	_, err = s.db.Exec(`UPDATE capability_attempts SET validation_json=?,final_status=?,immutable_result_digest=?,updated_at=?
		WHERE issue_id=? AND stage=? AND attempt_id=? AND validation_json=''`, string(encoded), status, resultSHA,
		time.Now().UTC().Format(time.RFC3339Nano), identity.IssueID, identity.Stage, identity.AttemptID)
	return err
}

func (s *Store) CapabilityAttempt(issueID, stage, attemptID string) (capability.AttemptRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	identity := capability.AttemptIdentity{IssueID: issueID, Stage: stage, AttemptID: attemptID}
	if err := validateCapabilityIdentity(identity); err != nil {
		return capability.AttemptRecord{}, false, err
	}
	var schemaVersion int
	var contractJSON, planJSON, baselineJSON, validationJSON, resultDigest string
	err := s.db.QueryRow(`SELECT schema_version,contract_json,enforcement_plan_json,baseline_json,validation_json,immutable_result_digest
		FROM capability_attempts WHERE issue_id=? AND stage=? AND attempt_id=?`, issueID, stage, attemptID).
		Scan(&schemaVersion, &contractJSON, &planJSON, &baselineJSON, &validationJSON, &resultDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return capability.AttemptRecord{}, false, nil
	}
	if err != nil {
		return capability.AttemptRecord{}, false, err
	}
	record := capability.AttemptRecord{Identity: identity, SchemaVersion: schemaVersion, ImmutableResultID: resultDigest}
	for _, item := range []struct {
		value  string
		target any
	}{{contractJSON, &record.Contract}, {planJSON, &record.Plan}, {baselineJSON, &record.Baseline}, {validationJSON, &record.Validation}} {
		if item.value != "" {
			if err := json.Unmarshal([]byte(item.value), item.target); err != nil {
				return capability.AttemptRecord{}, false, lifecycleDiagnostic(CodeIntegrity, "capability evidence is malformed")
			}
		}
	}
	return record, true, nil
}

func (s *Store) CapabilityAudit(issueID, stage, attemptID string) ([]capability.AuditRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	identity := capability.AttemptIdentity{IssueID: issueID, Stage: stage, AttemptID: attemptID}
	if err := validateCapabilityIdentity(identity); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT contract_id,phase,outcome,reason,provider,implementation,operation,paths_json,diagnostic
		FROM capability_audit_records WHERE issue_id=? AND stage=? AND attempt_id=? ORDER BY sequence`, issueID, stage, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []capability.AuditRecord
	for rows.Next() {
		record := capability.AuditRecord{Attempt: identity}
		var pathsJSON string
		if err := rows.Scan(&record.ContractID, &record.Phase, &record.Outcome, &record.Reason, &record.Provider,
			&record.Implementation, &record.Operation, &pathsJSON, &record.Diagnostic); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(pathsJSON), &record.Paths); err != nil {
			return nil, lifecycleDiagnostic(CodeIntegrity, "capability audit paths are malformed")
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (s *Store) fillCapabilitySlotLocked(identity capability.AttemptIdentity, valueColumn, digestColumn, value, digest string) error {
	query := fmt.Sprintf(`SELECT %s FROM capability_attempts WHERE issue_id=? AND stage=? AND attempt_id=?`, valueColumn)
	var existing string
	err := s.db.QueryRow(query, identity.IssueID, identity.Stage, identity.AttemptID).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return lifecycleDiagnostic(CodeInvalidState, "capability attempt is missing")
	}
	if err != nil {
		return err
	}
	if existing != "" {
		if existing == value {
			return nil
		}
		return lifecycleDiagnostic(CodeConflict, "capability evidence slot already has different data")
	}
	update := fmt.Sprintf(`UPDATE capability_attempts SET %s=?,%s=?,updated_at=? WHERE issue_id=? AND stage=? AND attempt_id=? AND %s=''`, valueColumn, digestColumn, valueColumn)
	_, err = s.db.Exec(update, value, digest, time.Now().UTC().Format(time.RFC3339Nano), identity.IssueID, identity.Stage, identity.AttemptID)
	return err
}

func (s *Store) requireCapabilityValidationLocked(attempt StageLifecycleAttempt, resultSHA string) error {
	var schemaVersion int
	var contractID string
	err := s.db.QueryRow(`SELECT capability_schema_version,capability_contract_sha256 FROM stage_lifecycle_attempts
		WHERE issue_id=? AND stage=? AND attempt_id=?`, attempt.IssueID, attempt.Stage, attempt.AttemptID).Scan(&schemaVersion, &contractID)
	if err != nil {
		return err
	}
	if schemaVersion != capabilitySchemaVersion || !validDigest(contractID) {
		return lifecycleDiagnostic(CodeInvalidState, "capability validation evidence is required")
	}
	var status, boundResult, capabilityContract string
	err = s.db.QueryRow(`SELECT final_status,immutable_result_digest,contract_digest FROM capability_attempts
		WHERE issue_id=? AND stage=? AND attempt_id=?`, attempt.IssueID, attempt.Stage, attempt.AttemptID).
		Scan(&status, &boundResult, &capabilityContract)
	if errors.Is(err, sql.ErrNoRows) {
		return lifecycleDiagnostic(CodeInvalidState, "capability validation evidence is missing")
	}
	if err != nil {
		return err
	}
	if status != "passed" || boundResult != resultSHA || capabilityContract != contractID {
		return lifecycleDiagnostic(CodeIntegrity, "capability validation is failed or mismatched")
	}
	return nil
}

func validateCapabilityAttempt(record capability.AttemptRecord) (string, error) {
	if err := validateCapabilityIdentity(record.Identity); err != nil {
		return "", err
	}
	if record.SchemaVersion != capabilitySchemaVersion || record.Contract.Contract.Version != capability.ContractVersion {
		return "", lifecycleDiagnostic(CodeIntegrity, "unsupported capability schema version")
	}
	contract := record.Contract.Contract
	if contract.IssueID != record.Identity.IssueID || contract.Stage != record.Identity.Stage || contract.AttemptID != record.Identity.AttemptID {
		return "", lifecycleDiagnostic(CodeConflict, "capability contract identity conflicts with attempt")
	}
	contractBytes, err := json.Marshal(contract)
	if err != nil {
		return "", err
	}
	if hashBytes(contractBytes) != record.Contract.ContractID {
		return "", lifecycleDiagnostic(CodeIntegrity, "capability contract digest is invalid")
	}
	authority := contract
	authority.AttemptID = ""
	authorityBytes, err := json.Marshal(authority)
	if err != nil {
		return "", err
	}
	if hashBytes(authorityBytes) != record.Contract.AuthorityDigest {
		return "", lifecycleDiagnostic(CodeIntegrity, "capability authority digest is invalid")
	}
	encoded, err := json.Marshal(record.Contract)
	return string(encoded), err
}

func validateCapabilityIdentity(identity capability.AttemptIdentity) error {
	if strings.TrimSpace(identity.IssueID) == "" || strings.TrimSpace(identity.Stage) == "" || strings.TrimSpace(identity.AttemptID) == "" {
		return lifecycleDiagnostic(CodeIntegrity, "capability attempt identity is incomplete")
	}
	return nil
}

func safeCapabilityDiagnostic(value string) bool {
	lower := strings.ToLower(value)
	for _, forbidden := range []string{"token=", "password", "credential", "secret", "://", "command:", "prompt", "environment", "env="} {
		if strings.Contains(lower, forbidden) {
			return false
		}
	}
	return !strings.ContainsRune(value, '\x00')
}

func hashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
