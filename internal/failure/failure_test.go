package failure

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestCanonicalFailureSites(t *testing.T) {
	want := []Site{
		SiteRunner, SiteWorkspace, SiteArtifact, SitePlanner, SiteGit,
		SiteVerification, SiteCache, SiteStore, SiteFinalization,
		SiteLifecycle, SiteCapability,
	}

	got := CanonicalSites()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical sites = %v, want %v", got, want)
	}
	got[0] = SiteOther
	if again := CanonicalSites(); !reflect.DeepEqual(again, want) {
		t.Fatalf("canonical sites after caller mutation = %v, want %v", again, want)
	}
}

func TestFailureFingerprintIsDeterministicAndSortsKeyedInputs(t *testing.T) {
	inputs := FingerprintInputs{
		IssueID: "GH-63", Stage: "execute", FailureSite: SitePlanner,
		StageInputs: []InputIdentity{
			{Identity: "spec.md", SHA256: "spec-digest"},
			{Identity: "brainstorm.md", SHA256: "brainstorm-digest"},
		},
		WatchtowerIdentity:    "watchtower-v1",
		ConfigurationIdentity: "config-digest",
		Git:                   GitIdentity{Repository: "repo", Base: "base", Branch: "branch", Tree: "tree"},
		Artifacts: []ContentIdentity{
			{Identity: "z-artifact", SHA256: "z-digest"},
			{Identity: "a-artifact", SHA256: "a-digest"},
		},
		Decisions:    []ContentIdentity{{Identity: "decision-2", SHA256: "d2"}, {Identity: "decision-1", SHA256: "d1"}},
		Verification: VerificationIdentity{Command: "command-digest", Cache: "cache-digest"},
	}

	first := BuildFingerprint(inputs)
	inputs.StageInputs[0], inputs.StageInputs[1] = inputs.StageInputs[1], inputs.StageInputs[0]
	inputs.Artifacts[0], inputs.Artifacts[1] = inputs.Artifacts[1], inputs.Artifacts[0]
	inputs.Decisions[0], inputs.Decisions[1] = inputs.Decisions[1], inputs.Decisions[0]
	second := BuildFingerprint(inputs)
	if first != second {
		t.Fatalf("sorted fingerprint changed: %q != %q", first, second)
	}
	if !strings.HasPrefix(first, "sha256:") || len(first) != len("sha256:")+64 {
		t.Fatalf("fingerprint = %q", first)
	}

	inputs.FailureSite = SiteGit
	if changed := BuildFingerprint(inputs); changed == first {
		t.Fatal("changing failure site did not change fingerprint")
	}
}

func TestFailureFingerprintExcludesAttemptAndRecordIdentity(t *testing.T) {
	inputs := FingerprintInputs{IssueID: "GH-63", Stage: "execute", FailureSite: SiteRunner}
	first := BuildFingerprint(inputs)
	second := BuildFingerprint(inputs)
	if first != second {
		t.Fatalf("same sanitized inputs changed: %q != %q", first, second)
	}
}

func TestFailureRecordJSONContainsOnlySafeMetadata(t *testing.T) {
	record := FailureRecord{
		RecordID: 7, SchemaVersion: 1, IssueID: "GH-63", Stage: "execute", StageAttempt: 2,
		FailureSite: SitePlanner, FailureClass: ClassUnavailable, RetryDisposition: RetryAfterStateChange,
		RequiredStateChange: StatePlannerInput,
		Fingerprint:         "sha256:" + strings.Repeat("a", 64),
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, secret := range []string{"/private/repo", "run secret command", "configuration body", "artifact bytes", "decision text", "normalized input", "primary failure"} {
		if strings.Contains(text, secret) {
			t.Fatalf("record leaked %q: %s", secret, text)
		}
	}
	for _, field := range []string{"record_id", "schema_version", "failure_site", "fingerprint", "occurred_at"} {
		if !strings.Contains(text, `"`+field+`"`) {
			t.Fatalf("record omitted %q: %s", field, text)
		}
	}
}

func TestFailureRecordInputRejectsUncontrolledValues(t *testing.T) {
	input := RecordInput{
		IssueID: "GH-63", FailureSite: SitePlanner, FailureClass: Class("free-form"),
		RetryDisposition: RetryNow, RequiredStateChange: StateNone,
		Fingerprint: "sha256:" + strings.Repeat("a", 64),
	}
	if err := ValidateRecordInput(input); err == nil {
		t.Fatal("uncontrolled failure class was accepted")
	}
}

func TestLifecycleFailureSiteIsStableAndSafe(t *testing.T) {
	if got := NormalizeSite(Site("lifecycle")); got != SiteLifecycle {
		t.Fatalf("lifecycle site = %q, want %q", got, SiteLifecycle)
	}
	if got := NormalizeSite(Site("private-path")); got != SiteOther {
		t.Fatalf("unknown site = %q, want %q", got, SiteOther)
	}
}

func TestStateVectorRequiresStableDigestsOrUnavailable(t *testing.T) {
	valid := StateVector{
		TreeDigest:        "sha256:" + strings.Repeat("a", 64),
		ConfigDigest:      "sha256:" + strings.Repeat("b", 64),
		EnvironmentDigest: "sha256:" + strings.Repeat("c", 64),
		DecisionDigest:    Unavailable,
	}
	if err := ValidateStateVector(valid); err != nil {
		t.Fatalf("valid state vector rejected: %v", err)
	}

	invalid := valid
	invalid.EnvironmentDigest = "SECRET_API_TOKEN=value"
	if err := ValidateStateVector(invalid); err == nil {
		t.Fatal("raw environment value was accepted as a digest")
	}

	input := RecordInput{
		IssueID: "GH-65", Stage: "execute", StageAttempt: 1,
		FailureSite: SiteVerification, FailureClass: ClassValidation,
		RetryDisposition: RetryAfterStateChange, RequiredStateChange: StateVerification,
		Fingerprint: "sha256:" + strings.Repeat("d", 64), StateVector: &invalid,
	}
	if err := ValidateRecordInput(input); err == nil {
		t.Fatal("record input with invalid state evidence was accepted")
	}
}

func TestStateVectorJSONContainsOnlyDigestEvidence(t *testing.T) {
	vector := StateVector{
		TreeDigest:        "sha256:" + strings.Repeat("a", 64),
		ConfigDigest:      "sha256:" + strings.Repeat("b", 64),
		EnvironmentDigest: "sha256:" + strings.Repeat("c", 64),
		DecisionDigest:    "sha256:" + strings.Repeat("d", 64),
	}
	encoded, err := json.Marshal(vector)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, field := range []string{"tree_digest", "config_digest", "environment_digest", "decision_digest"} {
		if !strings.Contains(text, `"`+field+`"`) {
			t.Fatalf("state vector omitted %q: %s", field, text)
		}
	}
	for _, raw := range []string{"SECRET_API_TOKEN", "model sample", "exception text"} {
		if strings.Contains(text, raw) {
			t.Fatalf("state vector leaked %q: %s", raw, text)
		}
	}
}

func TestFailureFingerprintIncludesSanitizedEnvironmentIdentity(t *testing.T) {
	inputs := FingerprintInputs{
		IssueID: "GH-65", Stage: "execute", FailureSite: SiteVerification,
		EnvironmentIdentity: "sha256:" + strings.Repeat("a", 64),
	}
	first := BuildFingerprint(inputs)
	inputs.EnvironmentIdentity = "sha256:" + strings.Repeat("b", 64)
	if second := BuildFingerprint(inputs); second == first {
		t.Fatal("changing the sanitized environment identity did not change the fingerprint")
	}
}
